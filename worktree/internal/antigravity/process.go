package antigravity

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
	defer func() {
		if details, ok := b.ClassifyCapacityError(turnErr, nil, time.Time{}); ok {
			turnErr = backendcapacity.NewError(backendcapacity.Scope{BackendID: b.options.BackendID, BackendKind: BackendKind, Provider: b.provider(req)}, details, turnErr)
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
		return result, &infrastructureError{err: errors.New("antigravity command factory returned nil"), startup: true}
	}
	cmd.Dir = req.Workspace
	procgroup.SetEnvironment(cmd, req.Environment)
	procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{"HOME": b.options.Home}})
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inputReader, outputWriter, stderrWriter
	procgroup.Configure(ctx, cmd)
	if err := procgroup.Start(cmd); err != nil {
		return result, &infrastructureError{err: fmt.Errorf("start Antigravity: %w", err), startup: true}
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
	writeDone := make(chan error, 1)
	writeStarted := false
	defer func() {
		var waitErr error
		waited := false
		if turnErr == nil {
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
			turnErr = errors.Join(turnErr, &infrastructureError{err: fmt.Errorf("antigravity process exit: %w", waitErr)})
		}
		cancel()
		closePipe(input, &turnErr)
		if writeStarted && !state.promptWritten {
			if err := <-writeDone; err != nil && turnErr == nil {
				turnErr = err
			}
		}
		if errors.Is(waitErr, runner.ErrAgentTurnCleanup) {
			closePipe(stderrReader, &turnErr)
			closePipe(output, &turnErr)
		}
		turnErr = errors.Join(turnErr, <-stderrDone)
		<-readerDone
		if turnErr != nil && stderr.String() != "" {
			turnErr = fmt.Errorf("%w; Antigravity stderr: %s", turnErr, stderr.String())
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
	if err := emit(runner.AgentUpdate{Type: runner.AgentUpdateProcessStarted, ProcessIdentity: "antigravity-" + strconv.Itoa(cmd.Process.Pid), WorkerProcess: identity}); err != nil {
		return result, err
	}

	writeStarted = true
	go func() { writeDone <- writePrompt(input, req) }()
	state = &turnState{req: req, emit: emit, provider: b.provider(req), effort: b.effort(req)}
	err = b.exchange(ctx, records, writeDone, state)
	result = state.result()
	if err != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return result, err
}

func writePrompt(input *os.File, req runner.AgentTurnRequest) error {
	prompt := req.Prompt
	if req.ToolInstructions != "" {
		prompt += "\n\n" + req.ToolInstructions
	}
	record := map[string]any{"event": "user", "message": map[string]string{"role": "user", "content": prompt}}
	if err := json.NewEncoder(input).Encode(record); err != nil {
		return &infrastructureError{err: fmt.Errorf("write Antigravity prompt: %w", err), startup: true}
	}
	if err := input.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return &infrastructureError{err: fmt.Errorf("close Antigravity prompt: %w", err), startup: true}
	}
	return nil
}

func closePipe(file *os.File, result *error) {
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		*result = errors.Join(*result, err)
	}
}

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
