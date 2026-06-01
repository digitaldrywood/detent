package main

import (
	"context"
	"os/signal"
)

func newSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, shutdownSignals()...)
}
