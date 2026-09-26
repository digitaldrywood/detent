// Package workspaceterminal runs one PTY in one worktree and streams what it
// writes (decisions section 18.3).
//
// It is not the exec channel with a shell typed into it. An action (section
// 18.12) is a command the project wrote down: it is non-interactive, it ends on
// its own, it produces one bounded output with the issue's audience, and it may
// run with nobody watching. A terminal is the runner account's authority handed
// to a person, interactively, for as long as they keep looking at it. Every
// difference that follows -- a pseudo-terminal rather than a pipe, a window size
// the person controls, no output cap, a SIGHUP-then-SIGKILL teardown, and a
// process that outlives a dropped connection for the resume window -- is a
// consequence of that one.
//
// What this package is worth as a boundary is section 18.3's own admission,
// repeated here because a reader of this file is exactly who needs it:
// stripping environment variables does not confine a shell. At `user` isolation
// the process runs as the runner's account and can read that account's provider
// login files, its credential store, other checkouts and anything else it can
// reach. The scrub below is a courtesy. The boundary is who may open one, and
// that is enforced by the hub before a frame ever arrives here.
package workspaceterminal

import (
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
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/pathsafe"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Error is a refusal in the terminal channel's own vocabulary. The code is what
// reaches the person; the wrapped cause is for the runner's log. It mirrors
// workspaceexec.Error and workspacefiles.Error so the session maps every
// channel's refusals the same way.
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
// is not one of ours.
func ErrorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// KillGrace is how long a terminal has between the SIGHUP that asks its shell
// to leave and the SIGKILL that makes it. SIGHUP is the honest first signal for
// a PTY: it is what a real terminal sends when its window closes, so a shell
// receiving it runs the same exit path it would on a closed window, flushing
// history and taking its job control with it. SIGKILL after the grace is what
// makes the promise unconditional.
const KillGrace = 5 * time.Second

// Service opens terminals in one worktree.
type Service struct {
	// worktree is the canonical path every terminal starts in. It is resolved
	// once, at construction, so an open cannot be talked into a different
	// directory by a path the request supplied.
	worktree string
	// shell is the project's configured shell, not the runner's guess, for the
	// same reason an action's command runs through it: it is the shell the
	// project's own hooks and actions already use.
	shell string
	// isolation is the level this service actually runs at. It is decided at
	// construction rather than per open, because it is a fact about what this
	// runner can provide and not about one request.
	isolation string
	logger    *slog.Logger
}

// ErrContainerIsolation reports that container isolation was asked for and this
// runner cannot provide it.
//
// Section 18.3 describes `container` as a PTY inside a container that mounts
// only the worktree, has no access to the runner's home directory, credential
// files or sockets, and runs as an unprivileged user. This repository ships no
// container runtime hook of any kind -- there is nothing in the runner that
// starts, attaches to or even names a container -- so there is nothing here to
// hang that level on. Returning a plain PTY and calling it `container` would be
// the one failure this whole surface must not have: an organization choosing
// the recommended level and receiving the other one, silently. A runner that
// cannot provide container reports `user` and the terminal card stays disabled
// for organizations that require container, with the reason, which is exactly
// what section 18.3 says such a runner must do.
var ErrContainerIsolation = errors.New("workspaceterminal: container isolation is not implemented by this runner")

// New prepares a service for a worktree. It starts no process.
//
// isolation is the level the organization asked for. `user` is served; anything
// else is refused with ErrContainerIsolation rather than approximated, and the
// caller reports the runner's own level so the hub and the client can say why
// the card is disabled.
func New(worktree string, shellName string, isolation string, logger *slog.Logger) (*Service, error) {
	if isolation == "" {
		isolation = workspacesession.IsolationUser
	}
	if isolation != workspacesession.IsolationUser {
		if !workspacesession.ValidIsolation(isolation) {
			return nil, fmt.Errorf("workspaceterminal: unknown isolation %q", isolation)
		}
		return nil, ErrContainerIsolation
	}
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
		worktree:  canonical,
		shell:     shell.Normalize(shellName),
		isolation: isolation,
		logger:    logger.With("component", "workspace_terminal"),
	}, nil
}

// Path reports the worktree every terminal starts in.
func (s *Service) Path() string { return s.worktree }

// Shell reports the shell every terminal runs.
func (s *Service) Shell() string { return s.shell }

// Isolation reports the level this service serves.
func (s *Service) Isolation() string { return s.isolation }

// Result is how a terminal ended. ExitCode is the shell's own status, or -1
// when a signal ended it instead, which is what Signal names.
type Result struct {
	ExitCode int
	Signal   string
}

// Terminal is one PTY, held open for as long as the person is looking at it.
type Terminal struct {
	cmd  *exec.Cmd
	file *os.File
	// pid is captured at start rather than read from cmd.Process afterwards:
	// Wait releases the process handle, and a caller that answers an exit frame
	// after the wait still wants to log which process it was.
	pid       int
	isolation string
	logger    *slog.Logger

	// done closes when the shell has been waited on and result is final.
	done   chan struct{}
	result Result

	mu sync.Mutex
	// closed records that a teardown has already been asked for, so a close
	// racing an exit does not signal a pid the operating system has since
	// handed to somebody else.
	closed bool
	cols   int
	rows   int
}

// Open starts a shell on a new PTY and streams its output to emit.
//
// emit is called from a goroutine of the terminal's own, serially and in order,
// until the shell exits or the PTY is closed. A span it refuses ends the
// streaming but not the shell: a person whose connection dropped may come back
// inside the resume window (section 18.2), and killing their shell because one
// write failed would make the resume window a lie. The caller decides when the
// shell dies, through Close.
//
// The returned Terminal is live: Write feeds it, Resize changes its window, and
// Wait reports how it ended.
func (s *Service) Open(
	ctx context.Context,
	request workspacesession.TerminalOpen,
	emit func(workspacesession.TerminalOutput) error,
) (*Terminal, error) {
	if emit == nil {
		return nil, refuse(workspacesession.CodeInvalidFrame, errors.New("an output sink is required"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	options, err := workspacesession.ValidateTerminalOpen(request)
	if err != nil {
		return nil, refuse(workspacesession.CodeInvalidFrame, err)
	}
	directory, err := s.resolveCwd(options.Cwd)
	if err != nil {
		return nil, err
	}

	// The shell is started interactively and with no command, which is the
	// whole difference from the exec channel: there is nothing to run, because
	// the person is going to type it. That is also why the argv is built here
	// rather than through shell.Command, which exists to wrap one command in a
	// -c and is the wrong shape for a shell that is going to read its input
	// from a terminal.
	//
	// #nosec G204 -- the shell is the project's configured one, and section
	// 18.3 is explicit that what bounds a terminal is who may open it rather
	// than what it may run.
	// The command is not tied to ctx: cancellation is handled below through
	// Close, so the whole process group gets the hangup and the delayed kill.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), s.shell, interactiveArgs(s.shell)...)
	cmd.Dir = directory
	cmd.Env = terminalEnvironment(os.Environ(), s.worktree, options.Cols, options.Rows)

	file, err := startPTY(cmd, options.Cols, options.Rows)
	if err != nil {
		return nil, refuse(startCode(err), fmt.Errorf("start terminal: %w", err))
	}

	terminal := &Terminal{
		cmd:       cmd,
		file:      file,
		pid:       cmd.Process.Pid,
		isolation: s.isolation,
		logger:    s.logger.With("pid", cmd.Process.Pid),
		done:      make(chan struct{}),
		cols:      options.Cols,
		rows:      options.Rows,
	}
	s.logger.Info("workspace.terminal_started", "worktree", directory, "shell", s.shell,
		"pid", terminal.pid, "isolation", s.isolation, "cols", options.Cols, "rows", options.Rows)

	go terminal.stream(emit)
	go terminal.wait()
	// A cancelled context ends the terminal exactly the way Close does:
	// SIGHUP to the shell's process group, then SIGKILL to the whole group
	// after KillGrace. exec's own cancellation would signal the leader alone,
	// and a child that ignored the hangup would outlive the session.
	go func() {
		select {
		case <-ctx.Done():
			terminal.Close()
		case <-terminal.done:
		}
	}()
	return terminal, nil
}

// resolveCwd turns the open's optional relative directory into an absolute one
// inside the worktree.
//
// Containment is checked here rather than trusted, on the same reasoning the
// files channel applies (section 18.4): a path a client supplied must not be
// able to name somewhere else. It matters less than it does there -- the shell
// can cd anywhere the runner's account can reach the moment it starts, which
// section 18.3 says outright -- but starting a person somewhere they did not
// ask to be is still a surprise, and a refusal that names the reason is better
// than a shell that silently opens in the wrong tree.
func (s *Service) resolveCwd(relative string) (string, error) {
	if relative == "" {
		return s.worktree, nil
	}
	joined, err := pathsafe.WorkspaceRelative(s.worktree, relative)
	if err != nil {
		return "", refuse(workspacesession.CodeForbidden, fmt.Errorf("cwd %q: %w", relative, err))
	}
	info, err := os.Stat(joined)
	if err != nil {
		return "", refuse(workspacesession.CodeNotFound, fmt.Errorf("stat cwd %q: %w", relative, err))
	}
	if !info.IsDir() {
		return "", refuse(workspacesession.CodeNotFound, fmt.Errorf("cwd %q is not a directory", relative))
	}
	return joined, nil
}

// PID reports the shell's process id.
func (t *Terminal) PID() int { return t.pid }

// Isolation reports the level this PTY runs at.
func (t *Terminal) Isolation() string { return t.isolation }

// Size reports the window the PTY currently has.
func (t *Terminal) Size() (cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// Write feeds the person's keystrokes to the shell.
func (t *Terminal) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if t.ended() {
		return refuse(workspacesession.CodeNotFound, errors.New("this terminal has ended"))
	}
	if _, err := t.file.Write(data); err != nil {
		// A write to a PTY whose shell has gone is the shell's exit arriving as
		// an error. The exit frame is what reports it, so this is not a second
		// failure to tell the person about.
		if errors.Is(err, os.ErrClosed) || errors.Is(err, fs.ErrClosed) {
			return refuse(workspacesession.CodeNotFound, errors.New("this terminal has ended"))
		}
		return refuse(workspacesession.CodeForbidden, fmt.Errorf("write to terminal: %w", err))
	}
	return nil
}

// Resize changes the PTY's window.
//
// It is a real ioctl and not bookkeeping: a program that draws to the window --
// an editor, a pager, anything full-screen -- learns about the change from
// SIGWINCH and from the winsize it then reads, and without this it keeps
// drawing at the size the window had when it started.
func (t *Terminal) Resize(cols, rows int) error {
	if t.ended() {
		return refuse(workspacesession.CodeNotFound, errors.New("this terminal has ended"))
	}
	if err := setWindowSize(t.file, cols, rows); err != nil {
		return refuse(workspacesession.CodeForbidden, fmt.Errorf("resize terminal: %w", err))
	}
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	t.mu.Unlock()
	return nil
}

// Close ends the terminal: SIGHUP to the shell's process group, then SIGKILL
// after KillGrace, then the PTY itself.
//
// The signal goes to the group rather than to the shell, for the reason the
// exec channel kills a group: a shell that started a build or a server and is
// then killed on its own leaves the real work running in a worktree nobody is
// left to stop it from. The PTY is closed last, because closing it first would
// take the shell's controlling terminal away before it had a chance to act on
// the hangup.
//
// Close is safe to call more than once and from more than one goroutine; only
// the first call signals anything.
func (t *Terminal) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()

	select {
	case <-t.done:
		// The shell is already gone. There is nothing to signal, and signalling
		// a pid the operating system may have handed to somebody else is the
		// one thing a teardown must never do.
		t.closeFile()
		return
	default:
	}

	t.logger.Debug("workspace.terminal_closing")
	if err := hangup(t.cmd); err != nil {
		t.logger.Debug("workspace.terminal_hangup_failed", "error", err)
	}
	// The shell leaving is not enough: a child that ignored the hangup keeps
	// running in the group after its shell has gone. Close returns early only
	// once the whole group is gone, and otherwise kills the group when the
	// grace runs out.
	deadline := time.NewTimer(KillGrace)
	defer deadline.Stop()
	poll := time.NewTicker(groupPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-deadline.C:
			t.logger.Info("workspace.terminal_killed", "grace", KillGrace)
			if err := kill(t.cmd); err != nil {
				t.logger.Debug("workspace.terminal_kill_failed", "error", err)
			}
			t.closeFile()
			return
		case <-poll.C:
			if t.ended() && !groupAlive(t.pid) {
				t.closeFile()
				return
			}
		}
	}
}

// groupPollInterval is how often Close checks whether the shell's process
// group has emptied during the grace period.
const groupPollInterval = 20 * time.Millisecond

// closeFile releases the PTY. It ends the stream goroutine's read, which is
// what makes a terminal whose shell is unreachable still stop producing.
func (t *Terminal) closeFile() {
	if err := t.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.logger.Debug("workspace.terminal_pty_not_closed", "error", err)
	}
}

// Wait blocks until the shell has ended and reports how.
func (t *Terminal) Wait() Result {
	<-t.done
	return t.result
}

// Done answers a channel that closes when the shell has ended.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// ended reports whether the shell has already gone.
func (t *Terminal) ended() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// wait reaps the shell and publishes the result.
//
// It releases the PTY on the way out, whatever ended the shell. Closing it is
// also what ends the streaming goroutine's read, so a terminal that exited on
// its own stops producing without anybody having to call Close: an exited shell
// whose descriptor lived until the session's own teardown reached it would be
// one leaked descriptor per terminal, on a session that may run for hours.
func (t *Terminal) wait() {
	defer func() {
		close(t.done)
		t.closeFile()
	}()
	err := t.cmd.Wait()
	if err == nil {
		return
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		// Anything that is not an exit status is a failure to run the shell at
		// all rather than a verdict from it. -1 is what says so, and it is the
		// same value a signalled process reports, which is correct: neither has
		// an exit code of its own.
		t.logger.Warn("workspace.terminal_wait_failed", "error", err)
		t.result = Result{ExitCode: -1}
		return
	}
	t.result = Result{ExitCode: exit.ExitCode(), Signal: exitSignal(exit.ProcessState)}
}

// stream forwards the PTY's output until it ends.
//
// There is no output cap, and its absence is the contract rather than an
// oversight. An action's output is a log somebody reads afterwards, so section
// 18.12 caps it at a size worth storing; a terminal's output is a screen
// somebody is watching, and a shell that stopped producing after a megabyte
// would be a terminal that stopped working. What bounds a terminal instead is
// the relay: the hub holds at most StreamBufferBytes of unacknowledged frames
// per stream and closes the stream with overflow past that (section 18.2), so
// backpressure is honoured through the acks rather than by truncation here.
func (t *Terminal) stream(emit func(workspacesession.TerminalOutput) error) {
	buffer := make([]byte, workspacesession.MaxTerminalOutputFrameBytes)
	for {
		read, err := t.file.Read(buffer)
		if read > 0 {
			span := workspacesession.TerminalOutput{
				Data:     base64.StdEncoding.EncodeToString(buffer[:read]),
				Encoding: workspacesession.EncodingBase64,
			}
			if emitErr := emit(span); emitErr != nil {
				// The reader is gone. The shell is left running: a person whose
				// connection dropped may resume inside the window, and the
				// caller is what decides when that window has passed.
				t.logger.Debug("workspace.terminal_output_not_sent", "error", emitErr)
				return
			}
		}
		if err != nil {
			// A PTY reports its shell's exit as EIO rather than EOF on Linux
			// and as EOF on Darwin. Both are the same fact, and the exit frame
			// comes from Wait rather than from here, so neither is logged as a
			// failure.
			if !errors.Is(err, io.EOF) {
				t.logger.Debug("workspace.terminal_read_ended", "error", err)
			}
			return
		}
	}
}

// interactiveArgs is the argv that makes a shell interactive.
//
// An interactive shell is what a terminal is for: it prints a prompt, it reads
// its interactive startup files, and it turns on job control, so ^C reaches the
// foreground job rather than the shell. Without it a shell attached to a PTY
// reads a script from the terminal and never prompts, which reads to a person
// as a terminal that has hung.
//
// cmd and PowerShell take no such flag -- they are interactive when they have a
// console and not otherwise -- so they are handed nothing rather than an
// argument they would refuse.
func interactiveArgs(shellName string) []string {
	// The separator is checked for both platforms rather than through
	// filepath.Base, which on Unix reads a Windows path as one long file name.
	// A configured shell is a string in a project's configuration and may name
	// either platform's path, whichever platform is reading it.
	base := shellName
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	switch strings.ToLower(strings.TrimSuffix(base, ".exe")) {
	case "cmd", "powershell", "pwsh":
		return nil
	default:
		return []string{"-i"}
	}
}

// credentialPrefixes are the environment variable families this surface drops.
// It is the same list the exec channel uses, because it is the same courtesy
// for the same reason.
var credentialPrefixes = []string{"DETENT_", "OPENAI_", "ANTHROPIC_", "CODEX_", "CLAUDE_"}

// terminalEnvironment removes provider and Detent credentials from the shell's
// environment and sets the variables a terminal is expected to have.
//
// Section 18.3 says what the removal is worth: a courtesy, not a boundary. The
// shell runs as the runner account, so every key that account can reach on its
// own -- a logged-in CLI's token, a credential file, a keychain -- it can still
// reach, and a person who types `cat ~/.config/...` will read it. What this
// removes is the accidental inheritance of whatever the runner process happened
// to be started with, which is the part nobody chose to hand over.
//
// TERM is set because a shell with no TERM assumes a dumb terminal and refuses
// to draw anything; COLUMNS and LINES are set because a program that reads them
// before its first SIGWINCH would otherwise start at the wrong size.
func terminalEnvironment(environ []string, worktree string, cols, rows int) []string {
	kept := make([]string, 0, len(environ)+4)
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (credentialKey(key) || terminalOwnedKey(key)) {
			continue
		}
		kept = append(kept, entry)
	}
	// DETENT_WORKSPACE is set after the scrub, not before: the DETENT_ family is
	// dropped wholesale and the worktree is the one member of it a person in
	// this shell is meant to see. It matches what the workspace hooks and the
	// exec channel already export.
	kept = append(kept,
		"DETENT_WORKSPACE="+worktree,
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
	)
	return kept
}

// terminalOwnedKey reports the variables this surface sets itself, so an
// inherited one cannot shadow the value the PTY was actually allocated with.
func terminalOwnedKey(key string) bool {
	switch strings.ToUpper(strings.TrimSpace(key)) {
	case "TERM", "COLUMNS", "LINES":
		return true
	default:
		return false
	}
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

// startCode maps a failure to start the shell onto the channel's vocabulary. A
// missing shell is unsupported rather than forbidden: nothing was refused, this
// runner simply cannot run the project's shell.
func startCode(err error) string {
	switch {
	case errors.Is(err, ErrNoPTY):
		return workspacesession.CodeUnsupported
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return workspacesession.CodeUnsupported
	default:
		return workspacesession.CodeForbidden
	}
}
