package web_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestStartupSessionsAppearInStateAndHealth(t *testing.T) {
	digest := policy.Digest([]byte("startup health fixture"))
	pinnedPolicy := policy.Descriptor{
		SourceRevision: digest,
		SourceDigest:   digest,
		ConfigDigest:   digest,
		Gates: policy.Gates{
			Kind: "command", PlanReview: "none", PlanStopDigest: digest,
			AutomatedReview: "off", MergeMethod: "squash",
		},
	}.WithID()
	metadata, err := json.Marshal(map[string]policy.Descriptor{"policy": pinnedPolicy})
	if err != nil {
		t.Fatal(err)
	}

	for _, validator := range []bool{false, true} {
		name := "orphan resume"
		if validator {
			name = "validator after restart"
		}
		t.Run(name, func(t *testing.T) {
			db := &startupHealthStore{}
			issue := connector.NewIssue()
			issue.ID, issue.Identifier, issue.State = "issue-2486", "example-org/example-project#2486", "Rework"
			issue.Title = "Startup session"
			issue.URL = "https://github.com/example-org/example-project/issues/2486"
			if validator {
				issue.PullRequest = &connector.PullRequest{Number: 2487, URL: "https://github.com/example-org/example-project/pull/2487", HeadSHA: "head", State: "OPEN", CIStatus: "success"}
			}
			attempt, err := db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: "Rework", StartedAt: time.Now().Add(-time.Minute), LeaseExpiresAt: time.Now().Add(time.Minute), WorkerMetadataJSON: string(metadata)})
			if err != nil {
				t.Fatal(err)
			}
			prior, err := db.StartSession(t.Context(), store.SessionStart{WorkAttemptID: attempt, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, StartedAt: time.Now().Add(-time.Minute), AgentBackendKind: "codex", AgentRole: "code", ProviderThreadID: "old-thread"})
			if err != nil {
				t.Fatal(err)
			}
			tracker := fakehub.New(fakehub.Config{Issues: []connector.Issue{issue}})
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
			case <-t.Context().Done():
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
			server, err := web.NewServer(web.Config{LookupEnv: func(string) string { return "" }}, web.Dependencies{Hub: snapshots, Store: startupHealthWebStore{}, Registry: project.NewRegistry(), Connector: tracker, WorkerProcesses: db, ObserveProcesses: func(ids []procgroup.Identity) ([]procgroup.Observation, error) {
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
	db      *startupHealthStore
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

type startupHealthWebStore struct {
	store.Store
}

type startupHealthStore struct {
	mu        sync.Mutex
	attempts  []store.WorkAttempt
	sessions  []store.SessionStart
	processes []store.WorkerProcess
}

func (s *startupHealthStore) StartWorkAttempt(_ context.Context, start store.WorkAttemptStart) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(len(s.attempts) + 1)
	s.attempts = append(s.attempts, store.WorkAttempt{ID: id, ProjectID: start.ProjectID, IssueID: start.IssueID, Identifier: start.Identifier, WorkerType: start.WorkerType, Lane: start.Lane, StartedAt: start.StartedAt, LeaseExpiresAt: start.LeaseExpiresAt, WorkerMetadataJSON: start.WorkerMetadataJSON, Status: store.WorkAttemptStatusActive})
	return id, nil
}

func (s *startupHealthStore) WorkAttempt(_ context.Context, id int64) (store.WorkAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[id-1], nil
}

func (s *startupHealthStore) RecordWorkAttemptHeartbeat(context.Context, store.WorkAttemptHeartbeat) error {
	return nil
}
func (s *startupHealthStore) CompleteWorkAttempt(_ context.Context, completion store.WorkAttemptCompletion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[completion.AttemptID-1].Status = completion.Status
	return nil
}
func (s *startupHealthStore) ListActiveWorkAttempts(_ context.Context, query store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var active []store.WorkAttempt
	for _, attempt := range s.attempts {
		if attempt.ProjectID == query.ProjectID && attempt.Status == store.WorkAttemptStatusActive {
			active = append(active, attempt)
		}
	}
	return active, nil
}
func (s *startupHealthStore) ListRecentTerminalWorkAttempts(context.Context, store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}
func (s *startupHealthStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return nil, nil
}
func (s *startupHealthStore) RecordSchedulerDecision(context.Context, store.SchedulerDecision) (int64, error) {
	return 1, nil
}
func (s *startupHealthStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}
func (s *startupHealthStore) StartSession(_ context.Context, start store.SessionStart) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, start)
	return int64(len(s.sessions)), nil
}
func (s *startupHealthStore) UpdateSessionWorkerProcess(_ context.Context, id int64, registration store.WorkerProcessRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processes = append(s.processes, store.WorkerProcess{SessionID: id, WorkerProcessIdentity: registration.WorkerProcessIdentity})
	return nil
}
func (s *startupHealthStore) ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.WorkerProcess(nil), s.processes...), nil
}
func (s *startupHealthStore) ListOrphanedAgentSessions(_ context.Context, project string) ([]store.OrphanedAgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var orphans []store.OrphanedAgentSession
	for i, session := range s.sessions {
		if session.ProjectID != project || session.WorkAttemptID == 0 {
			continue
		}
		attempt := s.attempts[session.WorkAttemptID-1]
		orphans = append(orphans, store.OrphanedAgentSession{ResumeState: store.AgentResumeState{DetentSessionID: int64(i + 1), ProviderThreadID: session.ProviderThreadID, AgentBackendKind: session.AgentBackendKind, Orphaned: true}, WorkAttemptID: session.WorkAttemptID, ProjectID: project, IssueID: session.IssueID, Identifier: session.Identifier, WorkerType: attempt.WorkerType, Lane: attempt.Lane, StartedAt: session.StartedAt})
	}
	return orphans, nil
}
func (s *startupHealthStore) MarkAgentSessionOrphaned(context.Context, int64, time.Time) error {
	return nil
}
