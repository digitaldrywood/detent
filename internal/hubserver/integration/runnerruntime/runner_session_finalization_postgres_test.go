package runnerruntime_test

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
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func testRunnerSessionFinalizationRecovery(t *testing.T, f, other hubfixture.LeasedRunners, index int) {
	t.Helper()
	t.Run(fmt.Sprintf("%s/runner_%d/legacy_finalization", f.Organization, index), func(t *testing.T) {
		native, ctx := f.Client(t, index)
		owner := api.NewRunnerSessions(native)
		started := time.Now().UTC()
		id, err := owner.StartSession(ctx, store.SessionStart{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), StartedAt: started, AgentRole: "code"})
		if err != nil {
			t.Fatal(err)
		}
		missingPhase := &sessionmodel.Finalization{SessionID: id, Finish: store.SessionFinish{CompletedAt: started.Add(time.Second), FinalState: "completed"}, Usage: store.UsageEvent{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), Outcome: "completed"}}
		missingKey, err := missingPhase.Key("finish")
		if err != nil {
			t.Fatal(err)
		}
		if err := native.Transport().Request(ctx, http.MethodPost, native.Scope()+"/runner-sessions", runnersession.Mutation{SessionID: id, ExpectedRevision: 1, LeaseID: f.Leases[index].ID, FencingToken: f.Leases[index].FencingToken, Key: missingKey, Finalization: missingPhase, Finish: &missingPhase.Finish}, nil); err == nil || !strings.Contains(err.Error(), "session finalization phase differs") {
			t.Fatalf("session finalization omitted required phase: %v", err)
		}
		if err := owner.FinishSession(ctx, id, store.SessionFinish{CompletedAt: started.Add(time.Second), FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.RecordUsageEvent(ctx, store.UsageEvent{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		if err := api.NewRunnerSessions(native).ReconcileSessionFinalization(ctx, id, nil); err == nil {
			t.Fatal("legacy receipt bypassed missing durable phase")
		}
		if _, err := owner.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		if err := api.NewRunnerSessions(native).ReconcileSessionFinalization(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.Hub.Config.TenantRepository.ReadTenant(t.Context(), f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("%s/runner_%d/finalization_committed_%t", f.Organization, index, committed), func(t *testing.T) {
			native, ctx := f.Client(t, index)
			started := time.Now()
			sessions := api.NewRunnerSessions(native)
			id, err := sessions.StartSession(ctx, store.SessionStart{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), StartedAt: started, RequestedModel: string(f.Organization), AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code"})
			if err != nil {
				t.Fatal(err)
			}
			value := &sessionmodel.Finalization{SessionID: id,
				Finish: store.SessionFinish{CompletedAt: started.Add(time.Second), Turns: 1, InputTokens: 100, OutputTokens: 10, TotalTokens: 110, FinalState: "completed", Model: string(f.Organization)},
				Usage:  store.UsageEvent{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), InputTokens: 100, OutputTokens: 10, TotalTokens: 110, Outcome: "completed", Model: string(f.Organization)},
				Phase:  &store.WorkflowPhaseEvent{ProjectID: "local-project", IssueID: string(f.Leases[index].WorkItemID), SessionID: id, StartedAt: started, FinishedAt: started.Add(time.Second), PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed", Turns: 1, InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
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
					selected = mutation.SessionID == id && failed.CompareAndSwap(false, true)
					if selected && (mutation.Finish == nil || mutation.Usage == nil || mutation.Phase == nil) {
						t.Error("finalization did not carry finish, usage and phase together")
					}
				}
				if !selected {
					f.Hub.Handler().ServeHTTP(w, r)
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
				f.Hub.Handler().ServeHTTP(response, r)
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
			transport, err := api.New(api.Config{URL: fault.URL, TokenSource: func() string { return f.Tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
			if err != nil {
				t.Fatal(err)
			}
			uncertain, err := transport.Native(f.Organization, "prj_runner")
			if err != nil {
				t.Fatal(err)
			}
			if err := api.NewRunnerSessions(uncertain).ReconcileSessionFinalization(ctx, id, value); err == nil || !failed.Load() {
				t.Fatalf("finalization failure not reported: error=%v fault=%t", err, failed.Load())
			}
			readHistory := func() []runnersession.History {
				t.Helper()
				var history []runnersession.History
				if err := f.Hub.Config.TenantRepository.ReadTenant(t.Context(), f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					history, err = r.(runnersession.Reader).RunnerSessionHistory(t.Context(), "prj_runner", id, 0, 8)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return history
			}
			wantBefore := 1
			if committed {
				wantBefore++
			}
			if history := readHistory(); len(history) != wantBefore {
				t.Fatalf("wrong durable prefix after failure: entries=%d want=%d", len(history), wantBefore)
			}
			restarted, restartedCtx := f.Client(t, index)
			if err := api.NewRunnerSessions(restarted).ReconcileSessionFinalization(restartedCtx, id, value); err != nil {
				t.Fatal(err)
			}
			var snapshot runnersession.Snapshot
			if err := restarted.Transport().Request(restartedCtx, http.MethodGet, restarted.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Finalization == nil || !snapshot.Finalization.Finished || !snapshot.Finalization.UsageRecorded || !snapshot.Finalization.PhaseRecorded || snapshot.Finish == nil || snapshot.ID != id || snapshot.Finalization.Input.SessionID != id || snapshot.Start.RequestedModel != string(f.Organization) {
				t.Fatalf("restarted finalization lost immutable identity or durable stages: %+v", snapshot)
			}
			history := readHistory()
			if len(history) != 2 {
				t.Fatalf("start and finalization wrote %d revisions, want 2", len(history))
			}
			raw, err := f.Hub.Config.RunDetails.ReadDetail(t.Context(), string(f.Organization), history[1].Reference)
			if err != nil {
				t.Fatal(err)
			}
			var mutation runnersession.Mutation
			if err := json.Unmarshal(raw, &mutation); err != nil {
				t.Fatal(err)
			}
			if mutation.Finish == nil || mutation.Usage == nil || mutation.Phase == nil || !mutation.Finalization.Equal(*value) {
				t.Fatal("one finalization revision did not retain the immutable inputs")
			}
			mutation.ExpectedRevision = snapshot.Revision
			var replay runnersession.Snapshot
			if err := restarted.Transport().Request(restartedCtx, http.MethodPost, restarted.Scope()+"/runner-sessions", mutation, &replay); err != nil {
				t.Fatal(err)
			}
			if replay.Revision != snapshot.Revision {
				t.Fatalf("committed replay wrote another revision: got=%d want=%d", replay.Revision, snapshot.Revision)
			}
			conflict := runnersession.Mutation{SessionID: id, Finalization: value, Usage: &value.Usage, ExpectedRevision: snapshot.Revision, LeaseID: f.Leases[index].ID, FencingToken: f.Leases[index].FencingToken, Key: mutation.Key}
			if err := restarted.Transport().Request(restartedCtx, http.MethodPost, restarted.Scope()+"/runner-sessions", conflict, nil); err == nil {
				t.Fatal("finalization replay bypassed mutation hash conflict")
			}
			changed := *value
			changed.Usage.InputTokens++
			if err := api.NewRunnerSessions(restarted).ReconcileSessionFinalization(restartedCtx, id, &changed); err == nil {
				t.Fatal("changed immutable finalization was accepted")
			}
			otherNative, otherCtx := other.Client(t, index)
			if err := api.NewRunnerSessions(otherNative).ReconcileSessionFinalization(otherCtx, id, value); err == nil {
				t.Fatal("foreign tenant reconciled session finalization")
			}
			if history := readHistory(); len(history) != 2 {
				t.Fatalf("reconciliation duplicated durable effects: %d", len(history))
			}
		})
	}
}

func TestPostgresHostSessionRecordsOnlyBoundaries(t *testing.T) {
	f := hubfixture.NewLeasedRunners(t)
	for index, lease := range f.Leases {
		client, ctx := f.Client(t, index)
		sessions := api.NewRunnerSessions(client)
		started := time.Now().UTC()
		worker := store.WorkerProcessRegistration{
			WorkerProcessIdentity: store.WorkerProcessIdentity{PID: 10000 + index, GroupID: 10000 + index, StartedAt: started, StartKey: "example-identity"},
			CleanupRoot:           t.TempDir(),
		}
		id, err := sessions.StartSession(ctx, store.SessionStart{
			IssueID: string(lease.WorkItemID), StartedAt: started, AgentBackendKind: "host",
			AgentRole: store.WorkerProcessRoleHostValidation, Worker: &worker,
		})
		if err != nil {
			t.Fatal(err)
		}
		path := client.Scope() + "/runner-sessions/" + strconv.FormatInt(id, 10)
		var running runnersession.Snapshot
		if err := client.Transport().Request(ctx, http.MethodGet, path, nil, &running); err != nil {
			t.Fatal(err)
		}
		if running.Revision != 1 || running.Worker == nil || running.Worker.PID != worker.PID || running.Reaped != nil {
			t.Fatalf("start did not record its fenced process at the first revision: %+v", running)
		}
		finish := time.Now().UTC()
		if err := sessions.FinishSession(ctx, id, store.SessionFinish{
			CompletedAt: finish, FinalState: "completed",
			Reap: &store.WorkerProcessReap{ReapedAt: finish, Outcome: "already_exited", Reason: "host_validation"},
		}); err != nil {
			t.Fatal(err)
		}
		var stopped runnersession.Snapshot
		if err := client.Transport().Request(ctx, http.MethodGet, path, nil, &stopped); err != nil {
			t.Fatal(err)
		}
		if stopped.Revision != 2 || stopped.Finish == nil || stopped.Reaped == nil || !stopped.Reaped.ReapedAt.Equal(finish) {
			t.Fatalf("finish did not retain reap evidence in the second revision: %+v", stopped)
		}
	}
}
