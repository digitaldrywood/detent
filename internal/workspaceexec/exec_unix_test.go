//go:build !windows

package workspaceexec

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRunChunksOutputWithinTheFrameCap(t *testing.T) {
	t.Parallel()

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

	service, _ := newService(t)
	started := make(chan int, 1)
	ctx, cancel := context.WithCancel(t.Context())
	waitCtx := testenv.WaitContext(t, t.Context())
	var output string
	emit := func(span workspacesession.ExecOutput) error {
		data := []byte(span.Data)
		if span.Encoding != "" {
			var err error
			data, err = base64.StdEncoding.DecodeString(span.Data)
			if err != nil {
				return err
			}
		}
		output += string(data)
		if line, _, ok := strings.Cut(output, "\n"); ok {
			pid, err := strconv.Atoi(line)
			if err != nil {
				return err
			}
			started <- pid
		}
		return nil
	}
	command := `mkfifo gate; sh -c 'printf "%s\n" "$$"; exec >/dev/null 2>&1; exec cat gate' & wait`
	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = service.Run(ctx, command, emit)
		close(done)
	}()
	var identity procgroup.Identity
	defer func() {
		cancel()
		if identity.PID != 0 {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cleanupCancel()
			if _, err := procgroup.Terminate(cleanupCtx, identity, procgroup.DefaultTerminationGrace); err != nil {
				t.Errorf("clean up child process group: %v", err)
			}
		}
		<-done
	}()

	var pid int
	select {
	case pid = <-started:
	case <-done:
		t.Fatalf("command exited before child readiness: %v", runErr)
	case <-waitCtx.Done():
		t.Fatal("child never became ready")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	identity, err = procgroup.Inspect(&exec.Cmd{Process: process})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("run answered %v, want the caller's own cancellation", runErr)
		}
	case <-waitCtx.Done():
		t.Fatal("the run did not end when its context was cancelled")
	}
	observations, err := procgroup.Observe([]procgroup.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	if observations[0].Alive {
		t.Fatal("child process group survived cancellation")
	}
}

func TestRunWorksInTheWorktree(t *testing.T) {
	t.Parallel()

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

// TestRunScrubsInheritedCredentials is a courtesy and not a boundary:
// the command runs as the runner account and can still reach whatever
// that account can reach. What it must not inherit is a token the runner
// process merely happened to be started with.
func TestRunScrubsInheritedCredentials(t *testing.T) {

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

// TestRunEndsWhenTheReaderIsGone covers the only reason to refuse a span: the
// person it was going for has left, and there is nothing to be gained by
// running their command to completion.
func TestRunEndsWhenTheReaderIsGone(t *testing.T) {
	t.Parallel()

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
