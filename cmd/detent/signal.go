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

type shutdownInterruptRequester interface {
	RequestInterruptKind() (cli.ShutdownRequest, bool)
}

func notifyShutdownRequests(controller *cli.ShutdownController, cancelRoot context.CancelFunc, noticeOut io.Writer, hardExit func(int)) func() {
	if controller == nil {
		return func() {}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, shutdownSignals()...)

	go func() {
		defer close(done)
		defer signal.Stop(signals)
		for {
			select {
			case <-stop:
				return
			case <-signals:
				if handleShutdownSignal(controller, cancelRoot, noticeOut, hardExit) {
					return
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			signal.Stop(signals)
			<-done
		})
	}
}

func handleShutdownSignal(controller shutdownInterruptRequester, cancelRoot context.CancelFunc, noticeOut io.Writer, hardExit func(int)) bool {
	var request cli.ShutdownRequest
	var handled bool
	if controller != nil {
		request, handled = controller.RequestInterruptKind()
	}
	writeSignalShutdownNotice(noticeOut, request)
	if request == cli.ShutdownRequestForce {
		hardExitSignal(hardExit)
		return true
	}
	if handled {
		return false
	}
	if cancelRoot != nil {
		cancelRoot()
	}
	return true
}

func hardExitSignal(hardExit func(int)) {
	if hardExit == nil {
		hardExit = os.Exit
	}
	hardExit(cli.ExitGeneral)
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
