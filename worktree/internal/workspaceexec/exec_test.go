package workspaceexec

import (
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The service is tested through Run against a real shell rather than by
// inspecting its parts: what it has to get right is the shape of a stream --
// span sizes, rune boundaries, the cap, the exit code and a process group that
// dies -- and none of that is visible from a unit call.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newService prepares a service over a fresh worktree and reports both.
func newService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	service, err := New(root, "", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return service, root
}

// writeWorktreeFile puts a payload in the worktree for a command to read back.
func writeWorktreeFile(t *testing.T, root, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// collector records the spans a run emitted, which is what a test asserts
// against: the session turns each of these into one output frame.
type collector struct {
	spans []workspacesession.ExecOutput
}

func (c *collector) emit(span workspacesession.ExecOutput) error {
	c.spans = append(c.spans, span)
	return nil
}

// bytes reassembles the forwarded output, decoding the spans that are not text
// and leaving out the relay's own truncation marker.
func (c *collector) bytes(t *testing.T) []byte {
	t.Helper()
	var out []byte
	for _, span := range c.spans {
		if span.Truncated {
			continue
		}
		if span.Encoding == "" {
			out = append(out, span.Data...)
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(span.Data)
		if err != nil {
			t.Fatalf("span was marked base64 and is not: %v", err)
		}
		out = append(out, decoded...)
	}
	return out
}

// markers reports the truncation spans, which must appear exactly once.
func (c *collector) markers() []workspacesession.ExecOutput {
	var out []workspacesession.ExecOutput
	for _, span := range c.spans {
		if span.Truncated {
			out = append(out, span)
		}
	}
	return out
}

func TestScrubEnvironmentKeepsWhatIsNotACredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "detent", key: "DETENT_HUB_TOKEN", want: true},
		{name: "detent workspace", key: "DETENT_WORKSPACE", want: true},
		{name: "openai", key: "OPENAI_API_KEY", want: true},
		{name: "anthropic", key: "ANTHROPIC_AUTH_TOKEN", want: true},
		{name: "codex", key: "CODEX_HOME", want: true},
		{name: "claude", key: "CLAUDE_CONFIG_DIR", want: true},
		{name: "suffix", key: "GEMINI_API_KEY", want: true},
		{name: "bare key", key: "API_KEY", want: true},
		{name: "lowercase suffix", key: "vendor_api_key", want: true},
		{name: "path", key: "PATH", want: false},
		{name: "home", key: "HOME", want: false},
		{name: "unrelated", key: "API_KEYS_DOCUMENTATION", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := credentialKey(test.key); got != test.want {
				t.Fatalf("credentialKey(%q) = %v, want %v", test.key, got, test.want)
			}
		})
	}
}

func TestScrubEnvironmentExportsTheWorktree(t *testing.T) {
	t.Parallel()

	scrubbed := scrubEnvironment([]string{"PATH=/usr/bin", "DETENT_WORKSPACE=/elsewhere", "OPENAI_API_KEY=x"}, "/tmp/worktree")
	want := []string{"PATH=/usr/bin", "DETENT_WORKSPACE=/tmp/worktree"}
	if len(scrubbed) != len(want) {
		t.Fatalf("environment = %v, want %v", scrubbed, want)
	}
	for index, entry := range want {
		if scrubbed[index] != entry {
			t.Fatalf("environment = %v, want %v", scrubbed, want)
		}
	}
}

func TestBoundedSpanAndRemainderHoldsBackAPartialRune(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     []byte
		wantSpan []byte
		wantHeld []byte
	}{
		{name: "empty"},
		{name: "ascii", data: []byte("ok\n"), wantSpan: []byte("ok\n")},
		{name: "whole rune", data: []byte("日"), wantSpan: []byte("日")},
		{
			name:     "split rune",
			data:     []byte("ok\xe6\x97"),
			wantSpan: []byte("ok"),
			wantHeld: []byte("\xe6\x97"),
		},
		{
			name:     "split lead byte",
			data:     []byte("ok\xf0"),
			wantSpan: []byte("ok"),
			wantHeld: []byte("\xf0"),
		},
		{name: "invalid lead byte", data: []byte("ok\xff"), wantSpan: []byte("ok\xff")},
		{
			name:     "continuation run",
			data:     []byte{0x80, 0x80, 0x80, 0x80, 0x80},
			wantSpan: []byte{0x80, 0x80, 0x80, 0x80, 0x80},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			span, held := boundedSpanAndRemainder(test.data)
			if string(span) != string(test.wantSpan) || string(held) != string(test.wantHeld) {
				t.Fatalf("split = %q, %q; want %q, %q", span, held, test.wantSpan, test.wantHeld)
			}
		})
	}
}

func TestOutputSpanEncodesOnlyWhatIsNotText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		span         []byte
		wantEncoding string
	}{
		{name: "text", span: []byte("building...\n")},
		{name: "unicode", span: []byte("日本語\n")},
		{name: "invalid utf8", span: []byte{0xff, 0xfe}, wantEncoding: "base64"},
		{name: "nul byte", span: []byte("a\x00b"), wantEncoding: "base64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := outputSpan(test.span)
			if got.Encoding != test.wantEncoding {
				t.Fatalf("encoding = %q, want %q", got.Encoding, test.wantEncoding)
			}
			if got.Encoding == "" && got.Data != string(test.span) {
				t.Fatalf("data = %q, want %q", got.Data, test.span)
			}
		})
	}
}

func TestRunRefusesACommandItCannotRun(t *testing.T) {
	t.Parallel()

	service, _ := newService(t)
	tests := []struct {
		name     string
		command  string
		emit     func(workspacesession.ExecOutput) error
		wantCode string
	}{
		{name: "empty", command: "   ", emit: (&collector{}).emit, wantCode: workspacesession.CodeInvalidFrame},
		{
			name:     "oversized",
			command:  strings.Repeat("x", workspacesession.MaxExecCommandBytes+1),
			emit:     (&collector{}).emit,
			wantCode: workspacesession.CodeTooLarge,
		},
		{name: "no sink", command: "printf ok", wantCode: workspacesession.CodeInvalidFrame},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := service.Run(t.Context(), test.command, test.emit)
			if got := ErrorCode(err); got != test.wantCode {
				t.Fatalf("ErrorCode(%v) = %q, want %q", err, got, test.wantCode)
			}
		})
	}
}

func TestErrorCodeIgnoresForeignErrors(t *testing.T) {
	t.Parallel()

	if got := ErrorCode(errors.New("something else")); got != "" {
		t.Fatalf("ErrorCode = %q, want the empty string for an error that is not ours", got)
	}
	if got := ErrorCode(nil); got != "" {
		t.Fatalf("ErrorCode(nil) = %q, want the empty string", got)
	}
}

func TestNewRefusesAWorktreeThatIsNotADirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, "file")
	writeWorktreeFile(t, root, "file", []byte("not a worktree"))
	if _, err := New(file, "", discardLogger()); err == nil {
		t.Fatal("a file is not a worktree and must be refused")
	}
	if _, err := New(filepath.Join(root, "missing"), "", discardLogger()); err == nil {
		t.Fatal("a worktree that is not there must be refused")
	}
}
