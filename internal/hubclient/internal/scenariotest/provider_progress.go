package scenariotest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCurrentClaimRunner struct {
	started chan nativeCompletionRun
	checked bool
}

func (r *nativeCurrentClaimRunner) CheckProgress(context.Context, runner.RunRequest, progresscheck.Request) (progresscheck.Decision, error) {
	r.checked = true
	return progresscheck.Decision{}, errors.New("pre-dispatch progress should not run")
}

func (r *nativeCurrentClaimRunner) Run(ctx context.Context, req runner.RunRequest) (runner.RunResult, error) {
	err := req.Execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"})
	r.started <- nativeCompletionRun{request: req, err: err}
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}

func RunNativeRetryUsesCurrentClaim(t *testing.T, factories Constructors) {
	t.Parallel()
	h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done", "Todo"}},
		{Name: "Done", Terminal: true},
	}, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	issue := h.createInProgress(t, "Retry under the admitted lease")
	h.claim(t, issue.ID)
	prior := h.scheduler.RunExecution(issue.ID)
	if err := prior.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := prior.Finish(t.Context(), "failed"); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	requests := fixtureClient(h.scheduler.Transport()).requests
	transport := requests.Load()
	var investigations atomic.Int64
	requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(request.URL.Path, "/progress") {
			return transport.RoundTrip(request)
		}
		switch request.Method {
		case http.MethodGet:
			query, _ := url.ParseQuery(request.URL.RawQuery)
			if query.Get("round") == "" && query.Get("attempt") == "" {
				investigations.Add(1)
			}
		case http.MethodPost:
			var publication progresscheck.Publication
			if request.Body != nil {
				defer request.Body.Close()
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				request = request.Clone(request.Context())
				request.Body = io.NopCloser(bytes.NewReader(body))
				if err := json.Unmarshal(body, &publication); err != nil {
					t.Errorf("decode progress publication: %v", err)
				}
			}
			if publication.RoundID == "" && publication.AttemptID == "" {
				investigations.Add(1)
			}
		}
		return transport.RoundTrip(request)
	}))
	issue = h.claim(t, issue.ID)
	original := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
	backend := &nativeCurrentClaimRunner{started: make(chan nativeCompletionRun, 1)}
	source := &nativeCompletionScheduling{Scheduler: h.scheduler, issue: issue, released: make(chan error, 1)}
	orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: source, Runner: backend, Logger: slog.New(slog.NewTextHandler(t.Output(), nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
		if calls := investigations.Load(); calls != 0 {
			t.Errorf("retry made %d investigation requests, want none", calls)
		}
	}()
	select {
	case started := <-backend.started:
		if started.err != nil || started.request.Execution != original || backend.checked {
			t.Fatalf("retry did not start on the admitted execution: check=%t error=%v", backend.checked, started.err)
		}
	case <-testenv.WaitContext(t, ctx).Done():
		t.Fatal("retry did not start")
	}
}
