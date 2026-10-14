// Package workspaceexec runs one project action in one worktree and streams
// its output (decisions section 18.12).
//
// Everything here exists to keep three promises a reader depends on. The output
// they see is the output a terminal would have shown, in the order the shell
// produced it. The exit code they see is the command's own, which is why the
// pipe keeps being drained after the output cap rather than closed: a command
// blocked writing into a full pipe would exit on a broken pipe instead of on
// what it was doing. And a run that is stopped is stopped whole -- the shell and
// everything it started -- because a setup command that spawns a server and
// leaves it behind is a worktree nobody can reuse.
//
// The command itself is not validated. An action's command is shell syntax by
// construction (section 18.12's NormalizeActionCommand says so), and a runner
// that tried to decide which commands are safe would be pretending to a
// boundary it does not have. What bounds it is who may write an action and
// where it runs.
package workspaceexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Error is a refusal in the exec channel's own vocabulary. The code is what
// reaches the person; the wrapped cause is for the runner's log.
type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.Code
	}
	return e.Code + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error { return e.cause }

func refuse(code string, cause error) error { return &Error{Code: code, cause: cause} }

// ErrorCode reports the channel code for an error, or the empty string when it
// is not one of ours. It mirrors workspacefiles.ErrorCode so the session maps
// both channels' refusals the same way.
func ErrorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// Service runs actions in one worktree.
type Service struct {
	// worktree is the canonical path every run uses as its working directory.
	// It is resolved once, at construction, so a run cannot be talked into a
	// different directory by a path the request supplied.
	worktree string
	// shell is the project's configured shell. It is the project's choice and
	// not the runner's guess: a command an author wrote for bash must not be
	// handed to cmd because the runner defaulted.
	shell  string
	logger *slog.Logger
}

// New prepares a service for a worktree. It starts no process.
func New(worktree string, shellName string, logger *slog.Logger) (*Service, error) {
	canonical, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree %q: %w", worktree, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("stat worktree %q: %w", canonical, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("worktree %q is not a directory", canonical)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		worktree: canonical,
		shell:    shell.Normalize(shellName),
		logger:   logger.With("component", "workspace_exec"),
	}, nil
}

// Path reports the worktree every run works in.
func (s *Service) Path() string { return s.worktree }

// Shell reports the shell every run goes through.
func (s *Service) Shell() string { return s.shell }

// Result is how one run ended.
//
// ExitCode is the command's own status, or -1 when a signal ended it instead --
// which is what Signal names. Bytes counts everything the command wrote,
// including what was produced after the cap and therefore never forwarded, so a
// reader is told the size of the log rather than the size of the excerpt.
type Result struct {
	ExitCode  int
	Signal    string
	Bytes     int64
	Truncated bool
}

// Run runs one command in the worktree and hands each output span to emit.
//
// emit is called from Run's own goroutine, in order, and a span it refuses ends
// the run: the only reason to refuse one is that the reader it was going for is
// gone, and there is no point running a command for nobody.
//
// A cancelled ctx kills the whole process group and Run answers with ctx's
// error, so a caller that stopped the run is never handed an exit code that
// looks like the command's own verdict.
func (s *Service) Run(ctx context.Context, command string, emit func(workspacesession.ExecOutput) error) (Result, error) {
	if emit == nil {
		return Result{}, refuse(workspacesession.CodeInvalidFrame, errors.New("an output sink is required"))
	}
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return Result{}, refuse(workspacesession.CodeInvalidFrame, errors.New("a command is required"))
	}
	if len(trimmed) > workspacesession.MaxExecCommandBytes {
		return Result{}, refuse(workspacesession.CodeTooLarge, errors.New("the command is longer than this surface runs"))
	}

	// The run has a cancel of its own so a refused span or a failed read can
	// end the process as decisively as a cancelled ctx does.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The argv is the shell package's, never hand-built: it is what already
	// knows that a command is {sh, -c, command} on Unix and a quoted /C line
	// for cmd on Windows.
	cmd := shell.Command(runCtx, trimmed, s.shell)
	cmd.Dir = s.worktree
	cmd.Env = scrubEnvironment(os.Environ(), s.worktree)
	// Stdin is left unset, which is the null device. An action runs with nobody
	// watching (run-on-worktree-creation), so a command that decided to prompt
	// must read EOF rather than wait for a person who is not there.

	// One pipe carries both streams, so what a reader sees is interleaved the
	// way the shell interleaved it. The ordering is the shell's and not ours:
	// two pipes read by two goroutines would reorder a diagnostic against the
	// line it belongs to.
	reader, writer, err := os.Pipe()
	if err != nil {
		return Result{}, refuse(workspacesession.CodeForbidden, fmt.Errorf("open output pipe: %w", err))
	}
	cmd.Stdout = writer
	cmd.Stderr = writer

	// Setpgid plus a cancel that signals the group: killing only the shell
	// would leave the real work -- the build, the installer, the server it
	// started -- running in the worktree with nobody left to stop it.
	procgroup.Configure(runCtx, cmd)

	if err := cmd.Start(); err != nil {
		return Result{}, errors.Join(refuse(startCode(err), fmt.Errorf("start action: %w", err)), writer.Close(), reader.Close())
	}
	s.logger.Debug("workspace.action_started", "worktree", s.worktree, "shell", s.shell, "pid", cmd.Process.Pid)
	// The parent's copy of the write end is closed immediately: the read below
	// ends on EOF only once every holder of the write end is gone, and the
	// parent holding one would mean waiting for the parent.
	if err := writer.Close(); err != nil {
		s.logger.Debug("workspace.action_pipe_not_closed", "error", err)
	}

	result, emitErr := s.forward(reader, emit)
	if emitErr != nil {
		// The reader is gone. Kill the group before waiting, or Wait would
		// block for as long as the command felt like running.
		cancel()
	}
	if err := reader.Close(); err != nil {
		s.logger.Debug("workspace.action_pipe_not_closed", "error", err)
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			// Anything that is not an exit status is a failure to run the
			// command at all, not a verdict from it.
			return result, fmt.Errorf("run action: %w", waitErr)
		}
		result.ExitCode = exit.ExitCode()
		result.Signal = exitSignal(exit.ProcessState)
	}
	if emitErr != nil {
		return result, emitErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

// forward streams the command's output to emit, chunked to the frame cap and
// capped in total.
//
// Past MaxExecOutputBytes it stops forwarding and keeps reading. That is the
// whole point of draining rather than closing: a command whose output has
// nowhere to go blocks in write(), and a blocked command's exit code is the
// pipe's story rather than its own.
func (s *Service) forward(reader io.Reader, emit func(workspacesession.ExecOutput) error) (Result, error) {
	var result Result
	forwarded := 0
	forwarding := true

	// send forwards one span, applies the total cap, and marks the cut exactly
	// once. A marker rather than a silent stop, because a reader who cannot
	// tell a finished log from a cut one reads the cut one as finished.
	send := func(span []byte) error {
		if !forwarding || len(span) == 0 {
			return nil
		}
		out := span
		if allowance := max(workspacesession.MaxExecOutputBytes-forwarded, 0); len(out) > allowance {
			out = boundedSpan(out[:allowance])
		}
		if len(out) > 0 {
			if err := emit(outputSpan(out)); err != nil {
				return err
			}
			forwarded += len(out)
		}
		if len(out) == len(span) {
			return nil
		}
		forwarding = false
		result.Truncated = true
		s.logger.Info("workspace.action_output_truncated", "worktree", s.worktree, "bytes", forwarded)
		return emit(workspacesession.ExecOutput{Data: workspacesession.ExecTruncationMarker, Truncated: true})
	}

	// The read is a few bytes short of the frame cap so that a partial rune
	// held back from the previous read still fits in this span: a span is then
	// never split twice and never exceeds MaxExecOutputFrameBytes.
	buffer := make([]byte, workspacesession.MaxExecOutputFrameBytes-utf8.UTFMax)
	var pending []byte
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			result.Bytes += int64(read)
			data := buffer[:read]
			if len(pending) > 0 {
				data = append(pending, data...)
			}
			span, held := boundedSpanAndRemainder(data)
			// held is copied rather than retained: it points into the same
			// array the next read will overwrite.
			pending = append([]byte(nil), held...)
			if sendErr := send(span); sendErr != nil {
				return result, sendErr
			}
		}
		if err != nil {
			// EOF is the ordinary end; a closed pipe is the same fact arriving
			// as an error, and either way the command's own status is what
			// Wait will report.
			break
		}
	}
	// Whatever was held back for a rune that never arrived is still output, and
	// a reader is owed it: it goes out as it stands, base64 because it is not
	// valid text.
	if err := send(pending); err != nil {
		return result, err
	}
	return result, nil
}

// outputSpan describes one span for the wire. Valid text is sent as text so a
// log view can use it without a decode step; anything else is base64, which at
// the frame cap is still comfortably inside MaxFrameBytes.
//
// A span holding a NUL byte is treated as binary even when it is valid UTF-8,
// the same judgement workspacefiles makes about a file: a reader asking for a
// log has no use for a NUL in the middle of one.
func outputSpan(span []byte) workspacesession.ExecOutput {
	if utf8.Valid(span) && !bytes.ContainsRune(span, 0) {
		return workspacesession.ExecOutput{Data: string(span)}
	}
	return workspacesession.ExecOutput{Data: base64.StdEncoding.EncodeToString(span), Encoding: "base64"}
}

// boundedSpanAndRemainder splits a read into the part that may go out now and a
// trailing partial rune held back for the next one.
//
// Splitting on a byte count alone would cut a multi-byte rune in half and hand
// a reader two spans neither of which is text, so the character a person typed
// in a filename would arrive as two replacement glyphs.
func boundedSpanAndRemainder(data []byte) (span, held []byte) {
	for back := 1; back <= utf8.UTFMax && back <= len(data); back++ {
		index := len(data) - back
		leading := data[index]
		if leading&0xC0 == 0x80 {
			// A continuation byte says nothing about how long its sequence is;
			// the byte that starts the sequence does.
			continue
		}
		if size := sequenceLength(leading); size > back {
			return data[:index], data[index:]
		}
		return data, nil
	}
	// Either the data is shorter than one rune's worth of continuation bytes or
	// it is not UTF-8 at all. Neither is a reason to hold anything back.
	return data, nil
}

// boundedSpan drops a trailing partial rune from a span that is being cut to
// fit an allowance.
func boundedSpan(data []byte) []byte {
	span, _ := boundedSpanAndRemainder(data)
	return span
}

// sequenceLength reports how many bytes the UTF-8 sequence starting with
// leading has, or zero when it cannot start one.
func sequenceLength(leading byte) int {
	switch {
	case leading < 0x80:
		return 1
	case leading&0xE0 == 0xC0:
		return 2
	case leading&0xF0 == 0xE0:
		return 3
	case leading&0xF8 == 0xF0:
		return 4
	default:
		return 0
	}
}

// credentialPrefixes are the environment variable families this surface drops.
var credentialPrefixes = []string{"DETENT_", "OPENAI_", "ANTHROPIC_", "CODEX_", "CLAUDE_"}

// scrubEnvironment removes provider and Detent credentials from the child's
// environment and points DETENT_WORKSPACE at the worktree.
//
// Section 18.3 says exactly what this is worth for a user-isolation surface: it
// is a courtesy, not a boundary. The command runs as the runner account, so
// every key that account can reach on its own -- a logged-in CLI's token, a
// credential file, a keychain -- it can still reach. What this removes is the
// accidental inheritance of whatever the runner process happened to be started
// with, which is the part nobody chose to hand over.
func scrubEnvironment(environ []string, worktree string) []string {
	kept := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if ok && credentialKey(key) {
			continue
		}
		kept = append(kept, entry)
	}
	// DETENT_WORKSPACE is set after the scrub, not before: the DETENT_ family
	// is dropped wholesale and the worktree is the one member of it an action
	// is meant to see. It matches what the workspace hooks already export.
	return append(kept, "DETENT_WORKSPACE="+worktree)
}

// credentialKey reports whether a variable name is one this surface drops. The
// name is upper-cased first so the same list holds on Windows, where the
// environment is case-insensitive.
func credentialKey(key string) bool {
	name := strings.ToUpper(strings.TrimSpace(key))
	if name == "API_KEY" || strings.HasSuffix(name, "_API_KEY") {
		return true
	}
	for _, prefix := range credentialPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// startCode maps a failure to start the command onto the channel's vocabulary.
// A missing shell is unsupported rather than forbidden: nothing was refused,
// this runner simply cannot run the project's shell.
func startCode(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return workspacesession.CodeUnsupported
	default:
		return workspacesession.CodeForbidden
	}
}
