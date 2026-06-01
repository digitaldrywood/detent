package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCodecWriteMessageFramesJSONLine(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	codec := NewCodec(strings.NewReader(""), &out)

	msg := Message{
		JSONRPC: JSONRPCVersion,
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
		Params:  json.RawMessage(`{"client":"detent"}`),
	}

	if err := codec.WriteMessage(msg); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	got := out.String()
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("frame missing newline terminator: %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("frame contains extra newlines: %q", got)
	}

	var decoded Message
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &decoded); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	if decoded.JSONRPC != JSONRPCVersion {
		t.Fatalf("JSONRPC = %q, want %q", decoded.JSONRPC, JSONRPCVersion)
	}
	if string(decoded.ID) != "1" {
		t.Fatalf("ID = %s, want 1", decoded.ID)
	}
	if decoded.Method != "initialize" {
		t.Fatalf("Method = %q, want initialize", decoded.Method)
	}
	assertJSONEqual(t, decoded.Params, json.RawMessage(`{"client":"detent"}`))
}

func TestCodecReadMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want Message
	}{
		{
			name: "request",
			line: `{"jsonrpc":"2.0","id":7,"method":"thread/start","params":{"cwd":"/tmp/work"}}` + "\n",
			want: Message{
				JSONRPC: JSONRPCVersion,
				ID:      json.RawMessage(`7`),
				Method:  "thread/start",
				Params:  json.RawMessage(`{"cwd":"/tmp/work"}`),
			},
		},
		{
			name: "notification",
			line: `{"jsonrpc":"2.0","method":"turn/completed"}` + "\n",
			want: Message{
				JSONRPC: JSONRPCVersion,
				Method:  "turn/completed",
			},
		},
		{
			name: "response",
			line: `{"jsonrpc":"2.0","id":"abc","result":{"thread":{"id":"thread-1"}}}` + "\n",
			want: Message{
				JSONRPC: JSONRPCVersion,
				ID:      json.RawMessage(`"abc"`),
				Result:  json.RawMessage(`{"thread":{"id":"thread-1"}}`),
			},
		},
		{
			name: "codex lite without version",
			line: `{"method":"initialized","params":{}}` + "\n",
			want: Message{
				Method: "initialized",
				Params: json.RawMessage(`{}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			codec := NewCodec(strings.NewReader(tt.line), io.Discard)

			got, err := codec.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage() error = %v", err)
			}

			if got.JSONRPC != tt.want.JSONRPC {
				t.Fatalf("JSONRPC = %q, want %q", got.JSONRPC, tt.want.JSONRPC)
			}
			if string(got.ID) != string(tt.want.ID) {
				t.Fatalf("ID = %s, want %s", got.ID, tt.want.ID)
			}
			if got.Method != tt.want.Method {
				t.Fatalf("Method = %q, want %q", got.Method, tt.want.Method)
			}
			assertJSONEqual(t, got.Params, tt.want.Params)
			assertJSONEqual(t, got.Result, tt.want.Result)
		})
	}
}

func TestCodecReadMessageRejectsMalformedFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
	}{
		{name: "empty line", line: "\n"},
		{name: "invalid json", line: "{not-json}\n"},
		{name: "unsupported version", line: `{"jsonrpc":"1.0","method":"ping"}` + "\n"},
		{name: "missing message shape", line: `{"jsonrpc":"2.0","params":{}}` + "\n"},
		{name: "result and error", line: `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-1,"message":"bad"}}` + "\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			codec := NewCodec(strings.NewReader(tt.line), io.Discard)

			_, err := codec.ReadMessage()
			if !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("ReadMessage() error = %v, want ErrInvalidFrame", err)
			}
		})
	}
}

func TestCodecReadMessageReturnsEOF(t *testing.T) {
	t.Parallel()

	codec := NewCodec(strings.NewReader(""), io.Discard)

	_, err := codec.ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadMessage() error = %v, want EOF", err)
	}
}

func TestCodecReadsLargeFrame(t *testing.T) {
	t.Parallel()

	if MaxScanTokenSize < 1024*1024 {
		t.Fatalf("MaxScanTokenSize = %d, want at least 1 MiB", MaxScanTokenSize)
	}

	wantText := strings.Repeat("x", 1024*1024)
	frame := `{"jsonrpc":"2.0","method":"item/agentMessage","params":{"text":"` + wantText + `"}}` + "\n"
	codec := NewCodec(strings.NewReader(frame), io.Discard)

	got, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	var params struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.Text != wantText {
		t.Fatalf("params text length = %d, want %d", len(params.Text), len(wantText))
	}
}

func assertJSONEqual(t *testing.T, got json.RawMessage, want json.RawMessage) {
	t.Helper()

	if len(got) == 0 || len(want) == 0 {
		if len(got) != len(want) {
			t.Fatalf("JSON = %s, want %s", got, want)
		}
		return
	}

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("got JSON is invalid: %v", err)
	}

	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("want JSON is invalid: %v", err)
	}

	gotCanonical, err := json.Marshal(gotValue)
	if err != nil {
		t.Fatalf("marshal got JSON: %v", err)
	}
	wantCanonical, err := json.Marshal(wantValue)
	if err != nil {
		t.Fatalf("marshal want JSON: %v", err)
	}
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("JSON = %s, want %s", gotCanonical, wantCanonical)
	}
}
