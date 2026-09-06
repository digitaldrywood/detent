package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

type associationTestConnector struct {
	autoPromoteTickConnector
	fresh connector.Issue
	err   error
	stage *time.Time
	calls int
}

func (c *associationTestConnector) RevalidatePullRequestAssociation(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.calls++
	issue.PRNumber = c.fresh.PRNumber
	issue.PRRepository = c.fresh.PRRepository
	issue.PullRequest = c.fresh.PullRequest
	issue.PRSource = c.fresh.PRSource
	issue.PRVerifiedAt = time.Now().UTC()
	if c.stage != nil {
		issue.StageUpdatedAt = c.stage
	}
	return issue, c.err
}

func TestTickRejectsRemovedPullRequestAssociation(t *testing.T) {
	t.Parallel()
	for _, prState := range []string{"OPEN", "MERGED"} {
		t.Run(prState, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Enabled: true}})
			issue := autoPromoteTickIssue("I_2238", []string{"bug"}, &connector.PullRequest{Number: 2239, State: prState, CIStatus: "pending"})
			issue.State = "Todo"
			tracker := &associationTestConnector{autoPromoteTickConnector: autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.tick(t.Context(), &state, time.Now())
			if len(tracker.updates) != 0 {
				t.Fatalf("unrelated PR changed Todo lane: %#v", tracker.updates)
			}
		})
	}
}

func TestTickRecoversNoAttemptStaleTodoReview(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		human        bool
		reentered    bool
		freshReentry bool
		attempt      bool
		historyError bool
		refreshError bool
		linked       bool
		missingStage bool
		wantRecovery bool
	}{
		{name: "detent reconciliation after restart", wantRecovery: true},
		{name: "human review", human: true},
		{name: "human reentered review", reentered: true},
		{name: "human reentered during refresh", freshReentry: true},
		{name: "previous attempt", attempt: true},
		{name: "unavailable attempt history", historyError: true},
		{name: "failed reference refresh", refreshError: true},
		{name: "reference returns", linked: true},
		{name: "unknown current lane entry", missingStage: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entered := time.Date(2026, 9, 5, 22, 36, 29, 0, time.UTC)
			mutated := time.Date(2026, 9, 5, 22, 37, 12, 0, time.UTC)
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Enabled: true}})
			issue := autoPromoteTickIssue("I_2238", []string{"bug"}, nil)
			issue.StageUpdatedAt = &mutated
			if tt.reentered {
				issue.StageUpdatedAt = new(mutated.Add(time.Minute))
			}
			if tt.missingStage {
				issue.StageUpdatedAt = nil
			}
			metadata := `{"pull_request":{"number":2239,"head_sha":"ee5c7e9"},"tracker_mutation_at":"2026-09-05T22:37:12Z","provenance":{"schema":2,"origin":"detent","initiator":"detent_instance","basis":"detent_operation"}}`
			if tt.human {
				metadata = `{"provenance":{"schema":2,"origin":"human"}}`
			}
			recorder := &autoPromoteWorkflowMetricsRecorder{events: []store.WorkflowPhaseEvent{{IssueID: issue.ID, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Human Review", PreviousPhaseName: "Todo", Reason: "ci_not_green", Status: "entered", StartedAt: entered, PRNumber: new(int64(2239)), MetadataJSON: metadata}}}
			tracker := &associationTestConnector{autoPromoteTickConnector: autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			if tt.freshReentry {
				tracker.stage = new(mutated.Add(time.Minute))
			}
			if tt.refreshError {
				tracker.err = errors.New("lookup unavailable")
			}
			if tt.linked {
				tracker.fresh.PullRequest = &connector.PullRequest{Number: 2242, State: "OPEN"}
			}
			attempts := &recordingWorkAttemptStore{}
			if tt.attempt {
				attempts.history = []store.WorkAttempt{{IssueID: issue.ID}}
			}
			if tt.historyError {
				attempts.historyErr = errors.New("store unavailable")
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: recorder, workAttempts: attempts}
			state := newState(cfg)
			orch.tick(t.Context(), &state, mutated.Add(6*time.Minute))
			if !tt.wantRecovery {
				if len(tracker.updates) != 0 {
					t.Fatalf("human review changed: %#v", tracker.updates)
				}
			} else if len(tracker.updates) != 1 || tracker.updates[0].state != "Todo" {
				t.Fatalf("updates = %#v, want recovery to Todo", tracker.updates)
			}
			if tt.wantRecovery {
				issue.State = "Todo"
				issue.StageUpdatedAt = new(mutated.Add(6 * time.Minute))
				tracker.stateIssues = []connector.Issue{issue}
				tracker.stateIssues[0].PRNumber = new(2239)
				tracker.stateIssues[0].PullRequest = &connector.PullRequest{Number: 2239, State: "MERGED"}
				tracker.fresh = connector.Issue{PRNumber: new(2239), PRRepository: workAttemptRepository(issue), PullRequest: tracker.stateIssues[0].PullRequest}
				state = newState(cfg)
				orch.tick(t.Context(), &state, mutated.Add(9*time.Minute))
				if len(tracker.updates) != 1 {
					t.Fatalf("recovered issue oscillated after restart: %#v", tracker.updates)
				}
			}
		})
	}
}

func TestAssociationRevalidationFailurePreservesLanes(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Todo", "Human Review", "In Progress", "Merging"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Enabled: true}})
			issue := autoPromoteTickIssue("I_2238", []string{"bug"}, &connector.PullRequest{Number: 2239, State: "MERGED"})
			issue.State = lane
			tracker := &associationTestConnector{autoPromoteTickConnector: autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, err: errors.New("lookup failed")}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.tick(t.Context(), &state, time.Now())
			if len(tracker.updates) != 0 {
				t.Fatalf("failed refresh changed lane: %#v", tracker.updates)
			}
		})
	}
}

func TestAssociationProvenanceSurvivesLaneTransition(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"github_closing_reference", "detent_branch"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Enabled: true}})
			issue := autoPromoteTickIssue("I_2238", []string{"bug"}, &connector.PullRequest{Number: 2239, State: "OPEN", CIStatus: "pending"})
			issue.State = "Todo"
			tracker := &associationTestConnector{autoPromoteTickConnector: autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, fresh: issue}
			tracker.fresh.PRSource = source
			tracker.fresh.PRRepository = "digitaldrywood/detent"
			recorder := &autoPromoteWorkflowMetricsRecorder{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: recorder}
			state := newState(cfg)
			orch.tick(t.Context(), &state, time.Now())
			found := false
			for _, event := range recorder.events {
				if event.Status == "entered" && event.PreviousPhaseName == "Todo" {
					found = true
					for _, fragment := range []string{`"association_source":"` + source + `"`, `"association_checked_at":`, `"repository":"digitaldrywood/detent"`, `"reconciliation":"stale_todo_pr"`} {
						if !strings.Contains(event.MetadataJSON, fragment) {
							t.Fatalf("metadata %s missing %s", event.MetadataJSON, fragment)
						}
					}
				}
			}
			if !found {
				t.Fatal("missing reconciliation lane entry")
			}
		})
	}
}
