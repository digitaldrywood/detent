package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"

	"github.com/digitaldrywood/detent/internal/cli"
)

func newSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, shutdownSignals()...)
}

func notifyShutdownRequests(controller *cli.ShutdownController, cancelRoot context.CancelFunc, noticeOut io.Writer) func() {
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
			request, handled := controller.RequestInterruptKind()
			writeSignalShutdownNotice(noticeOut, request)
			if !handled {
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
			request, handled := controller.RequestInterruptKind()
			writeSignalShutdownNotice(noticeOut, request)
			if handled {
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

func writeSignalShutdownNotice(out io.Writer, request cli.ShutdownRequest) {
	if out == nil {
		return
	}
	switch request {
	case cli.ShutdownRequestDrain:
		fmt.Fprintln(out, "shutdown requested; draining sessions, press Ctrl+C again to force quit")
	case cli.ShutdownRequestForce:
		fmt.Fprintln(out, "force quit requested; interrupting sessions")
	default:
		fmt.Fprintln(out, "shutdown requested; stopping")
	}
}
