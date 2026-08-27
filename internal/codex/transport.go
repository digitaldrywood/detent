package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

const defaultStderrTailBytes = 64 * 1024

type Transport interface {
	Send(context.Context, Message) error
	Receive(context.Context) (Message, error)
	Close(context.Context) error
}

type TransportFactory interface {
	NewTransport(context.Context) (Transport, error)
}

type CommandFactory func(context.Context) *exec.Cmd

type LocalTransportFactory struct {
	newCommand CommandFactory
}

type localTransport struct {
	cmd            *exec.Cmd
	processGroupID int
	workerProcess  procgroup.Identity
	startedAt      time.Time
	readyAt        time.Time
	exitedAt       time.Time
	exitStatus     string
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	stderr         io.ReadCloser
	stderrTail     *tailBuffer
	codec          *Codec
	received       chan transportResult
	readStop       chan struct{}
	readDone       chan struct{}
	stderrDone     chan error
	done           chan struct{}
	sendLock       chan struct{}
	waitErr        error
	waitMu         sync.Mutex
	readStopOnce   sync.Once
	closeOnce      sync.Once
	closeErr       error
	readDrainErr   error
}

type transportResult struct {
	msg Message
	err error
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

type workerTempDirContextKey struct{}
type workerEnvironmentContextKey struct{}

func NewLocalTransportFactory(newCommand CommandFactory) (*LocalTransportFactory, error) {
	if newCommand == nil {
		return nil, errors.New("command factory is nil")
	}

	return &LocalTransportFactory{newCommand: newCommand}, nil
}

func (f *LocalTransportFactory) NewTransport(ctx context.Context) (Transport, error) {
	ctx = contextOrBackground(ctx)

	cmd := f.newCommand(ctx)
	if cmd == nil {
		return nil, errors.New("command factory returned nil command")
	}
	procgroup.SetEnvironment(cmd, workerEnvironment(ctx))
	procgroup.SetTempDir(cmd, workerTempDir(ctx))

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		closeErr := stdin.Close()
		if closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("create stdout pipe: %w", err),
				fmt.Errorf("close stdin pipe: %w", closeErr),
			)
		}
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("create stderr pipe: %w", err),
			stdin.Close(),
			stdout.Close(),
			stdoutWriter.Close(),
		)
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	procgroup.Configure(ctx, cmd)
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start command: %w", err),
			stdin.Close(),
			stdout.Close(),
			stdoutWriter.Close(),
			stderr.Close(),
			stderrWriter.Close(),
		)
	}
	processStartedAt := time.Now().UTC()
	if err := errors.Join(stdoutWriter.Close(), stderrWriter.Close()); err != nil {
		terminateErr := procgroup.TerminateTree(cmd, procgroup.GroupID(cmd))
		waitErr := cmd.Wait()
		cleanupErr := procgroup.Cleanup(procgroup.GroupID(cmd))
		return nil, errors.Join(
			fmt.Errorf("close parent output writers: %w", err),
			terminateErr,
			waitErr,
			cleanupErr,
			stdin.Close(),
			stdout.Close(),
			stderr.Close(),
		)
	}

	transport := &localTransport{
		cmd:            cmd,
		processGroupID: procgroup.GroupID(cmd),
		startedAt:      processStartedAt,
		stdin:          stdin,
		stdout:         stdout,
		stderr:         stderr,
		stderrTail:     newTailBuffer(defaultStderrTailBytes),
		codec:          NewCodec(stdout, stdin),
		received:       make(chan transportResult, 64),
		readStop:       make(chan struct{}),
		readDone:       make(chan struct{}),
		stderrDone:     make(chan error, 1),
		done:           make(chan struct{}),
		sendLock:       make(chan struct{}, 1),
	}
	transport.sendLock <- struct{}{}
	go transport.readLoop()
	go transport.readStderr()

	if err := procgroup.Deprioritize(cmd); err != nil {
		return nil, transport.abortStart(fmt.Errorf("deprioritize worker process: %w", err))
	}
	workerProcess, err := procgroup.Inspect(cmd)
	if err != nil {
		return nil, transport.abortStart(fmt.Errorf("inspect worker process: %w", err))
	}
	transport.workerProcess = workerProcess
	go transport.wait()

	return transport, nil
}

func withWorkerTempDir(ctx context.Context, path string) context.Context {
	ctx = contextOrBackground(ctx)
	path = strings.TrimSpace(path)
	if path == "" {
		return ctx
	}
	return context.WithValue(ctx, workerTempDirContextKey{}, path)
}

func workerTempDir(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	path, ok := ctx.Value(workerTempDirContextKey{}).(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(path)
}

func withWorkerEnvironment(ctx context.Context, environment procgroup.Environment) context.Context {
	ctx = contextOrBackground(ctx)
	if len(environment.Variables) == 0 && len(environment.PathPrefixes) == 0 {
		return ctx
	}
	return context.WithValue(ctx, workerEnvironmentContextKey{}, environment)
}

func workerEnvironment(ctx context.Context) procgroup.Environment {
	if ctx == nil {
		return procgroup.Environment{}
	}
	environment, ok := ctx.Value(workerEnvironmentContextKey{}).(procgroup.Environment)
	if !ok {
		return procgroup.Environment{}
	}
	return environment
}

func (t *localTransport) Send(ctx context.Context, msg Message) error {
	ctx = contextOrBackground(ctx)

	if err := t.acquireSend(ctx); err != nil {
		return err
	}
	defer t.releaseSend()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- t.codec.WriteMessage(msg)
	}()

	select {
	case err := <-writeDone:
		return err
	case <-ctx.Done():
		select {
		case err := <-writeDone:
			return err
		default:
		}

		closeErr := t.closeStdin()
		<-writeDone
		if closeErr != nil {
			return transportContextError(ctx.Err(), "close stdin", closeErr)
		}
		return ctx.Err()
	}
}

func (t *localTransport) Receive(ctx context.Context) (Message, error) {
	ctx = contextOrBackground(ctx)

	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case result, ok := <-t.received:
		if !ok {
			return Message{}, t.receiveError(ctx, io.EOF)
		}
		if result.err != nil {
			return Message{}, t.receiveError(ctx, result.err)
		}
		return result.msg, nil
	}
}

func (t *localTransport) Close(ctx context.Context) error {
	ctx = contextOrBackground(ctx)

	t.stopReading()
	closeErr := t.closeStdin()

	select {
	case <-t.done:
		waitErr := t.waitError()
		if waitErr != nil {
			waitErr = withStderrTail(waitErr, t.stderrText())
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("close stdin: %w", closeErr)
		}
		return errors.Join(closeErr, waitErr)
	case <-ctx.Done():
		var killErr error
		killErr = procgroup.TerminateTree(t.cmd, t.processGroupID)

		select {
		case <-t.done:
		case <-time.After(time.Second):
		}

		if killErr != nil {
			return transportContextError(ctx.Err(), "kill process", killErr)
		}
		return ctx.Err()
	}
}

func (t *localTransport) ProcessIdentity() string {
	if t.cmd == nil || t.cmd.Process == nil {
		return ""
	}
	return strconv.Itoa(t.cmd.Process.Pid)
}

func (t *localTransport) WorkerProcess() procgroup.Identity {
	return t.workerProcess
}

func (t *localTransport) MarkStartupReady(readyAt time.Time) {
	t.waitMu.Lock()
	defer t.waitMu.Unlock()
	if t.readyAt.IsZero() {
		t.readyAt = readyAt.UTC()
	}
}

func (t *localTransport) StartupProcessEvidence() backendcapacity.StartupProcessEvidence {
	t.waitMu.Lock()
	defer t.waitMu.Unlock()
	startedAt := t.startedAt
	if !t.workerProcess.StartedAt.IsZero() {
		startedAt = t.workerProcess.StartedAt.UTC()
	}
	evidence := backendcapacity.StartupProcessEvidence{
		StartedAt:    timePointer(startedAt),
		Ready:        !t.readyAt.IsZero(),
		ReadyAt:      timePointer(t.readyAt),
		ExitObserved: !t.exitedAt.IsZero(),
		ExitedAt:     timePointer(t.exitedAt),
		ExitStatus:   strings.TrimSpace(t.exitStatus),
	}
	if !startedAt.IsZero() && !t.readyAt.IsZero() {
		evidence.ReadyAfterMS = max(t.readyAt.Sub(startedAt).Milliseconds(), 0)
	}
	if !startedAt.IsZero() && !t.exitedAt.IsZero() {
		evidence.ExitAfterMS = max(t.exitedAt.Sub(startedAt).Milliseconds(), 0)
	}
	return evidence
}

func transportContextError(ctxErr error, operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ctxErr, operation, err)
}

func (t *localTransport) acquireSend(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return t.closedError()
	case <-t.sendLock:
		select {
		case <-t.done:
			t.releaseSend()
			return t.closedError()
		default:
			return nil
		}
	}
}

func (t *localTransport) releaseSend() {
	t.sendLock <- struct{}{}
}

func (t *localTransport) closeStdin() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.stdin.Close()
		if errors.Is(t.closeErr, os.ErrClosed) {
			t.closeErr = nil
		}
	})
	return t.closeErr
}

func (t *localTransport) closedError() error {
	select {
	case <-t.done:
		return t.processExitError(errors.New("transport closed"))
	default:
	}
	if err := t.waitError(); err != nil {
		return fmt.Errorf("transport closed: %w", err)
	}
	return errors.New("transport closed")
}

func (t *localTransport) readLoop() {
	defer close(t.readDone)
	defer close(t.received)

	for {
		msg, err := t.codec.ReadMessage()
		if err != nil {
			if !t.readingStopped() {
				t.publishReceived(transportResult{err: err})
			}
			if !errors.Is(err, io.EOF) {
				t.readDrainErr = t.codec.drain()
			}
			return
		}

		if t.readingStopped() {
			continue
		}
		t.publishReceived(transportResult{msg: msg})
	}
}

func (t *localTransport) readStderr() {
	_, err := io.Copy(t.stderrTail, t.stderr)
	t.stderrDone <- err
}

func (t *localTransport) wait() {
	err := t.cmd.Wait()
	if cleanupErr := procgroup.Cleanup(t.processGroupID); cleanupErr != nil {
		err = errors.Join(err, cleanupErr)
	}
	<-t.readDone
	if t.readDrainErr != nil {
		err = errors.Join(err, t.readDrainErr)
	}
	if stderrErr := <-t.stderrDone; stderrErr != nil {
		err = errors.Join(err, fmt.Errorf("read codex stderr: %w", stderrErr))
	}
	if closeErr := errors.Join(t.stdout.Close(), t.stderr.Close()); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	t.waitMu.Lock()
	t.waitErr = err
	t.exitedAt = time.Now().UTC()
	if t.cmd != nil && t.cmd.ProcessState != nil {
		t.exitStatus = t.cmd.ProcessState.String()
	}
	t.waitMu.Unlock()
	close(t.done)
}

func (t *localTransport) abortStart(cause error) error {
	t.stopReading()
	closeErr := t.closeStdin()
	terminateErr := procgroup.TerminateTree(t.cmd, t.processGroupID)
	waitErr := t.cmd.Wait()
	cleanupErr := procgroup.Cleanup(t.processGroupID)
	<-t.readDone
	stderrErr := <-t.stderrDone
	pipeCloseErr := errors.Join(t.stdout.Close(), t.stderr.Close())
	err := errors.Join(cause, closeErr, terminateErr, waitErr, cleanupErr, stderrErr, pipeCloseErr)
	return withStderrTail(err, t.stderrText())
}

func (t *localTransport) receiveError(ctx context.Context, cause error) error {
	if !errors.Is(cause, io.EOF) {
		return cause
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.done:
		return t.processExitError(cause)
	}
}

func (t *localTransport) processExitError(cause error) error {
	status := "unknown status"
	if t.cmd != nil && t.cmd.ProcessState != nil {
		status = t.cmd.ProcessState.String()
	}
	err := fmt.Errorf("%w: codex app-server process exited (%s)", cause, status)
	if waitErr := t.waitError(); waitErr != nil {
		err = fmt.Errorf("%w: wait error: %w", err, waitErr)
	}
	return withStderrTail(err, t.stderrText())
}

func (t *localTransport) stderrText() string {
	if t == nil || t.stderrTail == nil {
		return ""
	}
	return t.stderrTail.String()
}

func (t *localTransport) publishReceived(result transportResult) bool {
	if t.readStop == nil {
		t.received <- result
		return true
	}

	select {
	case <-t.readStop:
		return false
	default:
	}

	select {
	case t.received <- result:
		return true
	case <-t.readStop:
		return false
	}
}

func (t *localTransport) readingStopped() bool {
	if t.readStop == nil {
		return false
	}

	select {
	case <-t.readStop:
		return true
	default:
		return false
	}
}

func (t *localTransport) stopReading() {
	if t.readStop == nil {
		return
	}
	t.readStopOnce.Do(func() {
		close(t.readStop)
	})
}

func (t *localTransport) waitError() error {
	t.waitMu.Lock()
	defer t.waitMu.Unlock()
	return t.waitErr
}

func newTailBuffer(limit int) *tailBuffer {
	if limit <= 0 {
		limit = defaultStderrTailBytes
	}
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func withStderrTail(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: stderr: %s", err, stderr)
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
