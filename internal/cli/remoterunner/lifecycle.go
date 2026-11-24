package remoterunner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type Lifecycle struct {
	routingState string
	mu           sync.Mutex
	report       runnerauth.LifecycleReport
	send         func(context.Context, runnerauth.LifecycleReport) error
}

func StartLifecycle(ctx context.Context, settings globalconfig.HubClient, build buildinfo.Info, version string) (*Lifecycle, error) {
	settings = settings.Normalized()
	client, err := api.New(api.Config{URL: settings.URL, IdentityFile: settings.IdentityFile, HTTPClient: &http.Client{Timeout: settings.RequestTimeout()}})
	if err != nil {
		return nil, err
	}
	if build.Version == "" {
		build.Version = version
	}
	build = buildinfo.Normalize(build)
	owner := NewLifecycle(build.Version, build.Commit, client.ReportRunnerLifecycle)
	if err := owner.Report(ctx, runnerauth.LifecycleStarting, "", ""); err != nil {
		return nil, err
	}
	return owner, nil
}

func (l *Lifecycle) ReportStartupFailure(ctx context.Context) {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := l.Report(ctx, runnerauth.LifecycleStartFailed, "", ""); err != nil {
		slog.Warn("report runner startup failure", "error", err)
	}
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

func (l *Lifecycle) Shutdown(ctx context.Context, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.report.State == runnerauth.LifecycleShuttingDown && l.report.Reason == reason {
		return nil
	}
	var target string
	if reason == "update" {
		target = l.report.TargetVersion
	}
	return l.reportLocked(ctx, runnerauth.LifecycleShuttingDown, reason, target)
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
		var refusal *api.APIError
		if errors.As(err, &refusal) && refusal.Status == http.StatusConflict {
			l.report.Sequence = next.Sequence
		}
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
