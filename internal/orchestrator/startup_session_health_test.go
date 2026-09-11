package orchestrator_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestStartupSessionsAppearInStateAndHealth(t *testing.T) {
	for _, validator := range []bool{false, true} {
		name := "orphan resume"
		if validator {
			name = "validator after restart"
		}
		t.Run(name, func(t *testing.T) {
			db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "detent.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			issue := testIssue("issue-2486", "digitaldrywood/detent#2486", "Rework")
			if validator {
				issue.PullRequest = &connector.PullRequest{Number: 2487, URL: "https://github.com/digitaldrywood/detent/pull/2487", HeadSHA: "head", State: "OPEN", CIStatus: "success"}
			}
			// The previous process died with a code session still recorded as running.
			attempt, err := db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: "Rework", StartedAt: time.Now().Add(-time.Minute), LeaseExpiresAt: time.Now().Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			prior, err := db.StartSession(t.Context(), store.SessionStart{WorkAttemptID: attempt, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, StartedAt: time.Now().Add(-time.Minute), AgentBackendKind: "codex", AgentRole: "code", ProviderThreadID: "old-thread"})
			if err != nil {
				t.Fatal(err)
			}
			tracker := memory.New(memory.Config{Issues: []connector.Issue{issue}})
			activeStates := []string{"Todo", "In Progress", "Rework", "Merging"}
			if validator {
				activeStates = []string{"Todo", "In Progress", "Merging"}
			}
			runner := &startupHealthRunner{db: db, started: make(chan startupHealthSession, 2)}
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "detent"}, PollInterval: time.Hour, ActiveStates: activeStates, ObservedStates: []string{"Rework"}, ResumeOrphanedSessions: !validator, AutoPromote: orchestrator.AutoPromoteConfig{Enabled: validator, SourceState: "Rework", Gate: gate.Config{Kind: gate.KindCommand, Validator: gate.ValidatorConfig{Enabled: validator}}}}, orchestrator.Dependencies{Connector: tracker, Runner: runner, WorkAttempts: db})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- orch.Run(ctx) }()
			defer func() { cancel(); <-done }()
			var session startupHealthSession
			select {
			case session = <-runner.started:
			case <-time.After(10 * time.Second):
				t.Fatal("startup did not start worker")
			}
			if session.err != nil {
				t.Fatal(session.err)
			}
			if session.validator != validator {
				t.Fatalf("validator = %t, want %t", session.validator, validator)
			}
			if !validator && session.resumedFrom != prior {
				t.Fatalf("resumed from %d, want %d", session.resumedFrom, prior)
			}
			state, err := orch.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			snapshot := state.Snapshot(time.Now())
			if len(snapshot.Running) != 1 || snapshot.Running[0].DetentSessionID != session.id {
				t.Fatalf("Running = %#v, want session %d", snapshot.Running, session.id)
			}
			snapshots := hub.New[telemetry.Snapshot]()
			defer snapshots.Close()
			if err := snapshots.Publish(snapshot); err != nil {
				t.Fatal(err)
			}
			server, err := web.NewServer(web.Config{LookupEnv: func(string) string { return "" }}, web.Dependencies{Hub: snapshots, Store: db, Registry: project.NewRegistry(), Connector: tracker, WorkerProcesses: db, ObserveProcesses: func(ids []procgroup.Identity) ([]procgroup.Observation, error) {
				out := make([]procgroup.Observation, len(ids))
				for i, id := range ids {
					out[i] = procgroup.Observation{Identity: id, Alive: true, ProcessCount: 2}
				}
				return out, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := server.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			for _, path := range []string{"/api/v1/state", "/health"} {
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != 200 {
					t.Fatalf("%s status %d: %s", path, response.Code, response.Body.String())
				}
				var body struct {
					Running     []telemetry.Running `json:"running"`
					AgentMemory []struct {
						RSSBytes int64 `json:"rss_bytes"`
					} `json:"agent_memory"`
					Orphans telemetry.OrphanedAgentProcesses `json:"orphaned_agent_processes"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if path == "/api/v1/state" {
					if len(body.Running) != 1 || body.Running[0].DetentSessionID != session.id {
						t.Fatalf("API Running = %#v", body.Running)
					}
				} else {
					if len(body.AgentMemory) != 1 || body.AgentMemory[0].RSSBytes != 256 {
						t.Fatalf("agent_memory = %#v", body.AgentMemory)
					}
					if body.Orphans.Count != 0 {
						t.Fatalf("active startup worker reported as orphan: %#v", body.Orphans)
					}
				}
			}
		})
	}
}

type startupHealthSession struct {
	id, resumedFrom int64
	validator       bool
	err             error
}
type startupHealthRunner struct {
	db      store.Store
	started chan startupHealthSession
}

func (r *startupHealthRunner) Run(ctx context.Context, req runpkg.RunRequest) (runpkg.RunResult, error) {
	r.session(ctx, req.Issue, req.OnUsageUpdate, false, req.ResumeState.DetentSessionID)
	return runpkg.RunResult{}, ctx.Err()
}
func (r *startupHealthRunner) Validate(ctx context.Context, req runpkg.ValidatorRequest) (gate.ValidatorResult, error) {
	r.session(ctx, req.Issue, req.OnUsageUpdate, true, 0)
	return gate.ValidatorResult{}, ctx.Err()
}
func (r *startupHealthRunner) session(ctx context.Context, issue connector.Issue, update runpkg.UsageUpdateHandler, validator bool, prior int64) {
	now := time.Now()
	role := "code"
	if validator {
		role = "validator"
	}
	id, err := r.db.StartSession(ctx, store.SessionStart{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, StartedAt: now, AgentBackendKind: "codex", AgentRole: role})
	if err == nil {
		err = r.db.UpdateSessionWorkerProcess(ctx, id, store.WorkerProcessRegistration{WorkerProcessIdentity: store.WorkerProcessIdentity{PID: 80484, GroupID: 80484, StartedAt: now}})
	}
	if err == nil && update != nil {
		err = update(runpkg.UsageUpdate{DetentSessionID: id, RSSBytes: 256, RSSObservedAt: now})
	}
	r.started <- startupHealthSession{id: id, resumedFrom: prior, validator: validator, err: err}
	<-ctx.Done()
}
