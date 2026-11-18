package remoterunner

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type Lifecycle struct {
	routingState string
	mu           sync.Mutex
	report       runnerauth.LifecycleReport
	send         func(context.Context, runnerauth.LifecycleReport) error
}

func NewLifecycle(version, revision string, send func(context.Context, runnerauth.LifecycleReport) error) *Lifecycle {
	return &Lifecycle{report: runnerauth.LifecycleReport{ProcessID: rand.Text(), Version: version, BuildRevision: revision}, send: send}
}

func (l *Lifecycle) Report(ctx context.Context, state runnerauth.LifecycleState, reason, target string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.reportLocked(ctx, state, reason, target); err != nil {
		return err
	}
	if state == runnerauth.LifecycleReady && (l.routingState == "draining" || l.routingState == "disabled") {
		return l.reportLocked(ctx, runnerauth.LifecycleDraining, "", "")
	}
	return nil
}

func (l *Lifecycle) reportLocked(ctx context.Context, state runnerauth.LifecycleState, reason, target string) error {
	next := l.report
	next.Sequence++
	next.State, next.Reason, next.TargetVersion = state, reason, target
	next.ErrorCode = ""
	if state == runnerauth.LifecycleStartFailed {
		next.ErrorCode = "startup_error"
	}
	if err := l.send(ctx, next); err != nil {
		return fmt.Errorf("report runner lifecycle: %w", err)
	}
	l.report = next
	return nil
}

func (l *Lifecycle) ObserveRouting(ctx context.Context, state string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.routingState = state
	if l.report.State != runnerauth.LifecycleReady && l.report.State != runnerauth.LifecycleDraining {
		return nil
	}
	next := runnerauth.LifecycleReady
	if state == "draining" || state == "disabled" {
		next = runnerauth.LifecycleDraining
	}
	if next == l.report.State {
		return nil
	}
	return l.reportLocked(ctx, next, "", "")
}
