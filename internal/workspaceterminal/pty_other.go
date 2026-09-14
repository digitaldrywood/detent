//go:build !unix

package workspaceterminal

import (
	"errors"
	"os"
	"os/exec"
)

// ErrNoPTY reports a platform with no pseudo-terminal.
//
// Windows has a pseudo-console (ConPTY) rather than a pseudo-terminal, and it is
// a different API with a different lifecycle and a different teardown story.
// Section 18.3's contract is written in terms this platform does not have --
// SIGHUP, a process group, a controlling terminal -- so a Windows terminal is a
// port rather than a build tag, and pretending otherwise here would produce a
// surface that starts and then cannot be stopped. A runner on this platform
// reports no terminal capability, the workspace is never claimed for one, and
// the card stays disabled with the reason, which is section 18.10's own answer
// for a capability a runner does not serve.
var ErrNoPTY = errors.New("workspaceterminal: this platform has no pseudo-terminal")

// Supported reports whether this build can open a terminal at all.
const Supported = false

func startPTY(*exec.Cmd, int, int) (*os.File, error) { return nil, ErrNoPTY }

func setWindowSize(*os.File, int, int) error { return ErrNoPTY }

func hangup(*exec.Cmd) error { return ErrNoPTY }

func kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// exitSignal answers nothing off Unix. Windows has no signals: a process that is
// terminated there ends with an exit code and the exit code is the whole story,
// so reporting an invented signal name would be reporting a fact the platform
// does not have.
func exitSignal(*os.ProcessState) string { return "" }
