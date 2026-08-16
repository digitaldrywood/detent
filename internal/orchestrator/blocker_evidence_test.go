package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRecordedBlockerPredicateRegistry(t *testing.T) {
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	present := true
	absent := false
	tests := []struct {
		name             string
		blocker          workpad.Blocker
		prepare          func(*Orchestrator, *State, *connector.Issue, *blockerEvidenceTestConnector)
		wantStatus       string
		wantUnverifiable bool
		wantOwner        string
	}{
		{
			name: "issue state holds",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:       workpad.PredicateIssueState,
				Identifier: "digitaldrywood/detent#42",
				States:     []string{"open"},
			}),
			prepare: func(_ *Orchestrator, _ *State, _ *connector.Issue, tracker *blockerEvidenceTestConnector) {
				tracker.blockers = []connector.Issue{{Identifier: "digitaldrywood/detent#42", State: "In Progress"}}
			},
			wantStatus: blockerEvidenceStatusHolds,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "issue state clears",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:       workpad.PredicateIssueState,
				Identifier: "digitaldrywood/detent#42",
				States:     []string{"open"},
			}),
			prepare: func(_ *Orchestrator, _ *State, _ *connector.Issue, tracker *blockerEvidenceTestConnector) {
				tracker.blockers = []connector.Issue{{Identifier: "digitaldrywood/detent#42", State: "Done", Closed: true}}
			},
			wantStatus: blockerEvidenceStatusCleared,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "issue state rejects self reference",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:       workpad.PredicateIssueState,
				Identifier: "digitaldrywood/detent#evidence",
				States:     []string{"open"},
			}),
			wantStatus:       blockerEvidenceStatusUnverifiable,
			wantUnverifiable: true,
			wantOwner:        workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "pull request state holds",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:   workpad.PredicatePullRequestState,
				States: []string{"open"},
			}),
			prepare: func(_ *Orchestrator, _ *State, issue *connector.Issue, _ *blockerEvidenceTestConnector) {
				issue.PullRequest = &connector.PullRequest{Number: 42, State: "OPEN"}
			},
			wantStatus: blockerEvidenceStatusHolds,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "pull request state clears",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:   workpad.PredicatePullRequestState,
				States: []string{"open"},
			}),
			prepare: func(_ *Orchestrator, _ *State, issue *connector.Issue, _ *blockerEvidenceTestConnector) {
				issue.PullRequest = &connector.PullRequest{Number: 42, State: "MERGED"}
			},
			wantStatus: blockerEvidenceStatusCleared,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "pull request state refresh clears stale state",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:   workpad.PredicatePullRequestState,
				States: []string{"open"},
			}),
			prepare: func(_ *Orchestrator, _ *State, issue *connector.Issue, tracker *blockerEvidenceTestConnector) {
				issue.PullRequest = &connector.PullRequest{Number: 42, State: "OPEN"}
				tracker.hydratedPullRequest = &connector.PullRequest{Number: 42, State: "MERGED"}
			},
			wantStatus: blockerEvidenceStatusCleared,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "check absence holds",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:    workpad.PredicateCheckPresence,
				Check:   "Test",
				Present: &absent,
			}),
			prepare: func(_ *Orchestrator, _ *State, issue *connector.Issue, _ *blockerEvidenceTestConnector) {
				issue.PullRequest = &connector.PullRequest{Number: 42, State: "OPEN"}
			},
			wantStatus: blockerEvidenceStatusHolds,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "check appearance clears",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:    workpad.PredicateCheckPresence,
				Check:   "Test",
				Present: &absent,
			}),
			prepare: func(_ *Orchestrator, _ *State, issue *connector.Issue, _ *blockerEvidenceTestConnector) {
				issue.PullRequest = &connector.PullRequest{Number: 42, State: "OPEN", Checks: []connector.PullRequestCheck{{Name: "Test"}}}
			},
			wantStatus: blockerEvidenceStatusCleared,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "check presence condition holds",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:    workpad.PredicateCheckPresence,
				Check:   "Test",
				Present: &present,
			}),
			prepare: func(_ *Orchestrator, _ *State, issue *connector.Issue, _ *blockerEvidenceTestConnector) {
				issue.PullRequest = &connector.PullRequest{Number: 42, State: "OPEN", Checks: []connector.PullRequestCheck{{Name: "Test"}}}
			},
			wantStatus: blockerEvidenceStatusHolds,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "budget exhaustion holds",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:      workpad.PredicateBudgetCapacity,
				Scope:     "daily_budget",
				Condition: "exhausted",
			}),
			prepare: func(orch *Orchestrator, _ *State, _ *connector.Issue, _ *blockerEvidenceTestConnector) {
				orch.dailyBudgetStatus = fakeDailyBudgetStatusProvider{status: DailyBudgetStatus{Active: true, CurrentSpendUSD: 10, MaxUSD: 10}}
			},
			wantStatus: blockerEvidenceStatusHolds,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "budget recovery clears",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:      workpad.PredicateBudgetCapacity,
				Scope:     "daily_budget",
				Condition: "exhausted",
			}),
			prepare: func(orch *Orchestrator, _ *State, _ *connector.Issue, _ *blockerEvidenceTestConnector) {
				orch.dailyBudgetStatus = fakeDailyBudgetStatusProvider{status: DailyBudgetStatus{Active: true, CurrentSpendUSD: 4, MaxUSD: 10}}
			},
			wantStatus: blockerEvidenceStatusCleared,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "config fingerprint holds",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:        workpad.PredicateConfigFingerprint,
				Fingerprint: "config-a",
			}),
			prepare: func(orch *Orchestrator, _ *State, _ *connector.Issue, _ *blockerEvidenceTestConnector) {
				orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: blockedRecoverySnapshotWithConfig("config-a")}
			},
			wantStatus: blockerEvidenceStatusHolds,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "config fingerprint change clears",
			blocker: typedTestBlocker(workpad.Predicate{
				Type:        workpad.PredicateConfigFingerprint,
				Fingerprint: "config-a",
			}),
			prepare: func(orch *Orchestrator, _ *State, _ *connector.Issue, _ *blockerEvidenceTestConnector) {
				orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: blockedRecoverySnapshotWithConfig("config-b")}
			},
			wantStatus: blockerEvidenceStatusCleared,
			wantOwner:  workpad.BlockerOwnerOrchestrator,
		},
		{
			name: "free text remains unverifiable",
			blocker: workpad.Blocker{
				Reason:       "waiting for an investigation",
				Owner:        workpad.BlockerOwnerHuman,
				Unverifiable: true,
			},
			wantStatus:       blockerEvidenceStatusUnverifiable,
			wantUnverifiable: true,
			wantOwner:        workpad.BlockerOwnerHuman,
		},
		{
			name: "expired orchestrator predicate escalates",
			blocker: func() workpad.Blocker {
				blocker := typedTestBlocker(workpad.Predicate{Type: workpad.PredicateConfigFingerprint, Fingerprint: "config-a"})
				expiresAt := now.Add(-time.Minute)
				blocker.ExpiresAt = &expiresAt
				return blocker
			}(),
			prepare: func(orch *Orchestrator, _ *State, _ *connector.Issue, _ *blockerEvidenceTestConnector) {
				orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: blockedRecoverySnapshotWithConfig("config-a")}
			},
			wantStatus:       blockerEvidenceStatusUnverifiable,
			wantUnverifiable: true,
			wantOwner:        workpad.BlockerOwnerOrchestrator,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := &blockerEvidenceTestConnector{dependencyAutoUnblockConnector: &dependencyAutoUnblockConnector{}}
			orch := blockedCauseTestOrchestrator(tracker.dependencyAutoUnblockConnector)
			orch.connector = tracker
			state := newState(orch.cfg)
			issue := dependencyAutoUnblockIssue("issue-evidence", blockedStatusState)
			recordedAt := now.Add(-time.Hour)
			issue.WorkpadSignal = &workpad.Signal{
				Source:     workpad.SourceStructured,
				Status:     workpad.StatusBlocked,
				RecordedAt: &recordedAt,
				Blockers:   []workpad.Blocker{tt.blocker},
			}
			if tt.prepare != nil {
				tt.prepare(orch, &state, &issue, tracker)
			}

			got := orch.evaluateRecordedBlockers(t.Context(), &state, issue, nil, now)
			if !got.Found || len(got.Evidence) != 1 {
				t.Fatalf("evaluation = %#v, want one recorded blocker", got)
			}
			evidence := got.Evidence[0]
			if evidence.Status != tt.wantStatus || evidence.Unverifiable != tt.wantUnverifiable || evidence.Owner != tt.wantOwner {
				t.Fatalf("evidence = %#v", evidence)
			}
			if evidence.AgeSeconds != int64(time.Hour/time.Second) {
				t.Fatalf("AgeSeconds = %d, want %d", evidence.AgeSeconds, int64(time.Hour/time.Second))
			}
		})
	}
}

func TestRecordedBlockerRecoveryClearsWithinTick(t *testing.T) {
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		fingerprint    string
		wantTransition bool
		wantStatus     string
	}{
		{name: "condition holds", fingerprint: "config-a", wantStatus: blockerEvidenceStatusHolds},
		{name: "condition clears", fingerprint: "config-b", wantTransition: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := &dependencyAutoUnblockConnector{}
			orch := blockedCauseTestOrchestrator(tracker)
			orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: blockedRecoverySnapshotWithConfig(tt.fingerprint)}
			issue := dependencyAutoUnblockIssue("issue-tick-evidence", blockedStatusState)
			issue.WorkpadSignal = &workpad.Signal{
				Source: workpad.SourceStructured,
				Status: workpad.StatusBlocked,
				Blockers: []workpad.Blocker{typedTestBlocker(workpad.Predicate{
					Type:        workpad.PredicateConfigFingerprint,
					Fingerprint: "config-a",
				})},
			}
			state := newState(orch.cfg)
			state.Blocked[issue.ID] = Blocked{Issue: issue, BlockedAt: now.Add(-time.Hour), Source: BlockedSourceProjectStatus}

			transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now)

			_, didTransition := transitioned[issue.ID]
			if didTransition != tt.wantTransition {
				t.Fatalf("transitioned = %t, want %t", didTransition, tt.wantTransition)
			}
			if tt.wantTransition {
				if len(tracker.updates) != 1 || tracker.updates[0].state != "Todo" {
					t.Fatalf("updates = %#v, want Todo transition", tracker.updates)
				}
				if _, ok := state.Blocked[issue.ID]; ok {
					t.Fatalf("Blocked[%q] remains after predicate cleared", issue.ID)
				}
				return
			}
			entry := state.Blocked[issue.ID]
			if entry.RecoveryAction != "defer" || len(entry.BlockerEvidence) != 1 || entry.BlockerEvidence[0].Status != tt.wantStatus {
				t.Fatalf("blocked entry = %#v", entry)
			}
			snapshot := state.Snapshot(now)
			if len(snapshot.Blocked) != 1 || len(snapshot.Blocked[0].BlockerEvidence) != 1 || snapshot.Blocked[0].BlockerEvidence[0].Owner != workpad.BlockerOwnerOrchestrator {
				t.Fatalf("snapshot blocker evidence = %#v", snapshot.Blocked)
			}
		})
	}
}

func TestExplicitIssueStatePredicateDoesNotBecomeLegacyDependency(t *testing.T) {
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	referenced := dependencyAutoUnblockIssue("issue-reference-open", "In Progress")
	referenced.Identifier = "digitaldrywood/detent#42"
	tracker := &dependencyAutoUnblockConnector{blockers: []connector.Issue{referenced}}
	orch := blockedCauseTestOrchestrator(tracker)
	issue := dependencyAutoUnblockIssue("issue-explicit-state", blockedStatusState)
	issue.WorkpadSignal = &workpad.Signal{
		Source: workpad.SourceStructured,
		Status: workpad.StatusBlocked,
		Blockers: []workpad.Blocker{typedTestBlocker(workpad.Predicate{
			Type:       workpad.PredicateIssueState,
			Identifier: referenced.Identifier,
			States:     []string{"closed"},
		})},
	}
	state := newState(orch.cfg)
	state.Blocked[issue.ID] = Blocked{Issue: issue, BlockedAt: now.Add(-time.Hour), Source: BlockedSourceProjectStatus}

	transitioned := orch.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing after explicit state stopped matching", issue.ID)
	}
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Todo" {
		t.Fatalf("updates = %#v, want Todo transition", tracker.updates)
	}
}

func typedTestBlocker(predicate workpad.Predicate) workpad.Blocker {
	return workpad.Blocker{
		Owner:           workpad.BlockerOwnerOrchestrator,
		Predicate:       &predicate,
		RecheckInterval: "tick",
	}
}

func blockedRecoverySnapshotWithConfig(fingerprint string) runpkg.BlockedRecoverySnapshot {
	return runpkg.BlockedRecoverySnapshot{
		ConfigFingerprint: fingerprint,
		WorkspaceStatus:   "absent",
		Health:            "ready",
	}
}

type blockerEvidenceTestConnector struct {
	*dependencyAutoUnblockConnector
	hydratedPullRequest *connector.PullRequest
}

func (c *blockerEvidenceTestConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	if c.hydratedPullRequest != nil {
		issue.PullRequest = c.hydratedPullRequest
	}
	return issue, nil
}
