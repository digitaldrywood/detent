package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/coordination"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestPlanCompletionCoordinationDeferral(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "retry"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
			issue := completionDeferralIssue("plan", "In Progress")
			tracker := &planCompletionTracker{completionDeferralConnector: completionDeferralConnector{issue: issue}}
			dbPath := filepath.Join(t.TempDir(), "detent.db")
			runtimeStore := openCompletionDeferralStoreWithoutCleanup(t, dbPath)
			t.Cleanup(func() {
				if err := runtimeStore.Close(); err != nil {
					t.Error(err)
				}
			})
			cfg := completionDeferralConfig()
			shared := &planCompletionCoordination{}
			ownerConfig := scheduleowner.Config{Enabled: true, Key: "plan-project"}.Normalized("example/repo", "")
			first, err := scheduleowner.New(ownerConfig, "host-a", shared, scheduleowner.Dependencies{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, owned, err := first.Acquire(t.Context()); err != nil || !owned {
				t.Fatalf("first host ownership: %v %v", owned, err)
			}
			second, err := scheduleowner.New(ownerConfig, "host-b", shared, scheduleowner.Dependencies{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			shared.err = &github.StatusError{StatusCode: 429, Err: github.ErrRateLimited, RetryAfter: 2 * time.Minute, ResetAt: now.Add(3 * time.Minute)}
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: runtimeStore, laneCoordination: shared, now: func() time.Time { return now }}
			state := newState(cfg)
			id := startCompletionDeferralAttempt(t, runtimeStore, issue, now)
			running := completionDeferralRunning(issue, id, now)
			running.Mode = runpkg.RunModePlan
			event := completionDeferralEvent(issue, id, now)
			event.Request.Mode = runpkg.RunModePlan
			state.Running[issue.ID] = running
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			planner := &planCompletionRunner{tracker: tracker, result: event.Result}
			event.Result, event.Err = planner.Run(t.Context(), event.Request)
			orch.handleRunResult(t.Context(), &state, event)
			if !state.Retry[issue.ID].CompletionDeferred {
				// Negative control: replay the planner selected by the old retry path.
				state.Running[issue.ID] = running
				event.Result, event.Err = planner.Run(t.Context(), event.Request)
				orch.handleRunResult(t.Context(), &state, event)
				t.Fatalf("successful plan scheduled full planner retry: runner calls=%d artifacts=%d", planner.calls, len(tracker.comments))
			}
			receipt, err := runtimeStore.WorkAttempt(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Status != store.WorkAttemptStatusActive || receipt.WaitReason != connector.TrackerUnavailableCondition || receipt.ErrorClass != connector.TrackerUnavailableCondition {
				t.Fatalf("receipt = %#v", receipt)
			}
			if got := state.Retry[issue.ID].DueAt; !got.Equal(now.Add(3 * time.Minute)) {
				t.Fatalf("retry at %v", got)
			}
			record := state.deferredCompletions[issue.ID]
			if record.Result.Output != event.Result.Output || record.Result.Tokens != event.Result.Tokens {
				t.Fatal("completed result changed during deferral")
			}
			before := shared.gets
			orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Minute))
			if shared.gets != before {
				t.Fatal("completion retried before GitHub reset deadline")
			}
			if restart {
				if err := runtimeStore.Close(); err != nil {
					t.Fatal(err)
				}
				runtimeStore = openCompletionDeferralStoreWithoutCleanup(t, dbPath)
				orch = Orchestrator{cfg: cfg, connector: tracker, workAttempts: runtimeStore, laneCoordination: shared, now: func() time.Time { return now }}
				state = newState(cfg)
				orch.recoverDurableWorkAttempts(t.Context(), &state, now)
			}
			for range 2 {
				now = state.Retry[issue.ID].DueAt
				orch.retryDeferredCompletions(t.Context(), &state, now)
				if _, owned, err := second.Acquire(t.Context()); owned || err == nil {
					t.Fatalf("peer acquired unavailable coordination: %v %v", owned, err)
				}
				if planner.calls != 1 || len(state.FailureBreaker.Failures) != 0 {
					t.Fatalf("runner calls=%d failures=%v", planner.calls, state.FailureBreaker.Failures)
				}
				if len(tracker.comments) != 2 {
					t.Fatalf("retry duplicated artifacts: %v", tracker.comments)
				}
				if !state.Retry[issue.ID].CompletionDeferred {
					t.Fatal("completion lost during repeated outage")
				}
				if got := orch.dispatchIssueWithOutcome(t.Context(), &state, issue, 3, now, ""); got.dispatched || got.reason != dispatchSkipCompletionDeferred {
					t.Fatalf("planner redispatch = %#v", got)
				}
			}
			shared.err = nil
			if _, owned, err := second.Acquire(t.Context()); owned || err != nil {
				t.Fatalf("peer stole waiting completion: %v %v", owned, err)
			}
			now = state.Retry[issue.ID].DueAt
			orch.retryDeferredCompletions(t.Context(), &state, now)
			if len(state.deferredCompletions) != 0 || len(tracker.comments) != 2 || len(tracker.updates) != 1 || tracker.updates[0].state != "Plan Review" {
				t.Fatalf("recovery: deferred=%d comments=%d updates=%v", len(state.deferredCompletions), len(tracker.comments), tracker.updates)
			}
			receipt, err = runtimeStore.WorkAttempt(t.Context(), id)
			if err != nil || receipt.TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("completed receipt = %#v, %v", receipt, err)
			}
		})
	}
}

type planCompletionTracker struct {
	completionDeferralConnector
	publicationError error
	readError        error
	transitionError  error
}

func (c *planCompletionTracker) CreateComment(ctx context.Context, id, body string) error {
	if err := c.backendCapacityTestConnector.CreateComment(ctx, id, body); err != nil {
		return err
	}
	return c.publicationError
}

func (c *planCompletionTracker) UpdateIssueState(ctx context.Context, id, target string) error {
	if c.transitionError != nil {
		return c.transitionError
	}
	return c.backendCapacityTestConnector.UpdateIssueState(ctx, id, target)
}

func (c *planCompletionTracker) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	if c.readError != nil {
		return nil, c.readError
	}
	comments := make([]connector.IssueComment, len(c.comments))
	for i, body := range c.comments {
		comments[i] = connector.IssueComment{Body: body}
	}
	return comments, nil
}

type planCompletionCoordination struct {
	err     error
	gets    int
	records map[string]coordination.Record
}

func (s *planCompletionCoordination) Get(_ context.Context, key string) (coordination.Record, bool, error) {
	s.gets++
	if s.err != nil {
		return coordination.Record{}, false, s.err
	}
	r, ok := s.records[key]
	return r, ok, nil
}
func (s *planCompletionCoordination) CompareAndSwap(_ context.Context, key, version string, value []byte) (coordination.Record, bool, error) {
	if s.records == nil {
		s.records = map[string]coordination.Record{}
	}
	if s.records[key].Version != version {
		return coordination.Record{}, false, nil
	}
	r := coordination.Record{Value: append([]byte(nil), value...), Version: version + "x", ModifiedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	s.records[key] = r
	return r, true, nil
}

type planCompletionRunner struct {
	tracker *planCompletionTracker
	result  runpkg.RunResult
	calls   int
}

func (r *planCompletionRunner) Run(_ context.Context, _ runpkg.RunRequest) (runpkg.RunResult, error) {
	r.calls++
	r.tracker.comments = append(r.tracker.comments, "## Detent Plan Review\n\nApproved")
	return r.result, nil
}

func TestPlanCompletionPublicationRecovery(t *testing.T) {
	for _, scenario := range []string{"lost response", "crash after comment", "comment read unavailable", "tracker lane unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
			issue := completionDeferralIssue("publication", "In Progress")
			tracker := &planCompletionTracker{completionDeferralConnector: completionDeferralConnector{issue: issue}}
			runtimeStore := openCompletionDeferralStore(t, filepath.Join(t.TempDir(), "detent.db"))
			cfg := completionDeferralConfig()
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: runtimeStore, now: func() time.Time { return now }}
			state := newState(cfg)
			id := startCompletionDeferralAttempt(t, runtimeStore, issue, now)
			running := completionDeferralRunning(issue, id, now)
			running.Mode = runpkg.RunModePlan
			event := completionDeferralEvent(issue, id, now)
			event.Request.Mode = runpkg.RunModePlan
			state.Running[issue.ID] = running
			switch scenario {
			case "tracker lane unavailable":
				tracker.transitionError = completionDeferralAvailabilityError()
			case "lost response":
				tracker.publicationError = errors.New("response lost after commit")
			case "comment read unavailable":
				tracker.readError = completionDeferralAvailabilityError()
			case "crash after comment":
				// Durable pre-publication checkpoint survives, but the publication receipt does not.
				if !orch.persistDeferredCompletion(t.Context(), &state, newDeferredCompletion(event, running, nil, now)) {
					t.Fatal("checkpoint failed")
				}
				if err := orch.publishCompletedPlan(t.Context(), event, &running); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "crash after comment" {
				orch.handleRunResult(t.Context(), &state, event)
			}
			receipt, err := runtimeStore.WorkAttempt(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Phase != deferredCompletionPhase {
				t.Fatalf("phase = %q", receipt.Phase)
			}
			if scenario == "comment read unavailable" && len(tracker.comments) != 0 {
				t.Fatal("posted without reconciling")
			}
			tracker.transitionError = nil
			tracker.publicationError = nil
			tracker.readError = nil
			recovered := newState(cfg)
			orch.recoverDeferredCompletions(t.Context(), &recovered, []store.WorkAttempt{receipt}, now)
			now = recovered.Retry[issue.ID].DueAt
			orch.retryDeferredCompletions(t.Context(), &recovered, now)
			if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0], "detent-plan-completion:") || len(tracker.updates) != 1 {
				t.Fatalf("comments=%v updates=%v", tracker.comments, tracker.updates)
			}
		})
	}
}

func TestPlanCompletionCheckpointFailure(t *testing.T) {
	for _, recovers := range []bool{false, true} {
		name := "unavailable"
		if recovers {
			name = "recovered"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
			issue := completionDeferralIssue("checkpoint", "In Progress")
			tracker := &planCompletionTracker{completionDeferralConnector: completionDeferralConnector{issue: issue}}
			attempts := &implementProgressAttemptStore{heartbeatErr: errors.New("store unavailable")}
			cfg := completionDeferralConfig()
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, now: func() time.Time { return now }}
			state := newState(cfg)
			running := completionDeferralRunning(issue, 1, now)
			running.Mode = runpkg.RunModePlan
			state.Running[issue.ID] = running
			event := completionDeferralEvent(issue, 1, now)
			event.Request.Mode = runpkg.RunModePlan
			orch.handleRunResult(t.Context(), &state, event)
			if len(tracker.comments) != 0 || len(tracker.updates) != 0 || state.deferredCompletions[issue.ID].Persisted {
				t.Fatal("side effect preceded durable checkpoint")
			}
			if recovers {
				attempts.heartbeatErr = nil
			}
			now = state.Retry[issue.ID].DueAt
			orch.retryDeferredCompletions(t.Context(), &state, now)
			want := 0
			if recovers {
				want = 1
			}
			if len(tracker.comments) != want || len(tracker.updates) != want {
				t.Fatalf("comments=%d updates=%d want=%d", len(tracker.comments), len(tracker.updates), want)
			}
		})
	}
}
