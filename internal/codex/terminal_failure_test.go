package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeTerminalFailureMetadata(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		err       error
		provider  string
		operation string
		code      string
		sizes     bool
	}{
		{name: "wrapped RPC data", err: fmt.Errorf("private-prompt: %w", &ResponseError{Request: "turn/start", Code: -32602, Message: "private-prompt", Body: `{"error":{"data":{"code":"input_too_large","max_chars":1048576,"actual_chars":2927066,"prompt":"private-prompt","token":"private-secret"}}}`}), provider: "codex", operation: "turn/start", code: "input_too_large", sizes: true},
		{name: "structured RPC message", err: &ResponseError{Request: "turn/start", Code: -32602, Message: `{"error":"input_too_large","max_chars":1048576,"actual_chars":2927066,"prompt":"private-prompt"}`}, provider: "codex", operation: "turn/start", code: "input_too_large", sizes: true},
		{name: "missing metadata", err: &ResponseError{Request: "turn/start", Code: -32602, Message: "private-prompt", Body: `{"error":{"data":{"prompt":"private-prompt"}}}`}, provider: "codex", operation: "turn/start"},
		{name: "malformed counts", err: &ResponseError{Request: "turn/start", Code: -32602, Body: `{"error":{"data":{"code":"input_too_large","max_chars":"private-secret","actual_chars":2.5}}}`}, provider: "codex", operation: "turn/start"},
		{name: "negative counts and private codes", err: &ResponseError{Request: "/private/source", Code: -32602, Body: `{"error":{"data":{"code":"private-secret","max_chars":-1,"actual_chars":-2}}}`}, provider: "codex"},
		{name: "joined failed turn", err: errors.Join(&TurnFailedError{Message: "private-prompt", Body: "private-secret"}, errors.New("/private/source")), provider: "codex", operation: "turn/completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var provider interface {
				NativeTerminalFailure() tracker.NativeTerminalFailure
			}
			if !errors.As(test.err, &provider) {
				t.Fatal("missing provider evidence")
			}
			got := provider.NativeTerminalFailure()
			if got.Provider != test.provider || got.Operation != test.operation || got.ProviderCode != test.code {
				t.Fatalf("recorded failure=%+v", got)
			}
			if test.sizes {
				if got.MaxChars == nil || *got.MaxChars != 1048576 || got.ActualChars == nil || *got.ActualChars != 2927066 {
					t.Fatalf("recorded request sizes=%+v", got)
				}
			} else if got.MaxChars != nil || got.ActualChars != nil || !slices.Contains(got.Unavailable, "max_chars") || !slices.Contains(got.Unavailable, "actual_chars") {
				t.Fatalf("invented request sizes=%+v", got)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private-prompt", "/private/source", "private-secret"} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("terminal evidence leaked %q", private)
				}
			}
		})
	}
}
