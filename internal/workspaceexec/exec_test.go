package workspaceexec

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The service is tested through Run against a real shell rather than by
// inspecting its parts: what it has to get right is the shape of a stream --
// span sizes, rune boundaries, the cap, the exit code and a process group that
// dies -- and none of that is visible from a unit call.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// requirePOSIXShell skips a test whose command is written in POSIX shell.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the command is POSIX shell syntax")
	}
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

func TestRunChunksOutputWithinTheFrameCap(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	payload := []byte(strings.Repeat("detent-action-output\n", 8000))
	writeWorktreeFile(t, root, "payload.txt", payload)

	spans := &collector{}
	result, err := service.Run(t.Context(), "cat payload.txt", spans.emit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 || result.Truncated {
		t.Fatalf("result = %+v, want a clean exit with no truncation", result)
	}
	if len(spans.spans) < 2 {
		t.Fatalf("output arrived in %d spans, want it chunked", len(spans.spans))
	}
	for index, span := range spans.spans {
		if size := len(span.Data); size > workspacesession.MaxExecOutputFrameBytes {
			t.Fatalf("span %d is %d bytes, want at most %d", index, size, workspacesession.MaxExecOutputFrameBytes)
		}
	}
	if got := spans.bytes(t); string(got) != string(payload) {
		t.Fatalf("forwarded %d bytes, want the command's %d", len(got), len(payload))
	}
}

// TestRunSplitsSpansOnRuneBoundaries is the reason a span is not simply the
// bytes a read returned: a multi-byte rune cut in half arrives as two spans
// neither of which is text, and a reader sees replacement glyphs where the
// command wrote a character.
func TestRunSplitsSpansOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	// A three-byte rune divides neither the frame cap nor the read size, so
	// some span boundary lands inside one.
	payload := []byte(strings.Repeat("日", 40000))
	writeWorktreeFile(t, root, "payload.txt", payload)

	spans := &collector{}
	if _, err := service.Run(t.Context(), "cat payload.txt", spans.emit); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(spans.spans) < 2 {
		t.Fatalf("output arrived in %d spans, want it chunked", len(spans.spans))
	}
	for index, span := range spans.spans {
		if span.Encoding != "" {
			t.Fatalf("span %d was encoded as %q; valid text must stay text", index, span.Encoding)
		}
		if !utf8.ValidString(span.Data) {
			t.Fatalf("span %d is not valid UTF-8, so a rune was split", index)
		}
	}
	if got := spans.bytes(t); string(got) != string(payload) {
		t.Fatalf("forwarded %d bytes, want the command's %d", len(got), len(payload))
	}
}

func TestRunTruncatesPastTheOutputCap(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	line := strings.Repeat("x", 1023) + "\n"
	payload := []byte(strings.Repeat(line, 1536))
	if len(payload) <= workspacesession.MaxExecOutputBytes {
		t.Fatalf("the payload is %d bytes, want more than the cap", len(payload))
	}
	writeWorktreeFile(t, root, "payload.txt", payload)

	spans := &collector{}
	result, err := service.Run(t.Context(), "cat payload.txt", spans.emit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Truncated {
		t.Fatalf("result = %+v, want it marked truncated", result)
	}
	markers := spans.markers()
	if len(markers) != 1 {
		t.Fatalf("%d truncation markers, want exactly one", len(markers))
	}
	// A marker rather than a silent stop: a reader who cannot tell a finished
	// log from a cut one reads the cut one as finished.
	if markers[0].Data != workspacesession.ExecTruncationMarker {
		t.Fatalf("marker = %q, want %q", markers[0].Data, workspacesession.ExecTruncationMarker)
	}
	if forwarded := len(spans.bytes(t)); forwarded != workspacesession.MaxExecOutputBytes {
		t.Fatalf("forwarded %d bytes, want exactly the %d-byte cap", forwarded, workspacesession.MaxExecOutputBytes)
	}
}

// TestRunKeepsDrainingPastTheCap is the whole reason the pipe is drained rather
// than closed once the cap is reached: a command blocked writing into a full
// pipe would exit on a broken pipe, and its exit code would be the pipe's story
// instead of its own.
func TestRunKeepsDrainingPastTheCap(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	payload := []byte(strings.Repeat(strings.Repeat("y", 1023)+"\n", 4096))
	writeWorktreeFile(t, root, "payload.txt", payload)

	spans := &collector{}
	result, err := service.Run(t.Context(), "cat payload.txt && exit 0", spans.emit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d (signal %q), want the command's own 0", result.ExitCode, result.Signal)
	}
	if result.Bytes != int64(len(payload)) {
		t.Fatalf("read %d bytes, want every one of the command's %d", result.Bytes, len(payload))
	}
	if forwarded := len(spans.bytes(t)); forwarded != workspacesession.MaxExecOutputBytes {
		t.Fatalf("forwarded %d bytes, want the cap to hold at %d", forwarded, workspacesession.MaxExecOutputBytes)
	}
}

func TestRunReportsTheCommandsExitCode(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	tests := []struct {
		name    string
		command string
		want    int
	}{
		{name: "success", command: "printf ok", want: 0},
		{name: "failure", command: "printf nope >&2; exit 3", want: 3},
		{name: "not found", command: "detent-no-such-command-9f2c", want: 127},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, _ := newService(t)
			spans := &collector{}
			result, err := service.Run(t.Context(), test.command, spans.emit)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.ExitCode != test.want {
				t.Fatalf("exit code = %d, want %d", result.ExitCode, test.want)
			}
			if result.Signal != "" {
				t.Fatalf("signal = %q, want none for a command that exited", result.Signal)
			}
		})
	}
}

// TestRunNamesTheSignalThatKilledTheCommand covers the case where there is no
// exit code to report: "exited 0" and "was killed" are different facts and a
// run report has to be able to say which happened.
func TestRunNamesTheSignalThatKilledTheCommand(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, _ := newService(t)
	spans := &collector{}
	result, err := service.Run(t.Context(), "kill -TERM $$", spans.emit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Signal != "SIGTERM" {
		t.Fatalf("signal = %q, want SIGTERM", result.Signal)
	}
	if result.ExitCode >= 0 {
		t.Fatalf("exit code = %d, want a signalled process to report none", result.ExitCode)
	}
}

// TestRunKillsTheWholeProcessGroup is the "kill the process on stream close or
// lease loss" requirement: a command that started a build or a server must not
// leave it running in the worktree, and killing only the shell would.
func TestRunKillsTheWholeProcessGroup(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	witness := filepath.Join(root, "witness")
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var once bool
	emit := func(workspacesession.ExecOutput) error {
		if !once {
			once = true
			close(started)
		}
		return nil
	}
	// The grandchild outlives its parent shell on purpose: it is the process a
	// kill aimed at the shell alone would leave behind, and the file it would
	// write is the proof.
	command := "sh -c 'sleep 2; : > " + witness + "' & printf started\n; sleep 5"
	done := make(chan error, 1)
	go func() {
		_, err := service.Run(ctx, command, emit)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the command never produced output")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run answered %v, want the caller's own cancellation", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not end when its context was cancelled")
	}

	// Past the grandchild's own sleep: if the group had survived, the witness
	// would exist by now.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the whole process group to have died", err)
	}
}

func TestRunWorksInTheWorktree(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, _ := newService(t)
	spans := &collector{}
	if _, err := service.Run(t.Context(), "pwd", spans.emit); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(string(spans.bytes(t))); got != service.Path() {
		t.Fatalf("working directory = %q, want the worktree %q", got, service.Path())
	}
}

func TestRunBase64EncodesASpanThatIsNotText(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	payload := []byte{0x00, 0xff, 0xfe, 0x41, 0x80}
	writeWorktreeFile(t, root, "payload.bin", payload)

	spans := &collector{}
	if _, err := service.Run(t.Context(), "cat payload.bin", spans.emit); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(spans.spans) != 1 || spans.spans[0].Encoding != "base64" {
		t.Fatalf("spans = %+v, want one base64 span", spans.spans)
	}
	if got := spans.bytes(t); string(got) != string(payload) {
		t.Fatalf("decoded %v, want %v", got, payload)
	}
}

// TestRunScrubsInheritedCredentials is a courtesy and not a boundary (section
// 18.3): the command runs as the runner account and can still reach whatever
// that account can reach. What it must not inherit is a token the runner
// process merely happened to be started with.
func TestRunScrubsInheritedCredentials(t *testing.T) {
	requirePOSIXShell(t)

	for key, value := range map[string]string{
		"ANTHROPIC_API_KEY":  "sk-anthropic",
		"OPENAI_BASE_URL":    "https://example.invalid",
		"CODEX_HOME":         "/tmp/codex",
		"CLAUDE_CODE_TOKEN":  "claude-token",
		"DETENT_HUB_TOKEN":   "detent-token",
		"TRACKER_API_KEY":    "tracker-key",
		"DETENT_ACTION_KEEP": "kept",
		"PROJECT_STAGE":      "kept",
	} {
		t.Setenv(key, value)
	}

	service, _ := newService(t)
	spans := &collector{}
	if _, err := service.Run(t.Context(), "env", spans.emit); err != nil {
		t.Fatalf("run: %v", err)
	}
	environment := string(spans.bytes(t))
	for _, dropped := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME", "CLAUDE_CODE_TOKEN",
		"DETENT_HUB_TOKEN", "TRACKER_API_KEY", "DETENT_ACTION_KEEP",
	} {
		if strings.Contains(environment, dropped+"=") {
			t.Fatalf("the child inherited %s", dropped)
		}
	}
	if !strings.Contains(environment, "PROJECT_STAGE=kept") {
		t.Fatal("the scrub removed a variable that is not a credential")
	}
	if !strings.Contains(environment, "DETENT_WORKSPACE="+service.Path()) {
		t.Fatalf("environment = %q, want DETENT_WORKSPACE set to the worktree", environment)
	}
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

// TestRunEndsWhenTheReaderIsGone covers the only reason to refuse a span: the
// person it was going for has left, and there is nothing to be gained by
// running their command to completion.
func TestRunEndsWhenTheReaderIsGone(t *testing.T) {
	t.Parallel()
	requirePOSIXShell(t)

	service, root := newService(t)
	writeWorktreeFile(t, root, "payload.txt", []byte(strings.Repeat("z", 512<<10)))
	refusal := errors.New("the reader is gone")
	_, err := service.Run(t.Context(), "cat payload.txt; sleep 30", func(workspacesession.ExecOutput) error {
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("run answered %v, want the refusal that ended it", err)
	}
}
