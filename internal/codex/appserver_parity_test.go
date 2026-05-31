package codex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAppServerRunTurnMatchesElixirTranscriptBytes(t *testing.T) {
	t.Parallel()

	received := readTranscriptFixture(t, "elixir_codex_transcript.in.jsonl")
	wantSent := readTranscriptFixture(t, "elixir_codex_transcript.out.jsonl")
	transport := newTranscriptTransport(received)
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	var updates []Update
	result, err := server.RunTurn(context.Background(), RunTurnRequest{
		Workspace:      "/tmp/symphony-transcript",
		Prompt:         "Ship issue #23",
		ApprovalPolicy: "never",
		ThreadSandbox:  "workspace-write",
		TurnSandboxPolicy: map[string]any{
			"type":          "workspaceWrite",
			"networkAccess": false,
		},
		Model: "gpt-5-codex",
	}, func(update Update) error {
		updates = append(updates, update)
		return nil
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if result.ThreadID != "thread-elixir-1" || result.TurnID != "turn-elixir-1" {
		t.Fatalf("RunTurn() result = %#v, want recorded thread and turn", result)
	}
	if len(updates) != 4 {
		t.Fatalf("updates = %d, want 4", len(updates))
	}
	if updates[0].Type != UpdateTurnStarted {
		t.Fatalf("updates[0] = %#v, want turn started", updates[0])
	}
	if updates[2].Tokens.InputTokens != 123 || updates[2].Tokens.OutputTokens != 45 || updates[2].Tokens.TotalTokens != 168 {
		t.Fatalf("token update = %#v, want recorded totals", updates[2].Tokens)
	}
	assertTranscriptBytes(t, transport.sent.Bytes(), wantSent)
}

type transcriptTransport struct {
	codec *Codec
	sent  bytes.Buffer
}

func newTranscriptTransport(received []byte) *transcriptTransport {
	transport := &transcriptTransport{}
	transport.codec = NewCodec(bytes.NewReader(received), &transport.sent)
	return transport
}

func (t *transcriptTransport) Send(_ context.Context, msg Message) error {
	return t.codec.WriteMessage(msg)
}

func (t *transcriptTransport) Receive(context.Context) (Message, error) {
	return t.codec.ReadMessage()
}

func (t *transcriptTransport) Close(context.Context) error {
	return nil
}

func readTranscriptFixture(t *testing.T, name string) []byte {
	t.Helper()

	path := filepath.Join("testdata", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func assertTranscriptBytes(t *testing.T, got []byte, want []byte) {
	t.Helper()

	if bytes.Equal(got, want) {
		return
	}
	t.Fatalf("sent transcript mismatch\nwant:\n%s\ngot:\n%s\nfirst mismatch: %s", want, got, firstTranscriptMismatch(got, want))
}

func firstTranscriptMismatch(got []byte, want []byte) string {
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(string(want), "\n")
	maxLines := len(wantLines)
	if len(gotLines) > maxLines {
		maxLines = len(gotLines)
	}

	for i := 0; i < maxLines; i++ {
		var gotLine string
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		var wantLine string
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if gotLine != wantLine {
			return "line " + strconv.Itoa(i+1)
		}
	}
	return "byte length"
}
