//go:build !unix

package workspaceexec

import "os"

// exitSignal answers nothing off Unix. Windows has no signals: a process that
// is terminated there ends with an exit code and the exit code is the whole
// story, so reporting an invented signal name would be reporting a fact the
// platform does not have.
func exitSignal(*os.ProcessState) string { return "" }
