package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func (b *AgentBackend) RunTurn(
	ctx context.Context,
	req runner.AgentTurnRequest,
	onUpdate runner.AgentUpdateHandler,
) (turnResult runner.AgentTurnResult, turnErr error) {
	ctx = contextOrBackground(ctx)
	turnTimeout := b.options.TurnTimeout
	if req.TurnTimeout > 0 {
		turnTimeout = req.TurnTimeout
	}
	if turnTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, turnTimeout)
		defer cancel()
	}

	cmd, sandboxSettings, err := b.commandWithIsolation(ctx, req)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	procgroup.SetEnvironment(cmd, req.Environment)
	procgroup.SetTempDir(cmd, req.TempDir)

	stderr := newTailBuffer(b.options.StderrTailBytes)
	var sandboxInput io.WriteCloser
	managedSandbox := sandboxSettings != nil
	if managedSandbox {
		sandboxInput, err = cmd.StdinPipe()
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		defer func() {
			if sandboxInput != nil {
				if err := sandboxInput.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					turnErr = errors.Join(turnErr, err)
				}
			}
		}()
		procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{"CLAUDE_CODE_TMPDIR": req.TempDir}})
	} else {
		cmd.Stdin = strings.NewReader(req.Prompt)
	}

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return runner.AgentTurnResult{}, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		return runner.AgentTurnResult{}, errors.Join(
			fmt.Errorf("create stderr pipe: %w", err),
			stdout.Close(),
			stdoutWriter.Close(),
		)
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	procgroup.Configure(ctx, cmd)
	if err := cmd.Start(); err != nil {
		return runner.AgentTurnResult{}, errors.Join(
			fmt.Errorf("start claude command: %w", err),
			stdout.Close(),
			stdoutWriter.Close(),
			stderrReader.Close(),
			stderrWriter.Close(),
		)
	}
	if err := procgroup.Deprioritize(cmd); err != nil {
		processGroupID := procgroup.GroupID(cmd)
		err = terminateWithCause(cmd, processGroupID, fmt.Errorf("deprioritize claude worker process: %w", err))
		return runner.AgentTurnResult{}, errors.Join(err, waitAndCleanup(cmd, processGroupID), stdout.Close(), stdoutWriter.Close(), stderrReader.Close(), stderrWriter.Close())
	}
	if err := errors.Join(stdoutWriter.Close(), stderrWriter.Close()); err != nil {
		processGroupID := procgroup.GroupID(cmd)
		err = terminateWithCause(cmd, processGroupID, fmt.Errorf("close parent output writers: %w", err))
		return runner.AgentTurnResult{}, errors.Join(err, waitAndCleanup(cmd, processGroupID), stdout.Close(), stderrReader.Close())
	}
	stderrDone := make(chan error)
	go func() {
		_, err := io.Copy(stderr, stderrReader)
		stderrDone <- err
	}()
	processGroupID := procgroup.GroupID(cmd)
	workerProcess, err := procgroup.Inspect(cmd)
	if err != nil {
		err = terminateWithCause(cmd, processGroupID, fmt.Errorf("inspect claude worker process: %w", err))
		if waitErr := waitAndCleanup(cmd, processGroupID); waitErr != nil {
			err = errors.Join(err, waitErr)
		}
		if stderrErr := <-stderrDone; stderrErr != nil {
			err = errors.Join(err, fmt.Errorf("read claude stderr: %w", stderrErr))
		}
		return runner.AgentTurnResult{}, errors.Join(err, stdout.Close(), stderrReader.Close())
	}

	processIdentity := "claude-" + strconv.Itoa(cmd.Process.Pid)
	if err := emitUpdate(onUpdate, runner.AgentUpdate{
		Type:            runner.AgentUpdateProcessStarted,
		ProcessIdentity: processIdentity,
		WorkerProcess:   workerProcess,
	}); err != nil {
		err = terminateWithCause(cmd, processGroupID, err)
		if waitErr := waitAndCleanup(cmd, processGroupID); waitErr != nil {
			err = errors.Join(err, fmt.Errorf("wait after terminating claude command: %w", waitErr))
		}
		if stderrErr := <-stderrDone; stderrErr != nil {
			err = errors.Join(err, fmt.Errorf("read claude stderr: %w", stderrErr))
		}
		return runner.AgentTurnResult{}, errors.Join(err, stdout.Close(), stderrReader.Close())
	}

	waitDone := make(chan error)
	go func() {
		waitDone <- waitAndCleanup(cmd, processGroupID)
	}()
	var streamOutput io.Reader = stdout
	if sandboxInput != nil {
		reader, verifyErr := verifySandboxProcess(ctx, sandboxInput, stdout, sandboxSettings)
		if verifyErr == nil {
			verifyErr = json.NewEncoder(sandboxInput).Encode(map[string]any{"type": "user", "session_id": "", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": req.Prompt}})
		}
		if err := sandboxInput.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			verifyErr = errors.Join(verifyErr, err)
		}
		sandboxInput = nil
		if verifyErr != nil {
			verifyErr = terminateWithCause(cmd, processGroupID, verifyErr)
			return runner.AgentTurnResult{}, errors.Join(verifyErr, <-waitDone, <-stderrDone, stdout.Close(), stderrReader.Close())
		}
		streamOutput = reader
	}
	state, streamErr := b.consumeStream(ctx, cmd, processGroupID, streamOutput, onUpdate)
	waitErr := <-waitDone
	if stderrErr := <-stderrDone; stderrErr != nil {
		waitErr = errors.Join(waitErr, fmt.Errorf("read claude stderr: %w", stderrErr))
	}
	closeErr := errors.Join(stdout.Close(), stderrReader.Close())

	result := runner.AgentTurnResult{
		ReportedCostMicros: state.reportedCostMicros,
		ThreadID:           state.sessionID,
		TurnID:             state.sessionID,
		SessionID:          state.sessionID,
	}

	if result.ReportedCostMicros != nil {
		result.CostSource = "backend_result"
	}
	if streamErr != nil {
		return result, errors.Join(streamErr, closeErr)
	}

	finalErr := finalTurnError(ctx, state, waitErr, stderr.String())
	if finalErr != nil && strings.EqualFold(strings.TrimSpace(state.resultSubtype), "error_max_turns") {
		finalErr = errors.Join(runner.ErrSessionTurnLimitExceeded, finalErr)
	}
	status := runner.FinalStateCompleted
	if finalErr != nil {
		status = runner.FinalStateFailed
	}

	if err := emitUpdate(onUpdate, runner.AgentUpdate{
		Type:                runner.AgentUpdateTurnCompleted,
		ThreadID:            state.sessionID,
		TurnID:              state.sessionID,
		Status:              status,
		Model:               state.model,
		RuntimeIdentity:     agentidentity.RuntimeUpdate(state.model, "", "", "", time.Time{}),
		BackendErrorMessage: state.resultText,
	}); err != nil {
		return result, errors.Join(err, closeErr)
	}

	return result, errors.Join(finalErr, closeErr)
}

func (b *AgentBackend) command(ctx context.Context, req runner.AgentTurnRequest) (*exec.Cmd, error) {
	cmd, _, err := b.commandWithIsolation(ctx, req)
	return cmd, err
}

func (b *AgentBackend) commandWithIsolation(ctx context.Context, req runner.AgentTurnRequest) (*exec.Cmd, map[string]any, error) {
	var verificationSettings map[string]any
	argv := b.argv(req)
	if b.options.IsolationPolicy != nil {
		policy, pinned := isolation.FromContext(ctx)
		var err error
		if !pinned {
			policy, err = b.options.IsolationPolicy()
		}
		if err != nil {
			return nil, nil, err
		}
		policy.AllowLocalBinding = req.AllowLocalBinding && !req.ReadOnly
		if !req.ReadOnly {
			policy.ExtraNetworkDomains = req.ExtraNetworkDomains
		}
		policy.WritableRoots = []string{req.Workspace}
		if req.TempDir != "" {
			policy.WritableRoots = append(policy.WritableRoots, req.TempDir)
		}
		policy.WritableRoots = append(policy.WritableRoots, req.ExtraWritableRoots...)
		if policy.Tier == isolation.Sandbox && !req.ReadOnly {
			paths, err := toolcache.Resolve(ctx)
			if err == nil {
				for _, path := range []string{paths.Build, paths.Modules} {
					if path != "" && path != "off" {
						policy.WritableRoots = append(policy.WritableRoots, path)
					}
				}
			}
		}
		settings, err := IsolationSettings(policy)
		if err != nil {
			return nil, nil, err
		}
		if policy.Tier == "sandbox" && len(b.options.ExtraArgs) > 0 {
			return nil, nil, errors.New("runner isolation cannot apply Claude extra arguments")
		}
		if policy.Tier == "sandbox" {
			verificationSettings = settings
			bounded := *b
			bounded.options.PermissionMode = "acceptEdits"
			argv = bounded.argv(req)
			argv = append(argv, "--input-format", "stream-json", "--tools", "Bash,Read,Glob,Grep", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`)
		}
		encoded, err := json.Marshal(settings)
		if err != nil {
			return nil, nil, err
		}
		argv = append(argv, "--setting-sources", "", "--settings", string(encoded))
	}
	var cmd *exec.Cmd
	if b.options.CommandFactoryWithArgs != nil {
		cmd = b.options.CommandFactoryWithArgs(ctx, argv)
	} else {
		cmd = b.options.CommandFactory(ctx)
	}
	if cmd == nil {
		return nil, nil, ErrNilCommand
	}
	cmd.Dir = req.Workspace
	if len(cmd.Args) == 0 {
		cmd.Args = []string{cmd.Path}
	}
	if b.options.CommandFactoryWithArgs == nil {
		cmd.Args = append(cmd.Args, argv...)
	}
	return cmd, verificationSettings, nil
}

func (b *AgentBackend) argv(req runner.AgentTurnRequest) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if model := strings.TrimSpace(req.Model); model != "" {
		args = append(args, "--model", model)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = strings.TrimSpace(b.options.Effort)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if sessionID := strings.TrimSpace(req.Resume.SessionID); sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if req.ReadOnly {
		args = append(args, "--permission-mode", "plan")
	} else {
		if mode := strings.TrimSpace(b.options.PermissionMode); mode != "" {
			args = append(args, "--permission-mode", mode)
		}
		if tools := nonEmptyStrings(b.options.AllowedTools); len(tools) > 0 {
			args = append(args, "--allowedTools")
			args = append(args, tools...)
		}
		if tools := nonEmptyStrings(b.options.DisallowedTools); len(tools) > 0 {
			args = append(args, "--disallowedTools")
			args = append(args, tools...)
		}
	}
	if b.options.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if !req.ReadOnly {
		for _, root := range req.ExtraWritableRoots {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			args = append(args, "--add-dir", root)
		}
		args = append(args, b.options.ExtraArgs...)
	} else {
		args = append(args,
			"--safe-mode",
			"--strict-mcp-config",
			"--disable-slash-commands",
			"--no-chrome",
			"--tools", "Read", "Glob", "Grep",
		)
	}
	return args
}

func (b *AgentBackend) consumeStream(
	ctx context.Context,
	cmd *exec.Cmd,
	processGroupID int,
	stdout io.Reader,
	onUpdate runner.AgentUpdateHandler,
) (turnState, error) {
	items := scanClaudeStream(ctx, stdout, b.options.MaxScanTokenSize)
	state := turnState{}
	var streamErr error
	ctxDone := ctx.Done()
	stallTimer := newStallTimer(b.options.StallTimeout)
	stallC := stallTimerChannel(stallTimer)
	defer stopStallTimer(stallTimer)

	for items != nil {
		select {
		case <-ctxDone:
			streamErr = ctx.Err()
			streamErr = terminateWithCause(cmd, processGroupID, streamErr)
			ctxDone = nil
			stallC = nil
		case <-stallC:
			streamErr = fmt.Errorf("%w after %s", ErrStreamStalled, b.options.StallTimeout)
			streamErr = terminateWithCause(cmd, processGroupID, streamErr)
			stallC = nil
			ctxDone = nil
		case item, ok := <-items:
			if !ok {
				items = nil
				continue
			}
			if streamErr != nil {
				continue
			}
			if item.err != nil {
				streamErr = item.err
				streamErr = terminateWithCause(cmd, processGroupID, streamErr)
				ctxDone = nil
				stallC = nil
				continue
			}
			resetStallTimer(stallTimer, b.options.StallTimeout)
			if err := state.apply(item.event, b.options.IncludePartialMessages, onUpdate); err != nil {
				streamErr = err
				streamErr = terminateWithCause(cmd, processGroupID, streamErr)
				ctxDone = nil
				stallC = nil
			}
		}
	}

	return state, streamErr
}

func terminateWithCause(cmd *exec.Cmd, processGroupID int, cause error) error {
	if err := procgroup.TerminateTree(cmd, processGroupID); err != nil {
		return errors.Join(cause, fmt.Errorf("terminate claude process tree: %w", err))
	}
	return cause
}

func newStallTimer(timeout time.Duration) *time.Timer {
	if timeout <= 0 {
		return nil
	}
	return time.NewTimer(timeout)
}

func stallTimerChannel(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func resetStallTimer(timer *time.Timer, timeout time.Duration) {
	if timer == nil || timeout <= 0 {
		return
	}
	stopStallTimer(timer)
	timer.Reset(timeout)
}

func stopStallTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func waitAndCleanup(cmd *exec.Cmd, processGroupID int) error {
	return waitAndCleanupWith(cmd, processGroupID, procgroup.Cleanup)
}

func waitAndCleanupWith(cmd *exec.Cmd, processGroupID int, cleanup func(int) error) error {
	err := cmd.Wait()
	if cleanupErr := cleanup(processGroupID); cleanupErr != nil {
		err = errors.Join(err, fmt.Errorf("clean up claude process group %d after %s: %w", processGroupID, cmd.ProcessState, cleanupErr))
	}
	return err
}

func finalTurnError(ctx context.Context, state turnState, waitErr error, stderr string) error {
	if !state.sawResult {
		if err := ctx.Err(); err != nil {
			return err
		}
		if waitErr == nil {
			return withStderrTail(ErrMissingResult, stderr)
		}
		return withStderrTail(fmt.Errorf("%w: process exited: %w", ErrMissingResult, waitErr), stderr)
	}

	switch {
	case state.resultIsError || !strings.EqualFold(strings.TrimSpace(state.resultSubtype), "success"):
		subtype := strings.TrimSpace(state.resultSubtype)
		if subtype == "" {
			subtype = "unknown"
		}
		err := fmt.Errorf("%w: result subtype %q", ErrTurnFailed, subtype)
		if strings.TrimSpace(state.resultText) != "" {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(state.resultText))
		}
		return withStderrTail(err, stderr)
	case waitErr != nil:
		return withStderrTail(fmt.Errorf("%w: process exited: %w", ErrTurnFailed, waitErr), stderr)
	default:
		return nil
	}
}

func withStderrTail(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: stderr: %s", err, stderr)
}

func emitUpdate(onUpdate runner.AgentUpdateHandler, update runner.AgentUpdate) error {
	if onUpdate == nil {
		return nil
	}
	if err := onUpdate(update); err != nil {
		return fmt.Errorf("%w: %w", ErrUpdateRejected, err)
	}
	return nil
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
