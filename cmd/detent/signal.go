package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/cli"
)

const signalForceExitDeadline = 5 * time.Second

func newSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, shutdownSignals()...)
}

type shutdownInterruptRequester interface {
	RequestInterruptKind() (cli.ShutdownRequest, bool)
}

type signalNoticeSuppressor interface {
	SignalNoticesSuppressed() bool
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
		var forceExitTimer *time.Timer
		defer func() {
			if forceExitTimer != nil {
				forceExitTimer.Stop()
			}
		}()
		for {
			select {
			case <-stop:
				return
			case <-signals:
				request, stopLoop := handleShutdownSignal(controller, cancelRoot, noticeOut)
				if request == cli.ShutdownRequestForce {
					if forceExitTimer == nil {
						forceExitTimer = time.AfterFunc(signalForceExitDeadline, func() {
							hardExitSignal(hardExit)
						})
					} else {
						hardExitSignal(hardExit)
						return
					}
				}
				if stopLoop {
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

func handleShutdownSignal(controller shutdownInterruptRequester, cancelRoot context.CancelFunc, noticeOut io.Writer) (cli.ShutdownRequest, bool) {
	var request cli.ShutdownRequest
	var handled bool
	if controller != nil {
		request, handled = controller.RequestInterruptKind()
	}
	slog.Default().Debug("shutdown interrupt request", "operation", "shutdown_interrupt_request", "source", "signal", "request", request.String(), "handled", handled)
	if !handled || !signalNoticesSuppressed(controller) {
		writeSignalShutdownNotice(noticeOut, request)
	}
	if handled {
		return request, false
	}
	if cancelRoot != nil {
		cancelRoot()
	}
	return request, true
}

func signalNoticesSuppressed(controller shutdownInterruptRequester) bool {
	suppressor, ok := controller.(signalNoticeSuppressor)
	return ok && suppressor.SignalNoticesSuppressed()
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
		fmt.Fprintln(out, "shutdown requested; draining sessions; press Ctrl+C again to force quit immediately")
	case cli.ShutdownRequestForce:
		fmt.Fprintln(out, "force quit requested; interrupting sessions")
	default:
		fmt.Fprintln(out, "shutdown requested; stopping")
	}
}
