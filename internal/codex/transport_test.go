package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

const (
	oversizedJSONRPCPayloadSize         = 4 * 1024 * 1024
	helperDrainPayloadSize              = 4 * 1024 * 1024
	transportTestDeadlockTimeout        = 30 * time.Second
	transportTestRecoveryTimeout        = 5 * time.Second
	helperFailureBeforeCloseReadySignal = "invalid frame written; waiting for stdin close"
	helperCloseBeforeFailureReadySignal = "waiting for stdin close before invalid frame"
	helperDrainWriteStartedSignal       = "drain write started"
	helperDrainWriteCompletedSignal     = "drain write completed"
)

func TestLocalTransportRoundTrip(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "roundtrip")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := transport.Close(closeCtx); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Close() error = %v", err)
		}
	})

	request := Message{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(`42`),
		Method:  "ping",
		Params:  json.RawMessage(`{"value":"hello"}`),
	}

	if err := transport.Send(context.Background(), request); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	response, err := transport.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}

	if response.JSONRPC != JSONRPCVersion {
		t.Fatalf("JSONRPC = %q, want %q", response.JSONRPC, JSONRPCVersion)
	}
	if string(response.ID) != "42" {
		t.Fatalf("ID = %s, want 42", response.ID)
	}
	assertJSONEqual(t, response.Result, json.RawMessage(`{"echoedMethod":"ping","ok":true}`))
}

func TestLocalTransportReceivesOversizedFrame(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "oversized-frame")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}
	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = transport.Close(closeCtx)
	})

	receiveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg, err := transport.Receive(receiveCtx)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if msg.Method != "item/agentMessage" {
		t.Fatalf("Method = %q, want item/agentMessage", msg.Method)
	}
	var params struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if len(params.Text) != oversizedJSONRPCPayloadSize {
		t.Fatalf("params text length = %d, want %d", len(params.Text), oversizedJSONRPCPayloadSize)
	}
}

func TestLocalTransportCloseDrainsAfterReadFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		mode               string
		receiveBeforeClose bool
		readySignal        string
	}{
		{
			name:               "failure reported before close",
			mode:               "invalid-frame-backpressure",
			receiveBeforeClose: true,
			readySignal:        helperFailureBeforeCloseReadySignal,
		},
		{
			name:        "failure arrives after close starts",
			mode:        "close-before-invalid-frame-backpressure",
			readySignal: helperCloseBeforeFailureReadySignal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
				return helperCommand(ctx, tt.mode)
			})
			if err != nil {
				t.Fatalf("NewLocalTransportFactory() error = %v", err)
			}
			transport, err := factory.NewTransport(context.Background())
			if err != nil {
				t.Fatalf("NewTransport() error = %v", err)
			}
			local, ok := transport.(*localTransport)
			if !ok {
				t.Fatalf("transport = %T, want *localTransport", transport)
			}
			t.Cleanup(func() {
				select {
				case <-local.done:
					return
				default:
				}
				if err := recoverLocalTransportTest(local); err != nil {
					t.Errorf("recover local transport: %v", err)
				}
			})

			waitForLocalTransportStderr(t, local, tt.readySignal)

			if tt.receiveBeforeClose {
				receiveCtx, cancelReceive := context.WithTimeout(context.Background(), transportTestDeadlockTimeout)
				defer cancelReceive()
				_, err = transport.Receive(receiveCtx)
				if !errors.Is(err, ErrInvalidFrame) {
					t.Fatalf("Receive() error = %v, want ErrInvalidFrame", err)
				}
			}

			closeCtx, cancelClose := context.WithTimeout(context.Background(), transportTestDeadlockTimeout)
			defer cancelClose()
			if err := transport.Close(closeCtx); err != nil {
				recoveryErr := recoverLocalTransportTest(local)
				t.Fatalf("Close() error = %v; recovery error = %v", err, recoveryErr)
			}
			assertLocalTransportClosed(t, local, tt.name)
			for _, signal := range []string{helperDrainWriteStartedSignal, helperDrainWriteCompletedSignal} {
				if !strings.Contains(local.stderrText(), signal) {
					t.Fatalf("helper stderr = %q, want signal %q", local.stderrText(), signal)
				}
			}
		})
	}
}

func TestLocalTransportReceiveHonorsContext(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "silent")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := transport.Close(closeCtx); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	receiveCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = transport.Receive(receiveCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Receive() error = %v, want context deadline exceeded", err)
	}
}

func TestLocalTransportEarlyExitIncludesStatusAndStderr(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "exit-stderr")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}
	server, err := NewAppServer(factory, WithReadTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	err = server.CheckHealth(context.Background())
	if err == nil {
		t.Fatal("CheckHealth() error = nil, want early exit diagnostics")
	}
	for _, want := range []string{"exit status 23", "broken launch environment"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("CheckHealth() error = %v, want containing %q", err, want)
		}
	}
}

func TestTailBufferKeepsBoundedSuffix(t *testing.T) {
	t.Parallel()

	buffer := newTailBuffer(5)
	if _, err := buffer.Write([]byte("abcdef")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := buffer.String(); got != "bcdef" {
		t.Fatalf("String() = %q, want %q", got, "bcdef")
	}
}

func TestLocalTransportFactoryAppliesWorkerTempDir(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "silent")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	tempDir := t.TempDir()
	toolBin := t.TempDir()
	ctx := withWorkerTempDir(context.Background(), tempDir)
	ctx = withWorkerEnvironment(ctx, procgroup.Environment{
		Variables:    map[string]string{"GOCACHE": "/shared/go-build", "GOMODCACHE": "/shared/go-mod"},
		PathPrefixes: []string{toolBin},
	})
	transport, err := factory.NewTransport(ctx)
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := transport.Close(closeCtx); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	local, ok := transport.(*localTransport)
	if !ok {
		t.Fatalf("transport = %T, want *localTransport", transport)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := environmentValue(local.cmd.Env, name); got != tempDir {
			t.Fatalf("%s = %q, want %q", name, got, tempDir)
		}
	}
	for name, want := range map[string]string{"GOCACHE": "/shared/go-build", "GOMODCACHE": "/shared/go-mod"} {
		if got := environmentValue(local.cmd.Env, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if got := environmentValue(local.cmd.Env, "PATH"); !strings.HasPrefix(got, toolBin+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want prefix %q", got, toolBin)
	}
}

func TestLocalTransportSendHonorsContextDuringBlockedWrite(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "block-send")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := transport.Close(closeCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close() error = %v", err)
		}
	})

	params, err := json.Marshal(strings.Repeat("x", oversizedJSONRPCPayloadSize))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	sendCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err = transport.Send(sendCtx, Message{
		ID:     json.RawMessage(`1`),
		Method: "large",
		Params: params,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send() error = %v, want context deadline exceeded", err)
	}
}

func TestLocalTransportCloseExitsAfterTurnErrorBackpressure(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "turn-error-backpressure")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}
	capturingFactory := &capturingLocalTransportFactory{factory: factory}
	server, err := NewAppServer(capturingFactory,
		WithReadTimeout(500*time.Millisecond),
		WithTurnTimeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = server.RunTurn(runCtx, RunTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "fail mid stream",
	}, waitForLocalTransportReceiveBackpressure(t, capturingFactory))
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("RunTurn() error = %v, want ErrTurnFailed", err)
	}
	assertLocalTransportClosed(t, capturingFactory.transport, "turn error backpressure")
}

func TestLocalTransportCloseDrainsAfterSuccessfulTurnBackpressure(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "turn-complete-backpressure")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}
	capturingFactory := &capturingLocalTransportFactory{factory: factory}
	server, err := NewAppServer(capturingFactory,
		WithReadTimeout(2*time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := server.RunTurn(runCtx, RunTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "complete then drain",
	}, waitForLocalTransportReceiveBackpressure(t, capturingFactory))
	if err != nil {
		assertLocalTransportClosed(t, capturingFactory.transport, "successful turn backpressure after RunTurn error")
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ThreadID != "thread-1" || result.TurnID != "turn-1" {
		t.Fatalf("RunTurn() result = %#v, want thread-1 turn-1", result)
	}
	assertLocalTransportClosed(t, capturingFactory.transport, "successful turn backpressure")
}

func TestLocalTransportPublishReceivedStopsDuringBackpressure(t *testing.T) {
	t.Parallel()

	transport := &localTransport{
		received: make(chan transportResult),
		readStop: make(chan struct{}),
	}

	published := make(chan bool, 1)
	go func() {
		published <- transport.publishReceived(transportResult{
			msg: Message{
				JSONRPC: JSONRPCVersion,
				Method:  "item/agentMessage/delta",
				Params:  json.RawMessage(`{"delta":"queued"}`),
			},
		})
	}()

	transport.stopReading()

	select {
	case got := <-published:
		if got {
			t.Fatal("publishReceived() = true, want false after read stop during backpressure")
		}
	case <-time.After(time.Second):
		t.Fatal("publishReceived() stayed blocked after read stop")
	}

	select {
	case result := <-transport.received:
		t.Fatalf("received unexpected result after read stop: %#v", result)
	default:
	}
}

func TestLocalTransportCloseUnblocksBlockedWriteAndPublish(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(strings.Repeat("x", oversizedJSONRPCPayloadSize))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "blocked-write-publish")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}
	local, ok := transport.(*localTransport)
	if !ok {
		t.Fatalf("transport = %T, want *localTransport", transport)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = transport.Close(closeCtx)
	})

	stdin := newCloseWaitsForWriteCloser(local.stdin)
	local.stdin = stdin
	local.codec.writer = bufio.NewWriter(stdin)

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- transport.Send(context.Background(), Message{
			ID:     json.RawMessage(`1`),
			Method: "large",
			Params: params,
		})
	}()

	select {
	case <-stdin.writeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("Send() did not start writing")
	}
	waitForReceivedBufferFull(t, local, "blocked write and publish")
	select {
	case err := <-sendDone:
		t.Fatalf("Send() returned before close with full receive buffer: %v", err)
	default:
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- transport.Close(closeCtx)
	}()

	select {
	case err := <-sendDone:
		if err == nil {
			t.Fatal("Send() error = nil, want blocked write interrupted by close")
		}
	case <-closeCtx.Done():
		local.stopReading()
		_ = procgroup.TerminateTree(local.cmd, local.processGroupID)
		select {
		case <-sendDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Send() could not be recovered after Close() blocked")
		}
		select {
		case <-closeDone:
		case <-time.After(5 * time.Second):
			t.Fatal("Close() could not be recovered after blocked write")
		}
		t.Fatalf("Send() stayed blocked after Close(): %v", closeCtx.Err())
	}

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-closeCtx.Done():
		t.Fatalf("Close() did not complete: %v", closeCtx.Err())
	}
	assertLocalTransportClosed(t, local, "blocked write and publish")
}

func TestLocalTransportSendWrapsCloseErrorAfterContextCancellation(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("close stdin failed")
	stdin := newBlockingWriteCloser(closeErr)
	transport := &localTransport{
		stdin:    stdin,
		codec:    NewCodec(nil, stdin),
		sendLock: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	transport.sendLock <- struct{}{}

	sendCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- transport.Send(sendCtx, Message{Method: "blocked"})
	}()

	select {
	case <-stdin.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("Send() did not start writing")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send() error = %v, want context canceled", err)
		}
		if !errors.Is(err, closeErr) {
			t.Fatalf("Send() error = %v, want close error in chain", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send() did not return after context cancellation")
	}
}

func TestLocalTransportCloseKillsUnresponsiveProcess(t *testing.T) {
	t.Parallel()

	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		return helperCommand(ctx, "ignore-close")
	})
	if err != nil {
		t.Fatalf("NewLocalTransportFactory() error = %v", err)
	}

	transport, err := factory.NewTransport(context.Background())
	if err != nil {
		t.Fatalf("NewTransport() error = %v", err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err = transport.Close(closeCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context deadline exceeded", err)
	}
}

func TestLocalTransportCloseWrapsKillErrorAfterContextCancellation(t *testing.T) {
	t.Parallel()

	cmd := helperCommand(context.Background(), "exit")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	killErr := cmd.Process.Kill()
	if killErr == nil {
		t.Fatal("Kill() error = nil, want post-exit process error")
	}

	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()

	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()

	transport := &localTransport{
		cmd:   cmd,
		stdin: noopWriteCloser{},
		done:  done,
	}

	err := transport.Close(closeCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context canceled", err)
	}
	if !errors.Is(err, killErr) {
		t.Fatalf("Close() error = %v, want kill error %v in chain", err, killErr)
	}
}

func TestLocalTransportFactoryRejectsInvalidCommandFactories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		newCommand CommandFactory
	}{
		{name: "nil factory", newCommand: nil},
		{name: "nil command", newCommand: func(context.Context) *exec.Cmd { return nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			factory, err := NewLocalTransportFactory(tt.newCommand)
			if tt.newCommand == nil {
				if err == nil {
					t.Fatal("NewLocalTransportFactory() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLocalTransportFactory() error = %v", err)
			}

			_, err = factory.NewTransport(context.Background())
			if err == nil {
				t.Fatal("NewTransport() error = nil, want error")
			}
		})
	}
}

func TestLocalTransportHelperProcess(t *testing.T) {
	if os.Getenv("DETENT_CODEX_TRANSPORT_HELPER") != "1" {
		return
	}

	mode := os.Getenv("DETENT_CODEX_TRANSPORT_MODE")

	switch mode {
	case "roundtrip":
		helperRoundTrip()
	case "oversized-frame":
		helperOversizedFrame()
	case "invalid-frame-backpressure":
		helperInvalidFrameBackpressure(false)
	case "close-before-invalid-frame-backpressure":
		helperInvalidFrameBackpressure(true)
	case "silent":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "block-send":
		time.Sleep(time.Hour)
	case "ignore-close":
		time.Sleep(time.Hour)
	case "exit":
		return
	case "exit-stderr":
		fmt.Fprint(os.Stderr, "broken launch environment")
		os.Exit(23)
	case "turn-error-backpressure":
		helperTurnBackpressure(json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed"}}`), true)
	case "turn-complete-backpressure":
		helperTurnBackpressure(json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`), false)
	case "blocked-write-publish":
		helperFloodOutput(NewCodec(os.Stdin, os.Stdout), false)
	default:
		os.Exit(2)
	}

	os.Exit(0)
}

func helperCommand(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalTransportHelperProcess$")
	cmd.Env = append(os.Environ(),
		"DETENT_CODEX_TRANSPORT_HELPER=1",
		"DETENT_CODEX_TRANSPORT_MODE="+mode,
	)
	return cmd
}

func environmentValue(environment []string, name string) string {
	value := ""
	for _, entry := range environment {
		key, candidate, ok := strings.Cut(entry, "=")
		if ok && key == name {
			value = candidate
		}
	}
	return value
}

func helperTurnBackpressure(completedParams json.RawMessage, blockAfterFlood bool) {
	codec := NewCodec(os.Stdin, os.Stdout)
	helperBackpressureRespond(codec, initializeRequestID, "initialize", json.RawMessage(`{"userAgent":"codex-cli/test"}`))
	helperBackpressureExpect(codec, 0, "initialized")
	helperBackpressureRespond(codec, threadStartRequestID, "thread/start", json.RawMessage(`{"thread":{"id":"thread-1"}}`))
	helperBackpressureRespond(codec, configReadRequestID, "config/read", json.RawMessage(`{"config":{"model":"gpt-5.6"}}`))
	helperBackpressureRespond(codec, turnStartRequestID, "turn/start", json.RawMessage(`{"turn":{"id":"turn-1"}}`))

	if err := codec.WriteMessage(Message{
		Method: "turn/completed",
		Params: completedParams,
	}); err != nil {
		helperBackpressureExit("write turn/completed", err)
	}
	helperFloodOutput(codec, blockAfterFlood)
}

func helperFloodOutput(codec *Codec, blockAfterFlood bool) {
	for i := range 1024 {
		msg := Message{
			JSONRPC: JSONRPCVersion,
			Method:  "item/agentMessage/delta",
			Params:  json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"still streaming"}`),
		}
		if err := codec.WriteMessage(msg); err != nil {
			helperBackpressureExit("write output flood", err)
		}
		if blockAfterFlood && i == 1023 {
			time.Sleep(time.Hour)
		}
	}
}

func helperOversizedFrame() {
	params, err := json.Marshal(map[string]string{
		"text": strings.Repeat("x", oversizedJSONRPCPayloadSize),
	})
	if err != nil {
		helperBackpressureExit("marshal oversized frame", err)
	}
	if err := NewCodec(os.Stdin, os.Stdout).WriteMessage(Message{
		Method: "item/agentMessage",
		Params: params,
	}); err != nil {
		helperBackpressureExit("write oversized frame", err)
	}
}

func helperInvalidFrameBackpressure(closeBeforeFailure bool) {
	if closeBeforeFailure {
		helperBackpressureSignal(helperCloseBeforeFailureReadySignal)
		helperWaitForStdinClose()
	}

	if _, err := fmt.Fprintln(os.Stdout, "{not-json}"); err != nil {
		helperBackpressureExit("write invalid frame", err)
	}

	if !closeBeforeFailure {
		helperBackpressureSignal(helperFailureBeforeCloseReadySignal)
		helperWaitForStdinClose()
	}

	helperBackpressureSignal(helperDrainWriteStartedSignal)
	if _, err := io.Copy(os.Stdout, strings.NewReader(strings.Repeat("x", helperDrainPayloadSize))); err != nil {
		helperBackpressureExit("write drain payload", err)
	}
	helperBackpressureSignal(helperDrainWriteCompletedSignal)
}

func helperWaitForStdinClose() {
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		helperBackpressureExit("wait for stdin close", err)
	}
}

func helperBackpressureSignal(signal string) {
	if _, err := fmt.Fprintln(os.Stderr, signal); err != nil {
		os.Exit(7)
	}
}

func helperBackpressureRespond(codec *Codec, id int, method string, result json.RawMessage) {
	helperBackpressureExpect(codec, id, method)
	if err := codec.WriteMessage(Message{
		ID:     json.RawMessage(strconv.Itoa(id)),
		Result: result,
	}); err != nil {
		helperBackpressureExit("write "+method+" response", err)
	}
}

func helperBackpressureExpect(codec *Codec, id int, method string) {
	msg, err := codec.ReadMessage()
	if err != nil {
		helperBackpressureExit("read "+method, err)
	}
	if msg.Method != method {
		helperBackpressureExit("read "+method, fmt.Errorf("method = %q", msg.Method))
	}
	if id == 0 && len(msg.ID) != 0 {
		helperBackpressureExit("read "+method, fmt.Errorf("id = %s, want notification", msg.ID))
	}
	if id != 0 && !requestIDMatches(msg.ID, id) {
		helperBackpressureExit("read "+method, fmt.Errorf("id = %s, want %d", msg.ID, id))
	}
}

func helperBackpressureExit(stage string, err error) {
	fmt.Fprintf(os.Stderr, "backpressure helper %s: %v\n", stage, err)
	os.Exit(7)
}

func helperRoundTrip() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), oversizedJSONRPCPayloadSize)

	if !scanner.Scan() {
		os.Exit(3)
	}

	var request Message
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		os.Exit(4)
	}

	result, err := json.Marshal(map[string]any{
		"echoedMethod": request.Method,
		"ok":           true,
	})
	if err != nil {
		os.Exit(5)
	}

	response := Message{
		JSONRPC: JSONRPCVersion,
		ID:      request.ID,
		Result:  result,
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		os.Exit(6)
	}
}

type blockingWriteCloser struct {
	closeErr     error
	writeStarted chan struct{}
	closed       chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
}

func newBlockingWriteCloser(closeErr error) *blockingWriteCloser {
	return &blockingWriteCloser{
		closeErr:     closeErr,
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.startOnce.Do(func() {
		close(w.writeStarted)
	})
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.closeOnce.Do(func() {
		close(w.closed)
	})
	return w.closeErr
}

type noopWriteCloser struct{}

func (noopWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (noopWriteCloser) Close() error {
	return nil
}

type closeWaitsForWriteCloser struct {
	writer       io.WriteCloser
	writeStarted chan struct{}
	writeDone    chan struct{}
	startOnce    sync.Once
	doneOnce     sync.Once
}

func newCloseWaitsForWriteCloser(writer io.WriteCloser) *closeWaitsForWriteCloser {
	return &closeWaitsForWriteCloser{
		writer:       writer,
		writeStarted: make(chan struct{}),
		writeDone:    make(chan struct{}),
	}
}

func (w *closeWaitsForWriteCloser) Write(p []byte) (int, error) {
	w.startOnce.Do(func() {
		close(w.writeStarted)
	})
	n, err := w.writer.Write(p)
	w.doneOnce.Do(func() {
		close(w.writeDone)
	})
	return n, err
}

func (w *closeWaitsForWriteCloser) Close() error {
	<-w.writeDone
	return w.writer.Close()
}

type capturingLocalTransportFactory struct {
	factory   *LocalTransportFactory
	transport *localTransport
}

func (f *capturingLocalTransportFactory) NewTransport(ctx context.Context) (Transport, error) {
	transport, err := f.factory.NewTransport(ctx)
	if err != nil {
		return nil, err
	}
	local, ok := transport.(*localTransport)
	if !ok {
		return nil, errors.New("transport is not local")
	}
	f.transport = local
	return transport, nil
}

func assertLocalTransportClosed(t *testing.T, transport *localTransport, scenario string) {
	t.Helper()

	if transport == nil {
		t.Fatalf("%s: captured transport is nil", scenario)
		return
	}
	assertChannelClosed(t, transport.readDone, scenario, "readLoop")
	assertChannelClosed(t, transport.done, scenario, "wait")
	if transport.cmd == nil {
		t.Fatalf("%s: command is nil, want reaped process", scenario)
		return
	}
	if transport.cmd.ProcessState == nil {
		t.Fatalf("%s: ProcessState is nil, want reaped process", scenario)
	}
}

func waitForLocalTransportStderr(t *testing.T, transport *localTransport, signal string) {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(transportTestDeadlockTimeout)
	defer timer.Stop()

	for !strings.Contains(transport.stderrText(), signal) {
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("helper stderr = %q, want signal %q", transport.stderrText(), signal)
		}
	}
}

func recoverLocalTransportTest(transport *localTransport) error {
	transport.stopReading()
	closeErr := transport.closeStdin()
	terminateErr := procgroup.TerminateTree(transport.cmd, transport.processGroupID)
	if errors.Is(terminateErr, os.ErrProcessDone) {
		terminateErr = nil
	}
	joinErr := waitForLocalTransportTestGoroutines(transport, transportTestRecoveryTimeout)
	return errors.Join(closeErr, terminateErr, joinErr)
}

func waitForLocalTransportTestGoroutines(transport *localTransport, timeout time.Duration) error {
	readDone := transport.readDone
	done := transport.done
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for readDone != nil || done != nil {
		select {
		case <-readDone:
			readDone = nil
		case <-done:
			done = nil
		case <-timer.C:
			return errors.New("timed out joining local transport goroutines")
		}
	}
	return nil
}

func waitForLocalTransportReceiveBackpressure(t *testing.T, factory *capturingLocalTransportFactory) UpdateHandler {
	t.Helper()

	return func(update Update) error {
		if update.Type != UpdateTurnCompleted {
			return nil
		}
		if factory.transport == nil {
			t.Fatal("captured transport is nil while waiting for receive backpressure")
		}
		waitForReceivedBufferFull(t, factory.transport, "turn completed")
		return nil
	}
}

func waitForReceivedBufferFull(t *testing.T, transport *localTransport, scenario string) {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	for len(transport.received) < cap(transport.received) {
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("%s: receive buffer did not fill", scenario)
		}
	}
}

func assertChannelClosed(t *testing.T, ch <-chan struct{}, scenario string, name string) {
	t.Helper()

	select {
	case <-ch:
	default:
		t.Fatalf("%s: %s still running after RunTurn returned", scenario, name)
	}
}
