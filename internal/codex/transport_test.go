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
	if os.Getenv("SYMPHONY_CODEX_TRANSPORT_HELPER") != "1" {
		return
	}

	mode := os.Getenv("SYMPHONY_CODEX_TRANSPORT_MODE")

	switch mode {
	case "roundtrip":
		helperRoundTrip()
	case "silent":
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "block-send":
		time.Sleep(time.Hour)
	case "ignore-close":
		time.Sleep(time.Hour)
	default:
		os.Exit(2)
	}

	os.Exit(0)
}

func helperCommand(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalTransportHelperProcess$")
	cmd.Env = append(os.Environ(),
		"SYMPHONY_CODEX_TRANSPORT_HELPER=1",
		"SYMPHONY_CODEX_TRANSPORT_MODE="+mode,
	)
	return cmd
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
