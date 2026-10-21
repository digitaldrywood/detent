//go:build unix

package workspaceterminal

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// ErrNoPTY reports a platform with no pseudo-terminal. It exists on both builds
// so the shared code can name it; on Unix nothing returns it.
var ErrNoPTY = errors.New("workspaceterminal: this platform has no pseudo-terminal")

// Supported reports whether this build can open a terminal at all. The runner
// reports the terminal capability only where it is true (section 18.10): a
// runner that claimed a workspace it would then refuse frame by frame is worse
// for the person than one that was never offered it, because an unclaimed
// workspace fails with no_runner and says so.
const Supported = true

// startPTY allocates a pseudo-terminal, makes it the shell's controlling
// terminal and starts the shell on it.
//
// Setsid plus Setctty is what makes it a terminal rather than a pipe that looks
// like one. A shell without a controlling terminal has no job control: ^C would
// reach nothing, a foreground job could not be suspended, and every program
// that asks isatty would answer no and switch to its non-interactive behaviour.
// Setsid also puts the shell in a session and a process group of its own, which
// is what lets the teardown signal the whole group.
func startPTY(cmd *exec.Cmd, cols, rows int, isolation, worktree string) (*os.File, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	master, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tty.Close() }()    //nolint:errcheck // Best effort: only the subprocess retains the slave.
	defer func() { _ = master.Close() }() //nolint:errcheck // Best effort: the duplicate owns the returned master.

	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil { // #nosec G115 -- bounded by NormalizeTerminalSize.
		return nil, err
	}
	// Darwin's PTY master is a blocking os.NewFile. Duplicate it before
	// wrapping a nonblocking descriptor so Go's poller can interrupt reads
	// on Close. The original wrapper must retain ownership of its own fd.
	fd, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate terminal master: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd) //nolint:errcheck // Best effort cleanup before reporting the setup failure.
		return nil, fmt.Errorf("make terminal master nonblocking: %w", err)
	}
	file := os.NewFile(uintptr(fd), master.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	var confirmation *os.File
	var ready *os.File
	if isolation == workspacesession.IsolationSandbox {
		confirmation, ready, err = os.Pipe()
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		defer func() {
			if err := confirmation.Close(); err != nil {
				slog.Debug("terminal confirmation descriptor close", "error", err)
			}
		}()
		defer func() {
			if err := ready.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				slog.Debug("terminal readiness descriptor close", "error", err)
			}
		}()
		cmd.ExtraFiles = []*os.File{ready}
		if err := sandboxCommand(cmd, worktree, tty.Name()); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	if err := cmd.Start(); err != nil {
		_ = file.Close() //nolint:errcheck // Best effort cleanup before reporting the start failure.
		return nil, err
	}
	if confirmation != nil {
		err := errors.Join(ready.Close(), confirmation.SetReadDeadline(time.Now().Add(5*time.Second)))
		var marker [1]byte
		if err == nil {
			_, err = io.ReadFull(confirmation, marker[:])
		}
		if err != nil || marker[0] != 1 {
			closeErr := file.Close()
			groupErr := kill(cmd)
			processErr := cmd.Process.Kill()
			waited := make(chan struct{})
			go func() {
				if err := cmd.Wait(); err != nil {
					slog.Debug("terminal launch cleanup wait", "error", err)
				}
				close(waited)
			}()
			if groupErr != nil && processErr != nil && !errors.Is(processErr, os.ErrProcessDone) {
				return nil, fmt.Errorf("%w: launch cleanup: %w", ErrSandboxIsolation, errors.Join(groupErr, processErr, closeErr))
			}
			select {
			case <-waited:
			case <-time.After(KillGrace):
				return nil, fmt.Errorf("%w: launch cleanup did not finish", ErrSandboxIsolation)
			}
			return nil, fmt.Errorf("%w: launch was not confirmed", errors.Join(ErrSandboxIsolation, closeErr))
		}
	}
	return file, nil
}

// setWindowSize applies a new window to the PTY, which is what raises SIGWINCH
// in the foreground process group.
func setWindowSize(file *os.File, cols, rows int) error {
	return pty.Setsize(file, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}) // #nosec G115 -- bounded by NormalizeTerminalSize.
}

// hangup asks the shell's process group to leave, the way a real terminal does
// when its window closes.
func hangup(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGHUP) }

// kill ends the shell's process group unconditionally.
func kill(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGKILL) }

// signalGroup signals the whole process group rather than the shell alone.
//
// Setsid above made the shell a group leader whose group id is its pid, so the
// negated pid names the group. A build, a server or an editor the person started
// is in that group, and signalling only the shell would leave them holding the
// worktree with nobody left to stop them.
func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("signal terminal group %d with %v: %w", cmd.Process.Pid, signal, err)
	}
	return nil
}

// groupAlive reports whether any process remains in the group led by pid.
func groupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// exitSignal names the signal that ended the shell, or the empty string when it
// exited on its own. The name is the signal's own ("SIGHUP") rather than Go's
// prose for it, because it is carried to a person as the reason a terminal has
// no exit code, and a reason a reader can search for is worth more than one
// they cannot.
func exitSignal(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	if name := unix.SignalName(status.Signal()); name != "" {
		return name
	}
	return status.Signal().String()
}
