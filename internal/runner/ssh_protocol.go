package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const SSHProtocolVersion = 2

// SSHPeer multiplexes worker callbacks and their replies over the authenticated
// SSH channel. Neither endpoint needs a listening socket or an on-disk request.
type SSHPeer struct {
	ctx      context.Context //nolint:containedctx // The peer owns the SSH channel lifetime across concurrent calls and callbacks.
	cancel   context.CancelFunc
	decoder  *json.Decoder
	encoder  *json.Encoder
	writeMu  sync.Mutex
	mu       sync.Mutex
	pending  map[uint64]chan sshMessage
	sequence atomic.Uint64
	handle   func(context.Context, string, []json.RawMessage) (any, error)
}

type sshMessage struct {
	ID        uint64
	Method    string            `json:",omitempty"`
	Arguments []json.RawMessage `json:",omitempty"`
	Result    json.RawMessage   `json:",omitempty"`
	Error     *sshError         `json:",omitempty"`
}

func NewSSHPeer(ctx context.Context, input io.Reader, output io.Writer, handle func(context.Context, string, []json.RawMessage) (any, error)) *SSHPeer {
	ctx, cancel := context.WithCancel(ctx)
	p := &SSHPeer{ctx: ctx, cancel: cancel, decoder: json.NewDecoder(input), encoder: json.NewEncoder(output), pending: make(map[uint64]chan sshMessage), handle: handle}
	go p.read()
	return p
}

func (p *SSHPeer) Context() context.Context { return p.ctx }
func (p *SSHPeer) Close()                   { p.cancel() }

func (p *SSHPeer) send(message sshMessage) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.ctx.Err() != nil {
		return io.ErrUnexpectedEOF
	}
	return p.encoder.Encode(message)
}

func (p *SSHPeer) Call(ctx context.Context, method string, result any, args ...any) error {
	message := sshMessage{ID: p.sequence.Add(1), Method: method}
	for _, arg := range args {
		data, err := json.Marshal(arg)
		if err != nil {
			return err
		}
		message.Arguments = append(message.Arguments, data)
	}
	reply := make(chan sshMessage, 1)
	p.mu.Lock()
	p.pending[message.ID] = reply
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, message.ID); p.mu.Unlock() }()
	if err := p.send(message); err != nil {
		return err
	}
	select {
	case message = <-reply:
		if message.Error != nil {
			return message.Error.err()
		}
		if result != nil {
			return json.Unmarshal(message.Result, result)
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-p.ctx.Done():
		return io.ErrUnexpectedEOF
	}
}

func (p *SSHPeer) read() {
	defer p.cancel()
	for {
		var message sshMessage
		if err := p.decoder.Decode(&message); err != nil {
			return
		}
		if message.Method == "" {
			p.mu.Lock()
			reply := p.pending[message.ID]
			p.mu.Unlock()
			if reply != nil {
				reply <- message
			}
			continue
		}
		// Callbacks may issue another call in the opposite direction. A separate
		// handler keeps the read loop available for that nested response.
		go func() {
			var result any
			var err error
			if p.handle == nil {
				err = fmt.Errorf("unsupported SSH method %q", message.Method)
			} else {
				result, err = p.handle(p.ctx, message.Method, message.Arguments)
			}
			response := sshMessage{ID: message.ID, Error: encodeSSHError(err)}
			if err == nil {
				response.Result, err = json.Marshal(result)
				response.Error = encodeSSHError(err)
			}
			if p.send(response) != nil {
				p.cancel()
			}
		}()
	}
}

// invokeSSHMethod is restricted to the caller's explicit method allowlist. It
// preserves each store/callback signature without a second persistence model.
func invokeSSHMethod(ctx context.Context, target any, method string, args []json.RawMessage) (any, error) {
	value := reflect.ValueOf(target)
	if !value.IsValid() {
		return nil, errors.New("SSH callback is unavailable")
	}
	if method != "" {
		value = value.MethodByName(method)
	}
	if !value.IsValid() || value.Kind() != reflect.Func {
		return nil, fmt.Errorf("SSH callback %q is unavailable", method)
	}
	if value.IsNil() {
		return nil, errors.New("SSH callback is unavailable")
	}
	typ := value.Type()
	inputs := make([]reflect.Value, 0, typ.NumIn())
	i := 0
	if typ.NumIn() > 0 && typ.In(0) == reflect.TypeFor[context.Context]() {
		inputs = append(inputs, reflect.ValueOf(ctx))
		i++
	}
	if len(args) != typ.NumIn()-i {
		return nil, errors.New("invalid SSH callback arguments")
	}
	for _, arg := range args {
		input := reflect.New(typ.In(i))
		if err := json.Unmarshal(arg, input.Interface()); err != nil {
			return nil, err
		}
		inputs = append(inputs, input.Elem())
		i++
	}
	outputs := value.Call(inputs)
	if len(outputs) > 0 && typ.Out(len(outputs)-1) == reflect.TypeFor[error]() {
		last := outputs[len(outputs)-1]
		if !last.IsNil() {
			callbackErr, ok := last.Interface().(error)
			if !ok {
				return nil, errors.New("invalid SSH callback error")
			}
			return nil, callbackErr
		}
		outputs = outputs[:len(outputs)-1]
	}
	if len(outputs) == 0 {
		return nil, nil //nolint:nilnil // A successful callback with no result is encoded as JSON null.
	}
	if len(outputs) != 1 {
		return nil, errors.New("invalid SSH callback result")
	}
	return outputs[0].Interface(), nil
}

// Errors keep their sentinel and structured identity across the transport so
// infrastructure and provider capacity remain attributed to the instance.
type sshError struct {
	Message  string
	Kind     string
	Data     json.RawMessage
	Children []*sshError
}

func sshErrorTypes() []error {
	return []error{&backendcapacity.Error{}, &WorkerGitHubTokenResolutionError{}, &WorkerGitHubBudgetMonitorError{}, &DeliverableCommandError{}, &DeliverableRecoveryError{}, &SessionBrakeError{}, &WorkspaceBranchHeldError{}, &IssueConfigurationError{}, &SessionTokenCeilingError{}, &SessionBudgetProjectionError{}, &SessionMemoryCeilingError{}, &AgentTurnCleanupError{}, &forgeavailability.Error{}, &connector.TrackerAvailabilityError{}, &githubconnector.StatusError{}, &githubconnector.RESTFanoutDeferralError{}, &workspace.LandRefusal{}, &workspace.ValidationError{}, &workspace.HookError{}, &workspace.CommandError{}, &os.PathError{}, &os.LinkError{}, &os.SyscallError{}}
}

func sshSentinels() []error {
	return []error{githubconnector.ErrRateLimited, githubconnector.ErrAuthenticationFailed, githubconnector.ErrUnexpectedStatus, context.Canceled, context.DeadlineExceeded, store.ErrNotFound, syscall.ENOSPC, os.ErrNotExist, os.ErrPermission,
		artifact.ErrInvalid, artifact.ErrIntegrity, artifact.ErrMissing, artifact.ErrStorage, artifact.ErrUnsupported,
		artifact.ErrConflict, artifact.ErrQuota, artifact.ErrExpired, artifact.ErrDenied, artifact.ErrAuthorization,
		ErrWorkspacePreparation, ErrWorkspaceBranchHeld, ErrAgentResumeUnsupported,
		ErrWorkerGitHubTokenResolution, ErrWorkerGitHubBudgetMonitor, ErrWorkerGitHubRESTReserved,
		ErrSessionTokenCeilingExceeded, ErrSessionBudgetProjectionExceeded, ErrSessionMemoryCeilingExceeded,
		ErrSessionDurationExceeded, ErrTurnDurationExceeded, ErrSessionTurnLimitExceeded,
		ErrOperatorStopped, ErrMergeRevoked, ErrLaneRevoked, ErrCIUnavailable, ErrModelPermitUnavailable,
		ErrMergeWorkerStartupTimeout, ErrMergeWorkerDurationExceeded, ErrAgentTurnCleanup, ErrWorkerProcessReap, ErrLandingNotReviewed,
		ErrDeliverableRecoveryExhausted, ErrSubscriptionAuthRequired, ErrExecutionAuthorityUnavailable, ErrNativeRecoveryRequired}
}

func encodeSSHError(err error) *sshError {
	if err == nil {
		return nil
	}
	wire := &sshError{Message: err.Error()}
	for _, sentinel := range sshSentinels() {
		if err == sentinel { //nolint:errorlint // Only exact sentinels use this encoding; wrappers retain their message and children below.
			wire.Kind = "sentinel"
			return wire
		}
	}
	for _, prototype := range sshErrorTypes() {
		if reflect.TypeOf(err) != reflect.TypeOf(prototype) {
			continue
		}
		wire.Kind = reflect.TypeOf(prototype).String()
		copy := reflect.New(reflect.TypeOf(err).Elem())
		copy.Elem().Set(reflect.ValueOf(err).Elem())
		if field := copy.Elem().FieldByName("Err"); field.IsValid() && field.CanSet() {
			field.SetZero()
		}
		data, marshalErr := json.Marshal(copy.Interface())
		if marshalErr != nil {
			// Retain the message and error chain when structured fields cannot
			// be encoded, rather than publishing an undecodable typed error.
			wire.Kind = ""
		} else {
			wire.Data = data
		}
		break
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range multi.Unwrap() {
			wire.Children = append(wire.Children, encodeSSHError(child))
		}
	} else if child := errors.Unwrap(err); child != nil {
		wire.Children = append(wire.Children, encodeSSHError(child))
	}
	return wire
}

func (wire *sshError) err() error {
	if wire == nil {
		return nil
	}
	if wire.Kind == "sentinel" {
		for _, sentinel := range sshSentinels() {
			if sentinel.Error() == wire.Message {
				return sentinel
			}
		}
	}
	var children []error
	for _, child := range wire.Children {
		children = append(children, child.err())
	}
	for _, prototype := range sshErrorTypes() {
		if reflect.TypeOf(prototype).String() != wire.Kind {
			continue
		}
		copy := reflect.New(reflect.TypeOf(prototype).Elem())
		if json.Unmarshal(wire.Data, copy.Interface()) != nil {
			break
		}
		if field := copy.Elem().FieldByName("Err"); field.IsValid() && field.CanSet() && len(children) > 0 {
			field.Set(reflect.ValueOf(errors.Join(children...)))
		}
		if decoded, ok := copy.Interface().(error); ok {
			return decoded
		}
	}
	return &sshWrappedError{message: wire.Message, children: children}
}

type sshWrappedError struct {
	message  string
	children []error
}

func (e *sshWrappedError) Error() string   { return e.message }
func (e *sshWrappedError) Unwrap() []error { return e.children }
