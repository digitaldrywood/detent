//go:build unix

package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func withSSHWorkerSignals(ctx context.Context) (context.Context, context.CancelFunc) {
	// sshd can deliver hangup or close stdout before stdin EOF is observed.
	// Both must enter the existing runner teardown instead of killing it.
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
}
