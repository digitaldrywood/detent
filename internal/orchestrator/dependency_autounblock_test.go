package orchestrator

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestTickRecoversCurrentWorkpadDependencyBlockers(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 8, 18, 33, 59, 0, time.UTC)
	resolved := dependencyAutoUnblockIssue("issue-workpad-resolved", "Done")
	resolved.Identifier = "digitaldrywood/ghostreel#34"
	resolved.Closed = true
	unresolved := dependencyAutoUnblockIssue("issue-workpad-unresolved", "In Progress")
	unresolved.Identifier = "digitaldrywood/ghostreel#35"

	tests := []struct {
		name                string
		blockers            []connector.Issue
		humanAction         string
		invalid             *workpad.Invalid
		labels              []string
		stickyReason        string
		nativeDuplicate     bool
		workpadUpdatedAt    time.Time
		previousLaneStarted time.Time
		wantTransition      bool
		wantHoldReason      string
		wantUnresolved      string
		wantWorkpadLookup   bool
	}{
		{
			name:                "all refs resolved after no progress limit",
			blockers:            []connector.Issue{resolved},
			stickyReason:        noProgressLimitReason,
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantTransition:      true,
			wantWorkpadLookup:   true,
		},
		{
			name:                "resolved ref also has native relation",
			blockers:            []connector.Issue{resolved},
			stickyReason:        noProgressLimitReason,
			nativeDuplicate:     true,
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantTransition:      true,
			wantWorkpadLookup:   true,
		},
		{
			name:                "one of several refs unresolved",
			blockers:            []connector.Issue{resolved, unresolved},
			stickyReason:        noProgressLimitReason,
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantHoldReason:      "workpad_blocker",
			wantUnresolved:      unresolved.Identifier,
			wantWorkpadLookup:   true,
		},
		{
			name:                "resolved refs with human action",
			blockers:            []connector.Issue{resolved},
			humanAction:         "install production credentials",
			stickyReason:        noProgressLimitReason,
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantHoldReason:      "human_action",
		},
		{
			name:                "resolved refs with opt-out label",
			blockers:            []connector.Issue{resolved},
			labels:              []string{"requires-human-review"},
			stickyReason:        noProgressLimitReason,
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantHoldReason:      "human_action",
		},
		{
			name:                "invalid signal",
			blockers:            []connector.Issue{resolved},
			invalid:             &workpad.Invalid{Message: "invalid status block"},
			stickyReason:        noProgressLimitReason,
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantHoldReason:      "invalid_workpad_signal",
		},
		{
			name:                "resolved refs with exhausted rework breaker",
			blockers:            []connector.Issue{resolved},
			stickyReason:        "rework_limit",
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantWorkpadLookup:   true,
		},
		{
			name:                "resolved refs with token ceiling breaker",
			blockers:            []connector.Issue{resolved},
			stickyReason:        "token_ceiling_circuit_breaker",
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantWorkpadLookup:   true,
		},
		{
			name:                "resolved refs with artifact convergence breaker",
			blockers:            []connector.Issue{resolved},
			stickyReason:        artifactGateConvergenceBlockedReasonPrefix + "render status unchanged",
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantWorkpadLookup:   true,
		},
		{
			name:                "resolved refs with merge worker exhaustion",
			blockers:            []connector.Issue{resolved},
			stickyReason:        mergeWorkerRetryExhaustedReason + ": retry limit reached",
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantWorkpadLookup:   true,
		},
		{
			name:                "resolved refs with operator stop",
			blockers:            []connector.Issue{resolved},
			stickyReason:        string(store.WorkAttemptTerminalOperatorStopped),
			workpadUpdatedAt:    parkedAt.Add(-time.Minute),
			previousLaneStarted: parkedAt.Add(-time.Hour),
			wantHoldReason:      "operator_stop",
		},
		{
			name:                "stale blocker list from prior blocked entry",
			blockers:            []connector.Issue{resolved},
			stickyReason:        noProgressLimitReason,
			workpadUpdatedAt:    parkedAt.Add(-2 * time.Hour),
			previousLaneStarted: parkedAt.Add(-time.Hour),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			waiting := dependencyAutoUnblockIssue("issue-workpad-no-progress", blockedStatusState)
			waiting.StageUpdatedAt = &parkedAt
			waiting.Labels = append(waiting.Labels, tt.labels...)
			waiting.WorkpadSignal = &workpad.Signal{
				Source:      workpad.SourceStructured,
				CommentURL:  "https://github.test/workpad/current",
				Status:      workpad.StatusBlocked,
				HumanAction: tt.humanAction,
				Invalid:     tt.invalid,
			}
			for _, blocker := range tt.blockers {
				waiting.WorkpadSignal.Blockers = append(waiting.WorkpadSignal.Blockers, workpad.Blocker{
					Ref:        blocker.Identifier,
					Identifier: blocker.Identifier,
				})
			}
			if tt.nativeDuplicate {
				waiting.BlockedBy = []connector.BlockedRef{{
					Identifier: resolved.Identifier,
					Source:     connector.BlockedRefSourceNative,
				}}
			}
			waiting.BlockerReason = workpad.Reason(waiting.WorkpadSignal)
			waiting.Comments = []connector.IssueComment{{
				URL:       waiting.WorkpadSignal.CommentURL,
				UpdatedAt: timePointer(tt.workpadUpdatedAt),
			}}
			tracker := &dependencyAutoUnblockConnector{
				stateIssues: []connector.Issue{waiting},
				blockers:    tt.blockers,
			}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
				Enabled:      true,
				SourceStates: []string{blockedStatusState},
				TargetState:  "Todo",
				Readiness:    DependencyReadinessTerminalOrMerged,
			})
			orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch.workflowMetrics = metrics
			recordDependencyLaneEntry(t, metrics, waiting, "In Progress", "dispatch", tt.previousLaneStarted)
			recordDependencyLaneEntry(t, metrics, waiting, blockedStatusState, tt.stickyReason, parkedAt)
			state := newState(orch.cfg)
			state.Blocked[waiting.ID] = Blocked{
				Issue:     waiting,
				Reason:    tt.stickyReason,
				BlockedAt: parkedAt,
				Source:    BlockedSourceProjectStatus,
			}

			orch.tick(t.Context(), &state, parkedAt.Add(3*time.Hour))

			if tt.wantTransition {
				if got, want := tracker.updates, []dependencyAutoUnblockUpdate{{issueID: waiting.ID, state: "Todo"}}; !slices.Equal(got, want) {
					t.Fatalf("updates = %#v, want resolved Workpad blocker moved to Todo", got)
				}
				if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, resolved.Identifier) {
					t.Fatalf("comments = %#v, want recovery audit naming %s", tracker.comments, resolved.Identifier)
				}
			} else if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want issue held", tracker.updates)
			}
			if tt.wantHoldReason != "" && !strings.Contains(logs.String(), "reason="+tt.wantHoldReason) {
				t.Fatalf("logs missing hold reason %q:\n%s", tt.wantHoldReason, logs.String())
			}
			if tt.wantUnresolved != "" && !strings.Contains(logs.String(), "unresolved_workpad_blockers=["+tt.wantUnresolved+"]") {
				t.Fatalf("logs missing unresolved blocker %q:\n%s", tt.wantUnresolved, logs.String())
			}
			lookedUp := slices.Contains(tracker.identifierCalls, resolved.Identifier)
			if lookedUp != tt.wantWorkpadLookup {
				t.Fatalf("workpad blocker lookup = %t, want %t; calls = %#v", lookedUp, tt.wantWorkpadLookup, tracker.identifierCalls)
			}
			if tt.name == "stale blocker list from prior blocked entry" && strings.Contains(logs.String(), "reason=workpad_blocker") {
				t.Fatalf("stale Workpad blocker held current entry:\n%s", logs.String())
			}
		})
	}
}

func TestWorkpadDependencyAutoUnblockConsumptionSurvivesRestart(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 8, 18, 33, 59, 0, time.UTC)
	waiting := dependencyAutoUnblockIssue("issue-workpad-restart", blockedStatusState)
	waiting.StageUpdatedAt = &parkedAt
	waiting.WorkpadSignal = &workpad.Signal{
		Source: workpad.SourceStructured,
		Status: workpad.StatusBlocked,
		Blockers: []workpad.Blocker{{
			Ref:        "digitaldrywood/ghostreel#34",
			Identifier: "digitaldrywood/ghostreel#34",
		}},
	}
	blocker := dependencyAutoUnblockIssue("issue-workpad-restart-blocker", "Done")
	blocker.Identifier = "digitaldrywood/ghostreel#34"
	blocker.Closed = true
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{blockedStatusState},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	state := newState(orch.cfg)

	if transitioned := orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, parkedAt.Add(time.Minute)); len(transitioned) != 1 {
		t.Fatalf("first transitioned = %#v, want successful Workpad recovery", transitioned)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, blocker.Identifier) {
		t.Fatalf("comments = %#v, want one recovery audit naming %s", tracker.comments, blocker.Identifier)
	}

	restartedTracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	restarted := dependencyAutoUnblockOrchestrator(restartedTracker, orch.cfg.DependencyAutoUnblock)
	restarted.workflowMetrics = metrics
	restartedState := newState(restarted.cfg)

	if transitioned := restarted.autoUnblockDependencyIssues(t.Context(), &restartedState, restartedTracker.stateIssues, parkedAt.Add(2*time.Minute)); len(transitioned) != 0 {
		t.Fatalf("restart transitioned = %#v, want consumed blocker signature held", transitioned)
	}
	if len(restartedTracker.updates) != 0 || len(restartedTracker.comments) != 0 {
		t.Fatalf("restart updates = %#v, comments = %#v, want no repeated transition", restartedTracker.updates, restartedTracker.comments)
	}
}

func recordDependencyLaneEntry(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	phase string,
	reason string,
	at time.Time,
) {
	t.Helper()
	if _, err := metrics.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{
		ProjectID:  defaultWorkflowMetricsProjectID,
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
		PhaseType:  store.WorkflowPhaseTypeLane,
		PhaseName:  phase,
		Reason:     reason,
		Status:     "entered",
		StartedAt:  at,
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

func TestTickAutoUnblocksDependencyWaitingIssue(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-blocked", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#388"}}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#388"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)
	now := time.Date(2026, 6, 12, 16, 0, 0, 0, time.UTC)

	orch.tick(context.Background(), &state, now)

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Todo"}) {
		t.Fatalf("updates = %#v, want Blocked issue moved to Todo", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one audit comment", tracker.comments)
	}
	for _, want := range []string{
		"Dependency blockers cleared.",
		"Blocked to Todo",
		"digitaldrywood/detent#388",
		"Done",
	} {
		if !strings.Contains(tracker.comments[0].body, want) {
			t.Fatalf("comment %q missing %q", tracker.comments[0].body, want)
		}
	}
	if _, ok := state.Blocked[waiting.ID]; ok {
		t.Fatalf("Blocked[%q] present after auto-unblock", waiting.ID)
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "dependency_auto_unblock_transition" {
		t.Fatalf("RecentEvents = %#v, want dependency auto-unblock event", state.RecentEvents)
	}
}

func TestTickAutoUnblocksDependencyOnlyOnceForSameResolvedBlockerSet(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-repeat-blocked", "Blocked")
	prNumber := 418
	waiting.PRNumber = &prNumber
	waiting.PullRequest = &connector.PullRequest{
		Number: prNumber,
		State:  "OPEN",
		URL:    "https://github.test/digitaldrywood/detent/pull/418",
	}
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415"}}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	state := newState(orch.cfg)
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)

	orch.tick(context.Background(), &state, now)
	orch.tick(context.Background(), &state, now.Add(time.Minute))

	if got, want := tracker.updates, []dependencyAutoUnblockUpdate{{issueID: waiting.ID, state: "Rework"}}; !slices.Equal(got, want) {
		t.Fatalf("updates = %#v, want one Blocked to Rework transition", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one audit comment", tracker.comments)
	}
	events := metrics.snapshot()
	rework := store.WorkflowPhaseEvent{}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].PhaseType == store.WorkflowPhaseTypeLane && events[index].PhaseName == "Rework" && events[index].Status == "entered" {
			rework = events[index]
			break
		}
	}
	metadata, ok := workflowLaneMetadataFromJSON(rework.MetadataJSON)
	if !ok || metadata.DependencyAutoUnblock == nil || metadata.DependencyAutoUnblock.BlockerSet == "" ||
		metadata.Provenance.Origin != provenance.OriginDependency {
		t.Fatalf("latest Rework metadata = %q, want dependency_auto_unblock blocker_set", rework.MetadataJSON)
	}
}

func TestDependencyAutoUnblockDoesNotConsumeRejectedTransition(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-rejected-transition", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415"}}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	rejecting := &dependencyAutoUnblockRejectOnceConnector{dependencyAutoUnblockConnector: tracker, reject: true}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	orch.connector = rejecting
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	state := newState(orch.cfg)
	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)

	if transitioned := orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now); len(transitioned) != 0 {
		t.Fatalf("first transitioned = %#v, want rejected transition held", transitioned)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("first comments = %#v, want no success audit", tracker.comments)
	}
	if _, ok := state.DependencyAutoUnblocks[waiting.ID]; ok {
		t.Fatalf("DependencyAutoUnblocks[%q] recorded after rejected transition", waiting.ID)
	}
	if orch.workflowTimelineHasDependencyAutoUnblock(t.Context(), waiting, dependencyAutoUnblockBlockerSet([]dependencyBlocker{{
		Ref:      waiting.BlockedBy[0],
		Issue:    blocker,
		Resolved: true,
	}})) {
		t.Fatal("rejected transition recorded a consumed blocker set")
	}

	rejecting.reject = false
	if transitioned := orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now.Add(time.Minute)); len(transitioned) != 1 {
		t.Fatalf("second transitioned = %#v, want successful retry", transitioned)
	}
	if len(tracker.updates) != 2 {
		t.Fatalf("updates = %#v, want rejected attempt and successful retry", tracker.updates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one success audit", tracker.comments)
	}
}

func TestDependencyAutoUnblockRearmsWhenBlockerReadinessChanges(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-readiness-change", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415"}}
	blocker := dependencyAutoUnblockIssue("issue-open", "In Progress")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)
	orch.recordLaneTransition(t.Context(), waiting, "Todo", now.Add(-time.Hour), "dependency_auto_unblock", workflowLaneMetadata{
		DependencyAutoUnblock: &workflowLaneDependencyAutoUnblockMetadata{
			BlockerSet: dependencyAutoUnblockBlockerSet([]dependencyBlocker{{
				Ref:      waiting.BlockedBy[0],
				Issue:    blocker,
				Resolved: true,
			}}),
			Blockers: []string{blocker.Identifier},
		},
	})
	state := newState(orch.cfg)

	if transitioned := orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now); len(transitioned) != 0 {
		t.Fatalf("open blocker transitioned = %#v, want blockers_not_ready", transitioned)
	}
	tracker.blockers[0].State = "Done"
	if transitioned := orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now.Add(time.Minute)); len(transitioned) != 1 {
		t.Fatalf("closed blocker transitioned = %#v, want readiness change to re-arm", transitioned)
	}
}

func TestDependencyAutoUnblockLoudlySuppressesUnchangedReadyLoop(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-unchanged-loop", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415"}}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	var logs bytes.Buffer
	orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
	state := newState(orch.cfg)
	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)

	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now)
	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now.Add(time.Minute))

	if len(tracker.updates) != 1 {
		t.Fatalf("updates = %#v, want unchanged ready loop suppressed", tracker.updates)
	}
	for _, want := range []string{
		"level=WARN",
		"action=error",
		"reason=terminal_blocker_set_already_consumed",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, logs.String())
		}
	}
	tracker.blockers[0].State = "Cancelled"
	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now.Add(2*time.Minute))
	if len(tracker.updates) != 2 {
		t.Fatalf("updates after blocker state change = %#v, want latch re-armed", tracker.updates)
	}
}

func TestDependencyAutoUnblockRearmsAfterBlockerReopens(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)
	waiting := dependencyAutoUnblockIssue("issue-reopened-blocker", "Blocked")
	waiting.StageUpdatedAt = timePointer(now.Add(-time.Hour))
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415"}}
	blocker := dependencyAutoUnblockIssue("issue-415", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	state := newState(orch.cfg)

	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now)
	tracker.stateIssues[0].StageUpdatedAt = timePointer(now.Add(time.Minute))
	tracker.blockers[0].State = "In Progress"
	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now.Add(2*time.Minute))
	tracker.blockers[0].State = "Done"
	orch.autoUnblockDependencyIssues(t.Context(), &state, tracker.stateIssues, now.Add(3*time.Minute))

	if got, want := tracker.updates, []dependencyAutoUnblockUpdate{
		{issueID: waiting.ID, state: "Todo"},
		{issueID: waiting.ID, state: "Todo"},
	}; !slices.Equal(got, want) {
		t.Fatalf("updates = %#v, want unblock before and after blocker reopening", got)
	}
}

func TestTickAutoUnblockSkipsPersistedReworkLimitBlockedIssue(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-rework-limit-blocked", "Blocked")
	waiting.Description = "Blocked by #415"
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      waiting.ID,
		Identifier:   waiting.Identifier,
		IssueURL:     waiting.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    "Blocked",
		Reason:       "rework_limit",
		Status:       "entered",
		StartedAt:    time.Date(2026, 7, 8, 14, 59, 0, 0, time.UTC),
		MetadataJSON: "{}",
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 7, 8, 15, 1, 0, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no auto-unblock for rework_limit Blocked issue", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want no auto-unblock comment", tracker.comments)
	}
}

func TestTickAutoUnblocksDependencyParkNewerThanStickyHistory(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-current-dependency-park", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{
		Identifier: "digitaldrywood/video-studio#59",
		Source:     connector.BlockedRefSourceNative,
	}}
	waiting.BlockerReason = noProgressLimitReason
	parkedAt := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	waiting.StageUpdatedAt = &parkedAt
	blocker := dependencyAutoUnblockIssue("issue-closed-blocker", "Done")
	blocker.Identifier = "digitaldrywood/video-studio#59"
	blocker.Closed = true
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      waiting.ID,
		Identifier:   waiting.Identifier,
		IssueURL:     waiting.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    "Blocked",
		Reason:       noProgressLimitReason,
		Status:       "entered",
		StartedAt:    parkedAt.Add(-time.Hour),
		MetadataJSON: "{}",
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, parkedAt.Add(time.Minute))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Todo"}) {
		t.Fatalf("updates = %#v, want newer dependency park moved to Todo", got)
	}
}

func TestTickAutoUnblocksLightweightDependencyWaitingIssue(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-lightweight-blocked", "Blocked")
	hydratedWaiting := waiting
	hydratedWaiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#388"}}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#388"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues:     []connector.Issue{waiting},
		hydratedIssues:  []connector.Issue{hydratedWaiting},
		blockers:        []connector.Issue{blocker},
		identifierCalls: []string{},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 3, 0, 0, time.UTC))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Todo"}) {
		t.Fatalf("updates = %#v, want lightweight Blocked issue moved to Todo", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one audit comment", tracker.comments)
	}
	if !strings.Contains(tracker.comments[0].body, "digitaldrywood/detent#388") {
		t.Fatalf("comment = %q, want hydrated dependency reference", tracker.comments[0].body)
	}
	if got, want := tracker.identifierCalls, []string{waiting.Identifier, "digitaldrywood/detent#388"}; !slices.Equal(got, want) {
		t.Fatalf("identifier calls = %#v, want %#v", got, want)
	}
}

func TestTickAutoUnblocksDependencyFromIssueBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		description string
	}{
		{name: "depends on colon", description: "Depends on: #415"},
		{name: "depends on no colon", description: "Depends on #415"},
		{name: "blocked by no colon", description: "Blocked by #415"},
		{name: "depends hyphen owner repo", description: "depends-on digitaldrywood/detent#415"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			waiting := dependencyAutoUnblockIssue("issue-body-blocked", "Blocked")
			waiting.Description = tt.description
			blocker := dependencyAutoUnblockIssue("issue-done", "Done")
			blocker.Identifier = "digitaldrywood/detent#415"
			tracker := &dependencyAutoUnblockConnector{
				stateIssues: []connector.Issue{waiting},
				blockers:    []connector.Issue{blocker},
			}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
				Enabled:      true,
				SourceStates: []string{"Blocked"},
				TargetState:  "Todo",
				Readiness:    DependencyReadinessTerminalOrMerged,
			})
			state := newState(orch.cfg)

			orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 3, 30, 0, time.UTC))

			if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Todo"}) {
				t.Fatalf("updates = %#v, want Blocked issue moved to Todo", got)
			}
			if !slices.Contains(tracker.identifierCalls, "digitaldrywood/detent#415") {
				t.Fatalf("identifier calls = %#v, want dependency lookup", tracker.identifierCalls)
			}
		})
	}
}

func TestTickAutoUnblocksDependencyFromBlockedReason(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-reason-blocked", "Blocked")
	waiting.BlockerReason = "Waiting on #415 before continuing."
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 4, 0, 0, time.UTC))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Todo"}) {
		t.Fatalf("updates = %#v, want Blocked issue moved to Todo", got)
	}
	if !slices.Contains(tracker.identifierCalls, "digitaldrywood/detent#415") {
		t.Fatalf("identifier calls = %#v, want dependency lookup", tracker.identifierCalls)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "digitaldrywood/detent#415") {
		t.Fatalf("comments = %#v, want recovery comment with blocked reason dependency", tracker.comments)
	}
}

func TestTickLeavesHumanBlockedIssueBlocked(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-human-blocked", "Blocked")
	waiting.BlockerReason = "Waiting on production credentials"
	tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{waiting}}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 5, 0, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
	}
	blocked, ok := state.Blocked[waiting.ID]
	if !ok {
		t.Fatalf("Blocked[%q] missing for human blocker", waiting.ID)
	}
	if blocked.Reason != waiting.BlockerReason {
		t.Fatalf("Blocked reason = %q, want %q", blocked.Reason, waiting.BlockerReason)
	}
}

func TestTickRecoversPRBackedBlockedIssueToRework(t *testing.T) {
	t.Parallel()

	prNumber := 426
	waiting := dependencyAutoUnblockIssue("issue-pr-blocked", "Blocked")
	waiting.PRNumber = &prNumber
	waiting.PullRequest = &connector.PullRequest{
		Number:          prNumber,
		URL:             "https://github.com/digitaldrywood/detent/pull/426",
		State:           "OPEN",
		HeadSHA:         "sha-current",
		BaseSHA:         "base-current",
		DiffFingerprint: "diff-current",
		MergeableState:  "dirty",
	}
	waiting.BlockerReason = "PR #426 conflicts with main and needs a rebase."
	tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{waiting}}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	metrics := &autoPromoteWorkflowMetricsRecorder{}
	orch.workflowMetrics = metrics
	recordBlockedRecoveryReasonEvent(t, metrics, waiting, time.Date(2026, 6, 12, 16, 5, 0, 0, time.UTC), blockedRecoveryReasonMergeConflict)
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 6, 0, 0, time.UTC))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Rework"}) {
		t.Fatalf("updates = %#v, want Blocked issue moved to Rework", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one recovery comment", tracker.comments)
	}
	for _, want := range []string{"PR maintenance is agent-recoverable.", "Blocked to Rework", "merge conflicts", "#426"} {
		if !strings.Contains(tracker.comments[0].body, want) {
			t.Fatalf("comment %q missing %q", tracker.comments[0].body, want)
		}
	}
	if _, ok := state.Blocked[waiting.ID]; ok {
		t.Fatalf("Blocked[%q] present after PR recovery", waiting.ID)
	}
}

func TestTickLeavesHumanOnlyPRBackedBlockedIssueBlocked(t *testing.T) {
	t.Parallel()

	prNumber := 427
	waiting := dependencyAutoUnblockIssue("issue-human-pr-blocked", "Blocked")
	waiting.PRNumber = &prNumber
	waiting.PullRequest = &connector.PullRequest{
		Number:         prNumber,
		State:          "OPEN",
		HeadSHA:        "sha-current",
		MergeableState: "dirty",
	}
	waiting.BlockerReason = "Waiting on explicit human approval before continuing."
	tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{waiting}}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 7, 0, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
	}
	if _, ok := state.Blocked[waiting.ID]; !ok {
		t.Fatalf("Blocked[%q] missing for human-only blocker", waiting.ID)
	}
}

func TestTickLeavesDependencyBlockedPRIssueBlocked(t *testing.T) {
	t.Parallel()

	prNumber := 428
	waiting := dependencyAutoUnblockIssue("issue-dependent-pr-blocked", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415", State: "In Progress"}}
	waiting.PRNumber = &prNumber
	waiting.PullRequest = &connector.PullRequest{
		Number:         prNumber,
		State:          "OPEN",
		HeadSHA:        "sha-current",
		MergeableState: "dirty",
	}
	waiting.BlockerReason = "Waiting on #415 before resolving PR conflicts."
	blocker := dependencyAutoUnblockIssue("issue-in-progress", "In Progress")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 8, 0, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none while dependency is not ready", tracker.updates)
	}
	if _, ok := state.Blocked[waiting.ID]; !ok {
		t.Fatalf("Blocked[%q] missing for unresolved dependency blocker", waiting.ID)
	}
}

func TestTickAutoUnblocksDependencyBlockedPRIssueToRework(t *testing.T) {
	t.Parallel()

	prNumber := 430
	waiting := dependencyAutoUnblockIssue("issue-ready-pr-blocked", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415", State: "Done"}}
	waiting.PRNumber = &prNumber
	waiting.PullRequest = &connector.PullRequest{
		Number:     prNumber,
		URL:        "https://github.com/digitaldrywood/detent/pull/430",
		State:      "OPEN",
		HeadSHA:    "sha-current",
		BranchName: "detent/digitaldrywood_detent_429",
	}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 8, 15, 0, time.UTC))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Rework"}) {
		t.Fatalf("updates = %#v, want dependency-unblocked PR issue moved to Rework", got)
	}
	if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "Blocked to Rework") {
		t.Fatalf("comments = %#v, want dependency auto-unblock comment for Rework", tracker.comments)
	}
}

func TestTickAutoUnblocksPreviouslyStartedDependencyIssueToRework(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-ready-started-blocked", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415", State: "Done"}}
	blocker := dependencyAutoUnblockIssue("issue-done", "Done")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)
	state.Retry[waiting.ID] = Retry{Issue: waiting, Attempt: 1}

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 8, 20, 0, time.UTC))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Rework"}) {
		t.Fatalf("updates = %#v, want dependency-unblocked started issue moved to Rework", got)
	}
}

func TestTickMergesHydratedTextDependenciesWithConnectorRefs(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-416", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#415"}}
	hydratedWaiting := waiting
	hydratedWaiting.Description = strings.Join([]string{
		"Depends on: #414",
		"Depends on: #415",
	}, "\n")
	readyBlocker := dependencyAutoUnblockIssue("issue-415", "Done")
	readyBlocker.Identifier = "digitaldrywood/detent#415"
	unreadyBlocker := dependencyAutoUnblockIssue("issue-414", "In Progress")
	unreadyBlocker.Identifier = "digitaldrywood/detent#414"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues:    []connector.Issue{waiting},
		hydratedIssues: []connector.Issue{hydratedWaiting},
		blockers:       []connector.Issue{readyBlocker, unreadyBlocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 8, 30, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none while hydrated body dependency is not ready", tracker.updates)
	}
	if !slices.Contains(tracker.identifierCalls, "digitaldrywood/detent#416") {
		t.Fatalf("identifier calls = %#v, want hydration lookup for waiting issue", tracker.identifierCalls)
	}
	if !slices.Contains(tracker.identifierCalls, "digitaldrywood/detent#414") {
		t.Fatalf("identifier calls = %#v, want body dependency lookup", tracker.identifierCalls)
	}
	if _, ok := state.Blocked[waiting.ID]; !ok {
		t.Fatalf("Blocked[%q] missing for unresolved hydrated body dependency", waiting.ID)
	}
}

func TestTickIgnoresSelfReferenceFromConnectorRefs(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-416", "Blocked")
	waiting.BlockedBy = []connector.BlockedRef{
		{Identifier: "digitaldrywood/detent#415"},
		{Identifier: "digitaldrywood/detent#416"},
	}
	hydratedWaiting := waiting
	hydratedWaiting.Description = strings.Join([]string{
		"Depends on: #414",
		"Depends on: #415",
	}, "\n")
	firstBlocker := dependencyAutoUnblockIssue("issue-414", "Done")
	firstBlocker.Identifier = "digitaldrywood/detent#414"
	secondBlocker := dependencyAutoUnblockIssue("issue-415", "Done")
	secondBlocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues:    []connector.Issue{waiting},
		hydratedIssues: []connector.Issue{hydratedWaiting},
		blockers:       []connector.Issue{firstBlocker, secondBlocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 8, 45, 0, time.UTC))

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: waiting.ID, state: "Todo"}) {
		t.Fatalf("updates = %#v, want self-reference ignored and issue moved to Todo", got)
	}
}

func TestTickLeavesTextDependencyBlockedPRIssueBlocked(t *testing.T) {
	t.Parallel()

	prNumber := 429
	waiting := dependencyAutoUnblockIssue("issue-text-dependent-pr-blocked", "Blocked")
	waiting.PRNumber = &prNumber
	waiting.PullRequest = &connector.PullRequest{
		Number:         prNumber,
		State:          "OPEN",
		HeadSHA:        "sha-current",
		MergeableState: "dirty",
	}
	waiting.BlockerReason = "Waiting on #415 before resolving PR conflicts."
	blocker := dependencyAutoUnblockIssue("issue-in-progress", "In Progress")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 9, 0, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none while text dependency is not ready", tracker.updates)
	}
	if !slices.Contains(tracker.identifierCalls, "digitaldrywood/detent#415") {
		t.Fatalf("identifier calls = %#v, want dependency lookup", tracker.identifierCalls)
	}
	if _, ok := state.Blocked[waiting.ID]; !ok {
		t.Fatalf("Blocked[%q] missing for unresolved text dependency blocker", waiting.ID)
	}
}

func TestTickAutoPromotesInactiveBlockerForDependencyWaitingIssue(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-416", "Blocked")
	waiting.Description = "Blocked by #415"
	blocker := dependencyAutoUnblockIssue("issue-415", "Backlog")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting, blocker},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	orch.cfg.BlockerAutoPromote = normalizeBlockerAutoPromoteConfig(BlockerAutoPromoteConfig{Enabled: true}, orch.cfg.ActiveStates, orch.cfg.DependencyAutoUnblock)
	state := newState(orch.cfg)
	now := time.Date(2026, 6, 12, 16, 9, 15, 0, time.UTC)

	orch.tick(context.Background(), &state, now)

	if got := tracker.updates; len(got) != 1 || got[0] != (dependencyAutoUnblockUpdate{issueID: blocker.ID, state: "Todo"}) {
		t.Fatalf("updates = %#v, want Backlog blocker moved to Todo", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one blocker promotion comment", tracker.comments)
	}
	for _, want := range []string{"Dependency blocker queued.", "Backlog to Todo", "digitaldrywood/detent#416"} {
		if !strings.Contains(tracker.comments[0].body, want) {
			t.Fatalf("comment %q missing %q", tracker.comments[0].body, want)
		}
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "blocker_auto_promote_transition" {
		t.Fatalf("RecentEvents = %#v, want blocker auto-promote event", state.RecentEvents)
	}
}

func TestTickDoesNotAutoPromoteInactiveBlockerWithOpenPullRequest(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-416", "Blocked")
	waiting.Description = "Blocked by #415"
	blocker := dependencyAutoUnblockIssue("issue-415", "Human Review")
	blocker.Identifier = "digitaldrywood/detent#415"
	blocker.PullRequest = &connector.PullRequest{
		Number: 415,
		State:  "OPEN",
		URL:    "https://github.test/digitaldrywood/detent/pull/415",
	}
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting, blocker},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	orch.cfg.BlockerAutoPromote = normalizeBlockerAutoPromoteConfig(BlockerAutoPromoteConfig{Enabled: true}, orch.cfg.ActiveStates, orch.cfg.DependencyAutoUnblock)
	state := newState(orch.cfg)

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 9, 25, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none for blocker with open PR", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none without promotion", tracker.comments)
	}
}

func TestTickAutoPromoteBlockerWaitsForAvailableCapacity(t *testing.T) {
	t.Parallel()

	waiting := dependencyAutoUnblockIssue("issue-416", "Blocked")
	waiting.Description = "Blocked by #415"
	blocker := dependencyAutoUnblockIssue("issue-415", "Backlog")
	blocker.Identifier = "digitaldrywood/detent#415"
	tracker := &dependencyAutoUnblockConnector{
		stateIssues: []connector.Issue{waiting, blocker},
		blockers:    []connector.Issue{blocker},
	}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
	orch.cfg.MaxConcurrentAgents = 1
	orch.cfg.BlockerAutoPromote = normalizeBlockerAutoPromoteConfig(BlockerAutoPromoteConfig{Enabled: true}, orch.cfg.ActiveStates, orch.cfg.DependencyAutoUnblock)
	state := newState(orch.cfg)
	running := dependencyAutoUnblockIssue("issue-running", "In Progress")
	state.Running[running.ID] = Running{Issue: running}

	orch.tick(context.Background(), &state, time.Date(2026, 6, 12, 16, 9, 30, 0, time.UTC))

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none without available capacity", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none without promotion", tracker.comments)
	}
}

func TestDependencyAutoUnblockDoesNotChangeTodoDependencyGate(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo"},
		TerminalStates:      []string{"Done"},
		DependencyAutoUnblock: DependencyAutoUnblockConfig{
			Enabled:      true,
			SourceStates: []string{"Blocked"},
			TargetState:  "Todo",
			Readiness:    DependencyReadinessTerminalOrMerged,
		},
	})
	state := newState(cfg)
	issue := dependencyAutoUnblockIssue("issue-todo", "Todo")
	issue.BlockedBy = []connector.BlockedRef{{
		Identifier: "digitaldrywood/detent#388",
		State:      "In Progress",
	}}

	planner := newDispatchPlanner(cfg)
	if _, ok, _ := planner.dispatchAction(&state, issue, time.Date(2026, 6, 12, 16, 10, 0, 0, time.UTC)); ok {
		t.Fatal("dispatchAction ok = true, want Todo issue blocked by dependency")
	}
	blocked, ok := state.Blocked[issue.ID]
	if !ok {
		t.Fatalf("Blocked[%q] missing after Todo dependency gate", issue.ID)
	}
	if blocked.Source != BlockedSourceDependency {
		t.Fatalf("Blocked source = %q, want dependency", blocked.Source)
	}
}

func TestDependencyAutoUnblockBlockerSetIgnoresRefSource(t *testing.T) {
	t.Parallel()

	nativeSet := dependencyAutoUnblockBlockerSet([]dependencyBlocker{{
		Ref: connector.BlockedRef{
			Identifier: "digitaldrywood/detent#415",
			Source:     connector.BlockedRefSourceNative,
		},
	}})
	proseSet := dependencyAutoUnblockBlockerSet([]dependencyBlocker{{
		Ref: connector.BlockedRef{
			Identifier: "digitaldrywood/detent#415",
			Source:     connector.BlockedRefSourceProse,
		},
	}})

	if nativeSet != proseSet {
		t.Fatalf("blocker set differs by source: native=%q prose=%q", nativeSet, proseSet)
	}
	if nativeSet != "digitaldrywood/detent#415" {
		t.Fatalf("blocker set = %q, want identifier only", nativeSet)
	}
}

func TestDependencyAutoUnblockBlockerSignatureIncludesReadinessAndState(t *testing.T) {
	t.Parallel()

	cfg := normalizeDependencyAutoUnblockConfig(DependencyAutoUnblockConfig{
		Readiness: DependencyReadinessTerminalOrMerged,
	})
	terminalStates := normalizedStates([]string{"Done", "Cancelled"})
	ref := connector.BlockedRef{Identifier: "digitaldrywood/detent#415"}
	open := dependencyBlocker{
		Ref:      ref,
		Issue:    dependencyAutoUnblockIssue("issue-415", "In Progress"),
		Resolved: true,
	}
	terminal := open
	terminal.Issue.State = "Done"
	terminal.Issue.StageUpdatedAt = timePointer(time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC))
	cancelled := terminal
	cancelled.Issue.State = "Cancelled"
	refreshedTerminal := terminal
	refreshedTerminal.Issue.StageUpdatedAt = timePointer(time.Date(2026, 7, 30, 17, 0, 0, 0, time.UTC))

	openSignature := dependencyAutoUnblockBlockerSignature([]dependencyBlocker{open}, cfg, terminalStates)
	terminalSignature := dependencyAutoUnblockBlockerSignature([]dependencyBlocker{terminal}, cfg, terminalStates)
	cancelledSignature := dependencyAutoUnblockBlockerSignature([]dependencyBlocker{cancelled}, cfg, terminalStates)
	refreshedTerminalSignature := dependencyAutoUnblockBlockerSignature([]dependencyBlocker{refreshedTerminal}, cfg, terminalStates)

	if openSignature == terminalSignature {
		t.Fatalf("open signature = terminal signature = %q, want readiness change to re-arm", openSignature)
	}
	if terminalSignature == cancelledSignature {
		t.Fatalf("Done signature = Cancelled signature = %q, want state change to re-arm", terminalSignature)
	}
	if terminalSignature == refreshedTerminalSignature {
		t.Fatalf("Done signatures match across state timestamps = %q, want same-state cycle to re-arm", terminalSignature)
	}
	for signature, readiness := range map[string]string{
		openSignature:     "ready=false",
		terminalSignature: "ready=true",
	} {
		if !strings.Contains(signature, readiness) {
			t.Fatalf("signature %q missing %q", signature, readiness)
		}
	}
}

func TestDependencyAutoUnblockDecisionLogIncludesBlockerSources(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	orch := &Orchestrator{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	issue := dependencyAutoUnblockIssue("issue-blocked", "Blocked")
	blockers := []dependencyBlocker{{
		Ref: connector.BlockedRef{
			Identifier: "digitaldrywood/detent#414",
			Source:     connector.BlockedRefSourceNative,
		},
	}, {
		Ref: connector.BlockedRef{
			Identifier: "digitaldrywood/detent#415",
		},
	}}

	orch.logDependencyAutoUnblockDecision(issue, "skip", "blockers_not_ready", blockers, "")

	got := logs.String()
	for _, want := range []string{
		"dependency auto-unblock decision",
		"blocker_sources=",
		"source=native",
		"source=prose",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q:\n%s", want, got)
		}
	}
}

func TestIssueWithDependencyRefsNativeOnlyIgnoresProseLines(t *testing.T) {
	t.Parallel()

	orch := &Orchestrator{cfg: normalizeConfig(Config{DependencySource: "native_only"})}
	issue := dependencyAutoUnblockIssue("issue-blocked", "Blocked")
	issue.Description = "Depends on: #415"
	issue.BlockedBy = []connector.BlockedRef{{
		Identifier: "digitaldrywood/detent#414",
		Source:     connector.BlockedRefSourceNative,
	}}

	got := orch.issueWithDependencyRefs(issue)
	want := []connector.BlockedRef{{
		Identifier: "digitaldrywood/detent#414",
		Source:     connector.BlockedRefSourceNative,
	}}
	if !slices.Equal(got.BlockedBy, want) {
		t.Fatalf("BlockedBy = %#v, want native refs only", got.BlockedBy)
	}
}

func dependencyAutoUnblockOrchestrator(
	tracker *dependencyAutoUnblockConnector,
	autoUnblock DependencyAutoUnblockConfig,
) *Orchestrator {
	cfg := normalizeConfig(Config{
		PollInterval:          time.Minute,
		MaxConcurrentAgents:   1,
		ActiveStates:          []string{"Todo", "In Progress"},
		TerminalStates:        []string{"Done", "Cancelled"},
		DependencyAutoUnblock: autoUnblock,
		BlockedRecovery: BlockedRecoveryConfig{
			Enabled:      true,
			SourceStates: []string{blockedStatusState},
			TargetState:  autoPromoteReworkState,
			ReasonCodes:  []string{blockedRecoveryReasonMergeConflict, blockedRecoveryReasonStaleBase, blockedRecoveryReasonMissingCurrentHeadCI},
		},
		ContinuationRetryDelay:     time.Second,
		FailureRetryBaseDelay:      time.Second,
		GitHubGraphQLWarnRemaining: 500,
	})
	return &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func dependencyAutoUnblockIssue(id string, state string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#" + strings.TrimPrefix(id, "issue-")
	issue.Title = "Dependency auto-unblock"
	issue.State = state
	return issue
}

type dependencyAutoUnblockUpdate struct {
	issueID string
	state   string
}

type dependencyAutoUnblockAudit struct {
	issueID string
	body    string
}

type dependencyAutoUnblockConnector struct {
	stateIssues     []connector.Issue
	hydratedIssues  []connector.Issue
	blockers        []connector.Issue
	updates         []dependencyAutoUnblockUpdate
	comments        []dependencyAutoUnblockAudit
	identifierCalls []string
}

type dependencyAutoUnblockRejectOnceConnector struct {
	*dependencyAutoUnblockConnector
	reject bool
}

func (c *dependencyAutoUnblockRejectOnceConnector) UpdateIssueState(ctx context.Context, issueID string, state string) error {
	if c.reject {
		c.updates = append(c.updates, dependencyAutoUnblockUpdate{issueID: issueID, state: state})
		return connector.ErrStateUpdateBlocked
	}
	return c.dependencyAutoUnblockConnector.UpdateIssueState(ctx, issueID, state)
}

func (c *dependencyAutoUnblockConnector) Name() string {
	return "dependency-auto-unblock"
}

func (c *dependencyAutoUnblockConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *dependencyAutoUnblockConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	return issuesInStates(c.stateIssues, states), nil
}

func (c *dependencyAutoUnblockConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *dependencyAutoUnblockConnector) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) ([]connector.Issue, error) {
	wanted := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		normalized := strings.ToLower(strings.TrimSpace(identifier))
		wanted[normalized] = struct{}{}
		c.identifierCalls = append(c.identifierCalls, normalized)
	}
	out := make([]connector.Issue, 0, len(c.hydratedIssues)+len(c.blockers))
	for _, issue := range append(c.hydratedIssues, c.blockers...) {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.Identifier))]; ok {
			out = append(out, cloneIssue(issue))
		}
	}
	return out, nil
}

func (c *dependencyAutoUnblockConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, dependencyAutoUnblockAudit{issueID: issueID, body: body})
	return nil
}

func (c *dependencyAutoUnblockConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, dependencyAutoUnblockUpdate{issueID: issueID, state: state})
	return nil
}

func (c *dependencyAutoUnblockConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *dependencyAutoUnblockConnector) SetField(context.Context, string, string, string) error {
	return nil
}
