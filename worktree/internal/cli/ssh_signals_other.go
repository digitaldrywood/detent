//go:build !unix

package cli

import (
	"context"
	"os"
	"os/signal"
)

func withSSHWorkerSignals(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt)
}
