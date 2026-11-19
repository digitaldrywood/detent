package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestStopAfterLocalWorkerExit(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		registered, incomplete, terminationFailed bool
	}{
		{name: "open session"},
		{name: "reaped provider", registered: true},
		{name: "incomplete inventory", incomplete: true},
		{name: "unverified exit", registered: true, terminationFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Stop after local worker exit")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			ctx, lease := t.Context(), e.claim.lease
			if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			sessions := api.NewRunnerSessions(h.native)
			identity := store.WorkerProcessIdentity{PID: 42, GroupID: 42, StartedAt: time.Now().UTC()}
			reaped := 0
			local, err := runner.NewRunner(runner.Dependencies{Workspace: &workspace.LocalGit{}, AgentBackend: struct{ runner.AgentBackend }{}, Store: sessions,
				ReapWorkerProcess: func(_ context.Context, got procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
					reaped++
					if got.PID != identity.PID || got.GroupID != identity.GroupID || !got.StartedAt.Equal(identity.StartedAt) {
						t.Fatalf("unexpected process identity %+v", got)
					}
					if tc.terminationFailed {
						return procgroup.TerminationOutcomeStaleIdentity, nil
					}
					return procgroup.TerminationOutcomeTerminated, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			callbacks := local.LocalRunCallbacks(runner.RunRequest{Issue: issue, Execution: e})
			start, err := json.Marshal(store.SessionStart{IssueID: string(lease.WorkItemID), StartedAt: time.Now().UTC(), AgentRole: runner.RoleCode})
			if err != nil {
				t.Fatal(err)
			}
			id, err := callbacks.Handle(ctx, "store.StartSession", []json.RawMessage{start})
			if err != nil {
				t.Fatal(err)
			}
			if tc.registered {
				if err := sessions.UpdateSessionWorkerProcess(sessions.BindExecution(ctx, e), id.(int64), store.WorkerProcessRegistration{WorkerProcessIdentity: identity}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.native.ValidateLease(ctx, lease); err != nil {
				t.Fatal(err)
			}
			closed, inventory := 0, 0
			requests := fixtureClient(h.native.Transport()).requests
			next := requests.Load()
			requests.Store(executionRoundTrip(func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "/runner-sessions") {
					t.Errorf("post-stop session request %s %s", req.Method, req.URL.Path)
				}
				if strings.HasSuffix(req.URL.Path, "/runner-processes") {
					inventory++
					if tc.incomplete {
						return &http.Response{Request: req, StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"more":true}`))}, nil
					}
				}
				return next.RoundTrip(req)
			}))
			if err := e.KeepWorker(func(closeCtx context.Context) error {
				closed++
				if _, err := sessions.StartSession(sessions.BindExecution(closeCtx, e), store.SessionStart{}); !errors.Is(err, runner.ErrOperatorStopped) {
					t.Errorf("post-stop start error %v", err)
				}
				return callbacks.Finish(closeCtx, time.Now().UTC())
			}, nil); err != nil {
				t.Fatal(err)
			}
			stopErr := h.scheduler.ReleaseClaim(ctx, issue.ID, runner.FinalStateOperatorStopped)
			_, leaseErr := h.native.ValidateLease(ctx, lease)
			wantRelease := !tc.incomplete && !tc.terminationFailed
			wantReaps := 0
			if tc.registered {
				wantReaps = 1
			}
			if closed != 1 || inventory != 1 || reaped != wantReaps {
				t.Fatalf("worker closes %d, inventory reads %d, process checks %d", closed, inventory, reaped)
			}
			testsupport.NativeChangeWrite(t, h.config, func(w tenantstore.Writer) error {
				a, err := w.Attempt(ctx, e.data.AttemptID)
				if err != nil {
					return err
				}
				l, err := w.Lease(ctx, string(lease.ID))
				if err != nil {
					return err
				}
				if a.Status != "interrupted" || a.Sequence != 2 {
					t.Errorf("attempt %s at sequence %d, want interrupted at 2", a.Status, a.Sequence)
				}
				if wantRelease {
					var refusal *APIError
					if stopErr != nil || l.ReleasedAt == nil || !errors.As(leaseErr, &refusal) || refusal.Status != http.StatusConflict || !nativeLeaseLost(leaseErr) {
						t.Errorf("stop %v, released at %v, original fence validation %v", stopErr, l.ReleasedAt, leaseErr)
					}
				} else if stopErr == nil || l.ReleasedAt != nil || leaseErr != nil {
					t.Errorf("unverified process exit: stop %v, released at %v, original fence validation %v", stopErr, l.ReleasedAt, leaseErr)
				}
				return nil
			})
		})
	}
}
