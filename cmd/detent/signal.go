package main

import (
	"context"
	"os"
	"os/signal"
	"sync"

	"github.com/digitaldrywood/detent/internal/cli"
)

func newSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, shutdownSignals()...)
}

func notifyShutdownRequests(controller *cli.ShutdownController, cancelRoot context.CancelFunc) func() {
	if controller == nil {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	first := make(chan os.Signal, 1)
	signal.Notify(first, shutdownSignals()...)

	go func() {
		defer close(done)
		select {
		case <-stop:
			signal.Stop(first)
			return
		case <-first:
			signal.Stop(first)
			if !controller.RequestInterrupt() {
				if cancelRoot != nil {
					cancelRoot()
				}
				return
			}
		}

		second := make(chan os.Signal, 1)
		signal.Notify(second, shutdownSignals()...)
		defer signal.Stop(second)
		select {
		case <-stop:
			return
		case <-second:
			if controller.RequestInterrupt() {
				return
			}
			if cancelRoot != nil {
				cancelRoot()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			signal.Stop(first)
			<-done
		})
	}
}
