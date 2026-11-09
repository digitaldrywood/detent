package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func testRunnerSessionFinalizationRecovery(t *testing.T, f, other runnerPGFixture, index int) {
	t.Helper()
	t.Run(fmt.Sprintf("%s/runner_%d/legacy_finalization", f.organization, index), func(t *testing.T) {
		native, ctx := f.client(t, index)
		owner := api.NewRunnerSessions(native)
		started := time.Now().UTC()
		id, err := owner.StartSession(ctx, store.SessionStart{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), StartedAt: started, AgentRole: "code"})
		if err != nil {
			t.Fatal(err)
		}
		missingPhase := &sessionmodel.Finalization{SessionID: id, Finish: store.SessionFinish{CompletedAt: started.Add(time.Second), FinalState: "completed"}, Usage: store.UsageEvent{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), Outcome: "completed"}}
		missingKey, err := missingPhase.Key("finish")
		if err != nil {
			t.Fatal(err)
		}
		if err := native.Transport().Request(ctx, http.MethodPost, native.Scope()+"/runner-sessions", runnersession.Mutation{SessionID: id, ExpectedRevision: 1, LeaseID: f.leases[index].ID, FencingToken: f.leases[index].FencingToken, Key: missingKey, Finalization: missingPhase, Finish: &missingPhase.Finish}, nil); err == nil || !strings.Contains(err.Error(), "session finalization phase differs") {
			t.Fatalf("session finalization omitted required phase: %v", err)
		}
		if err := owner.FinishSession(ctx, id, store.SessionFinish{CompletedAt: started.Add(time.Second), FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.RecordUsageEvent(ctx, store.UsageEvent{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		if err := api.NewRunnerSessions(native).ReconcileSessionFinalization(ctx, id, nil); err == nil {
			t.Fatal("legacy receipt bypassed missing durable phase")
		}
		if _, err := owner.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		if err := api.NewRunnerSessions(native).ReconcileSessionFinalization(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
			history, err := r.(runnersession.Reader).RunnerSessionHistory(t.Context(), "prj_runner", id, 0, 8)
			if err != nil {
				return err
			}
			if len(history) != 4 {
				t.Fatalf("legacy proof duplicated finalization: %d", len(history))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.SaveWorkflowActivityProfile(ctx, 0, store.WorkflowPhaseEvent{SessionID: id, PhaseName: "profile"}, workflowmetrics.ActivityProfile{Schema: 1, SessionID: id, Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		if err := api.NewRunnerSessions(native).ReconcileSessionFinalization(ctx, id, nil); err != nil {
			t.Fatalf("activity profile hid complete legacy finalization: %v", err)
		}
	})
	for _, operation := range []string{"finish", "usage", "phase"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/runner_%d/%s/committed_%t", f.organization, index, operation, committed), func(t *testing.T) {
				native, ctx := f.client(t, index)
				started := time.Now()
				sessions := api.NewRunnerSessions(native)
				id, err := sessions.StartSession(ctx, store.SessionStart{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), StartedAt: started, RequestedModel: string(f.organization), AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code"})
				if err != nil {
					t.Fatal(err)
				}
				value := &sessionmodel.Finalization{SessionID: id,
					Finish: store.SessionFinish{CompletedAt: started.Add(time.Second), Turns: 1, InputTokens: 100, OutputTokens: 10, TotalTokens: 110, FinalState: "completed", Model: string(f.organization)},
					Usage:  store.UsageEvent{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), InputTokens: 100, OutputTokens: 10, TotalTokens: 110, Outcome: "completed", Model: string(f.organization)},
					Phase:  &store.WorkflowPhaseEvent{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed", Turns: 1, InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
				}
				var failed atomic.Bool
				fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					selected := false
					if r.Method == http.MethodPost {
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						if err := r.Body.Close(); err != nil {
							t.Error(err)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(raw))
						var mutation runnersession.Mutation
						if err := json.Unmarshal(raw, &mutation); err != nil {
							t.Error(err)
							return
						}
						selected = mutation.SessionID == id && (operation == "finish" && mutation.Finish != nil || operation == "usage" && mutation.Usage != nil || operation == "phase" && mutation.Phase != nil) && failed.CompareAndSwap(false, true)
					}
					if !selected {
						f.service.Handler().ServeHTTP(w, r)
						return
					}
					if !committed {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						if _, err := w.Write([]byte(`{"code":"unavailable"}`)); err != nil {
							t.Error(err)
						}
						return
					}
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, r)
					if response.Code != http.StatusOK {
						t.Errorf("finalization did not commit before response loss: status=%d", response.Code)
					}
					w.Header().Set("Content-Length", strconv.Itoa(response.Body.Len()+1))
					w.WriteHeader(response.Code)
					if _, err := w.Write(response.Body.Bytes()); err != nil {
						t.Error(err)
					}
				}))
				t.Cleanup(fault.Close)
				transport, err := api.New(api.Config{URL: fault.URL, TokenSource: func() string { return f.tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
				if err != nil {
					t.Fatal(err)
				}
				uncertain, err := transport.Native(f.organization, "prj_runner")
				if err != nil {
					t.Fatal(err)
				}
				if err := api.NewRunnerSessions(uncertain).ReconcileSessionFinalization(ctx, id, value); err == nil || !failed.Load() {
					t.Fatalf("finalization failure not reported: error=%v fault=%t", err, failed.Load())
				}
				readHistory := func() []runnersession.History {
					t.Helper()
					var history []runnersession.History
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						history, err = r.(runnersession.Reader).RunnerSessionHistory(t.Context(), "prj_runner", id, 0, 8)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return history
				}
				wantBefore := map[string]int{"finish": 1, "usage": 2, "phase": 3}[operation]
				if committed {
					wantBefore++
				}
				if history := readHistory(); len(history) != wantBefore {
					t.Fatalf("wrong durable prefix after failure: entries=%d want=%d", len(history), wantBefore)
				}
				restarted, restartedCtx := f.client(t, index)
				if err := api.NewRunnerSessions(restarted).ReconcileSessionFinalization(restartedCtx, id, value); err != nil {
					t.Fatal(err)
				}
				var snapshot runnersession.Snapshot
				if err := restarted.Transport().Request(restartedCtx, http.MethodGet, restarted.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.Finalization == nil || !snapshot.Finalization.Finished || !snapshot.Finalization.UsageRecorded || !snapshot.Finalization.PhaseRecorded || snapshot.Finish == nil || snapshot.ID != id || snapshot.Finalization.Input.SessionID != id || snapshot.Start.RequestedModel != string(f.organization) {
					t.Fatalf("restarted finalization lost immutable identity or durable stages: %+v", snapshot)
				}
				counts := map[string]int{}
				saved := map[string]runnersession.Mutation{}
				for _, entry := range readHistory() {
					raw, err := f.service.runDetailStore().ReadDetail(t.Context(), string(f.organization), entry.Reference)
					if err != nil {
						t.Fatal(err)
					}
					var mutation runnersession.Mutation
					if err := json.Unmarshal(raw, &mutation); err != nil {
						t.Fatal(err)
					}
					switch {
					case mutation.Start != nil:
						counts["start"]++
					case mutation.Finish != nil:
						counts["finish"]++
						saved["finish"] = mutation
					case mutation.Usage != nil:
						counts["usage"]++
						saved["usage"] = mutation
					case mutation.Phase != nil:
						counts["phase"]++
						saved["phase"] = mutation
					}
				}
				for _, stage := range []string{"start", "finish", "usage", "phase"} {
					if counts[stage] != 1 {
						t.Fatalf("duplicate or missing durable %s: %v", stage, counts)
					}
				}
				for _, stage := range []string{"finish", "usage", "phase"} {
					mutation := saved[stage]
					mutation.SessionID, mutation.Finalization, mutation.ExpectedRevision = id, value, snapshot.Revision
					mutation.LeaseID, mutation.FencingToken = f.leases[index].ID, f.leases[index].FencingToken
					var replay runnersession.Snapshot
					if err := restarted.Transport().Request(restartedCtx, http.MethodPost, restarted.Scope()+"/runner-sessions", mutation, &replay); err != nil {
						t.Fatal(err)
					}
					if replay.Revision != snapshot.Revision {
						t.Fatalf("committed replay wrote another revision: got=%d want=%d", replay.Revision, snapshot.Revision)
					}
				}
				conflict := runnersession.Mutation{SessionID: id, Finalization: value, Usage: &value.Usage, ExpectedRevision: snapshot.Revision, LeaseID: f.leases[index].ID, FencingToken: f.leases[index].FencingToken, Key: saved["finish"].Key}
				if err := restarted.Transport().Request(restartedCtx, http.MethodPost, restarted.Scope()+"/runner-sessions", conflict, nil); err == nil {
					t.Fatal("finalization replay bypassed mutation hash conflict")
				}
				changed := *value
				changed.Usage.InputTokens++
				if err := api.NewRunnerSessions(restarted).ReconcileSessionFinalization(restartedCtx, id, &changed); err == nil {
					t.Fatal("changed immutable finalization was accepted")
				}
				otherNative, otherCtx := other.client(t, index)
				if err := api.NewRunnerSessions(otherNative).ReconcileSessionFinalization(otherCtx, id, value); err == nil {
					t.Fatal("foreign tenant reconciled session finalization")
				}
				if history := readHistory(); len(history) != 4 {
					t.Fatalf("reconciliation duplicated durable effects: %d", len(history))
				}
			})
		}
	}
}
