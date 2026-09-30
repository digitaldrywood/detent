package piagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (b *AgentBackend) RunTurn(ctx context.Context, req runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (result runner.AgentTurnResult, turnErr error) {
	// Use the existing instance capacity outcome for RPC infrastructure failures,
	// including local providers, whose transport failures are also instance-owned.
	defer func() {
		if details, ok := b.ClassifyCapacityError(turnErr, nil, time.Time{}); ok {
			provider := b.options.Provider
			if req.ModelProvider != "" {
				provider = req.ModelProvider
			}
			turnErr = backendcapacity.NewError(backendcapacity.Scope{BackendID: b.options.BackendID, BackendKind: "pi_agent", Provider: provider}, details, turnErr)
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
		return result, &infrastructureError{err: errors.New("Pi command factory returned nil"), startup: true}
	}
	cmd.Dir = req.Workspace
	procgroup.SetEnvironment(cmd, req.Environment)
	procgroup.SetTempDir(cmd, req.TempDir)

	// Own all pipes: Wait must reap the parent even when descendants inherit
	// output descriptors. The wait goroutine then cleans its process group.
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inputReader, outputWriter, stderrWriter
	procgroup.Configure(ctx, cmd)
	if err := cmd.Start(); err != nil {
		return result, &infrastructureError{err: fmt.Errorf("start Pi RPC: %w", err), startup: true}
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
	stderr := &tailBuffer{}
	stderrDone := make(chan error, 1)
	go func() { _, err := io.Copy(stderr, stderrReader); stderrDone <- err }()
	// Always terminate and join on completion, protocol errors, callback errors
	// and cancellation. Pi RPC stays alive after a completed prompt.
	defer func() {
		var waitErr error
		waited := false
		if turnErr == nil {
			// EOF is Pi's supported orderly shutdown. Bound disposal with the
			// shared process termination grace, then reap stubborn descendants.
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
			turnErr = errors.Join(turnErr, &infrastructureError{err: fmt.Errorf("Pi RPC process exit: %w", waitErr)})
		}
		cancel()
		// Cleanup failures must not leave drains blocked on foreign descriptors.
		if errors.Is(waitErr, runner.ErrAgentTurnCleanup) {
			closePipe(stderrReader, &turnErr)
			closePipe(output, &turnErr)
		}
		turnErr = errors.Join(turnErr, <-stderrDone)
		<-readerDone
		if turnErr != nil && stderr.String() != "" {
			turnErr = fmt.Errorf("%w; Pi stderr: %s", turnErr, stderr.String())
		}
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
	if err := emit(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, ProcessIdentity: "pi-" + strconv.Itoa(cmd.Process.Pid), WorkerProcess: identity}); err != nil {
		return result, err
	}

	if req.ModelProvider == "" {
		req.ModelProvider = b.options.Provider
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

// Only the stderr drain writes this buffer, and the caller reads it after join.
type tailBuffer struct{ buf []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	const limit = 64 * 1024
	n := len(p)
	if len(p) >= limit {
		b.buf = append(b.buf[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if len(b.buf)+len(p) > limit {
		b.buf = b.buf[len(b.buf)+len(p)-limit:]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}
func (b *tailBuffer) String() string { return string(b.buf) }

// encode writes a complete LF-delimited JSON record.
func encode(w io.Writer, record any) error {
	if err := json.NewEncoder(w).Encode(record); err != nil {
		return &infrastructureError{err: fmt.Errorf("write Pi RPC: %w", err)}
	}
	return nil
}
