package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
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

	params, err := json.Marshal(strings.Repeat("x", 2*MaxScanTokenSize))
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
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	started := time.Now()
	_, err = server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "fail mid stream",
	}, nil)
	elapsed := time.Since(started)
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("RunTurn() error = %v, want ErrTurnFailed", err)
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("RunTurn() took %s after turn error, want prompt close", elapsed)
	}

	transport := capturingFactory.transport
	if transport == nil {
		t.Fatal("captured transport is nil")
	}
	assertChannelClosed(t, transport.readDone, "readLoop")
	assertChannelClosed(t, transport.done, "wait")
	if transport.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil, want reaped process")
	}
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

	started := time.Now()
	result, err := server.RunTurn(context.Background(), RunTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "complete then drain",
	}, nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ThreadID != "thread-1" || result.TurnID != "turn-1" {
		t.Fatalf("RunTurn() result = %#v, want thread-1 turn-1", result)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("RunTurn() took %s after completed turn, want prompt close", elapsed)
	}

	transport := capturingFactory.transport
	if transport == nil {
		t.Fatal("captured transport is nil")
	}
	assertChannelClosed(t, transport.readDone, "readLoop")
	assertChannelClosed(t, transport.done, "wait")
	if transport.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil, want reaped process")
	}
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
	case "silent":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "block-send":
		time.Sleep(time.Hour)
	case "ignore-close":
		time.Sleep(time.Hour)
	case "exit":
		return
	case "turn-error-backpressure":
		helperTurnBackpressure(json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed"}}`), true)
	case "turn-complete-backpressure":
		helperTurnBackpressure(json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`), false)
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

func helperTurnBackpressure(completedParams json.RawMessage, blockAfterFlood bool) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)

	messages := []Message{
		{
			JSONRPC: JSONRPCVersion,
			ID:      json.RawMessage(`1`),
			Result:  json.RawMessage(`{"userAgent":"codex-cli/test"}`),
		},
		{
			JSONRPC: JSONRPCVersion,
			ID:      json.RawMessage(`2`),
			Result:  json.RawMessage(`{"thread":{"id":"thread-1"}}`),
		},
		{
			JSONRPC: JSONRPCVersion,
			ID:      json.RawMessage(`3`),
			Result:  json.RawMessage(`{"turn":{"id":"turn-1"}}`),
		},
		{
			JSONRPC: JSONRPCVersion,
			Method:  "turn/completed",
			Params:  completedParams,
		},
	}
	for _, msg := range messages {
		if err := encoder.Encode(msg); err != nil {
			os.Exit(7)
		}
	}
	for i := range 1024 {
		msg := Message{
			JSONRPC: JSONRPCVersion,
			Method:  "item/agentMessage/delta",
			Params:  json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"still streaming"}`),
		}
		if err := encoder.Encode(msg); err != nil {
			os.Exit(8)
		}
		if blockAfterFlood && i == 1023 {
			time.Sleep(time.Hour)
		}
	}
}

func helperRoundTrip() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxScanTokenSize)

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

func assertChannelClosed(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("%s did not exit", name)
	}
}
