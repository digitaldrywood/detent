package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (b *AgentBackend) RunTurn(ctx context.Context, req runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (result runner.AgentTurnResult, turnErr error) {
	defer func() {
		if details, ok := b.ClassifyCapacityError(turnErr, nil, time.Time{}); ok {
			provider, _, _ := strings.Cut(req.Model, "/")
			turnErr = backendcapacity.NewError(backendcapacity.Scope{BackendID: b.options.BackendID, BackendKind: "opencode", Provider: provider}, details, turnErr)
		}
	}()
	var state *turnState
	defer func() {
		if state == nil {
			return
		}
		status := runner.FinalStateCompleted
		message := ""
		if turnErr != nil {
			status = runner.FinalStateFailed
			message = turnErr.Error()
			if errors.Is(turnErr, context.Canceled) {
				status = "cancelled"
			}
		}
		turnErr = errors.Join(turnErr, state.update(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, Status: status, BackendErrorMessage: message}))
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	args, err := b.argv(ctx, req)
	if err != nil {
		return result, err
	}
	timeout := b.options.TurnTimeout
	if req.TurnTimeout > 0 {
		timeout = req.TurnTimeout
	}
	if req.MaxDuration > 0 && (timeout <= 0 || req.MaxDuration < timeout) {
		timeout = req.MaxDuration
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := b.options.CommandFactory(ctx, args)
	if cmd == nil {
		return result, &infrastructureError{err: errors.New("opencode command factory returned nil"), startup: true}
	}
	cmd.Dir = req.Workspace
	procgroup.SetEnvironment(cmd, req.Environment)
	procgroup.SetTempDir(cmd, req.TempDir)

	inputReader, input, err := os.Pipe()
	if err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	defer closePipe(inputReader, &turnErr)
	defer closePipe(input, &turnErr)
	output, outputWriter, err := os.Pipe()
	if err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	defer closePipe(output, &turnErr)
	defer closePipe(outputWriter, &turnErr)
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	defer closePipe(stderrReader, &turnErr)
	defer closePipe(stderrWriter, &turnErr)
	inputClosed := make(chan error, 1)
	stopInputClose := context.AfterFunc(ctx, func() { inputClosed <- input.Close() })
	defer func() {
		if !stopInputClose() {
			if err := <-inputClosed; err != nil && !errors.Is(err, os.ErrClosed) {
				turnErr = errors.Join(turnErr, err)
			}
		}
	}()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inputReader, outputWriter, stderrWriter
	procgroup.Configure(ctx, cmd)
	if err := procgroup.Start(cmd); err != nil {
		return result, &infrastructureError{err: fmt.Errorf("start OpenCode RPC: %w", err), startup: true}
	}
	groupID := procgroup.GroupID(cmd)
	waitDone := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		waitDone <- errors.Join(waitErr, runner.NewAgentTurnCleanupError(procgroup.Cleanup(groupID)))
	}()
	records := make(chan scanResult, 16)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); scan(ctx, output, records) }()
	stderrDone := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stderrReader); stderrDone <- err }()
	defer func() {
		var waitErr error
		waited := false
		if turnErr == nil {
			if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				turnErr = errors.Join(turnErr, err)
			}
			timer := time.NewTimer(procgroup.DefaultTerminationGrace)
			select {
			case waitErr = <-waitDone:
				waited = true
			case <-ctx.Done():
				turnErr = errors.Join(turnErr, ctx.Err())
			case <-timer.C:
			}
			timer.Stop()
		}
		if !waited {
			turnErr = errors.Join(turnErr, runner.NewAgentTurnCleanupError(procgroup.TerminateTree(cmd, groupID)))
			cancel()
			waitErr = <-waitDone
			if errors.Is(waitErr, runner.ErrAgentTurnCleanup) {
				turnErr = errors.Join(turnErr, waitErr)
			}
		} else if waitErr != nil {
			turnErr = errors.Join(turnErr, &infrastructureError{err: fmt.Errorf("OpenCode ACP process exit: %w", waitErr)})
		}
		cancel()
		closePipe(stderrReader, &turnErr)
		closePipe(output, &turnErr)
		if err := <-stderrDone; err != nil && !errors.Is(err, os.ErrClosed) {
			turnErr = errors.Join(turnErr, err)
		}
		<-readerDone
	}()
	if err := errors.Join(inputReader.Close(), outputWriter.Close(), stderrWriter.Close()); err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	if err := procgroup.Deprioritize(cmd); err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		return result, &infrastructureError{err: err, startup: true}
	}
	emit := func(update runner.AgentUpdate) error {
		if onUpdate != nil {
			return onUpdate(update)
		}
		return nil
	}
	if err := emit(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, ProcessIdentity: "opencode-" + strconv.Itoa(cmd.Process.Pid), WorkerProcess: identity}); err != nil {
		return result, err
	}

	state = &turnState{req: req, emit: emit}
	err = b.exchange(ctx, input, records, state)
	result = state.result()
	if err != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return result, err
}

func closePipe(file *os.File, result *error) {
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		*result = errors.Join(*result, err)
	}
}

func encode(w io.Writer, record any) error {
	if err := json.NewEncoder(w).Encode(record); err != nil {
		return &infrastructureError{err: fmt.Errorf("write OpenCode RPC: %w", err)}
	}
	return nil
}
