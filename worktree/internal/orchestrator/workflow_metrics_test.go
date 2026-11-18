package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type unavailableLaneMetricsConnector struct {
	*workflowMetricsConnector
	unavailableID string
	laneError     error
	entry         connector.IssueStateTransition
	updates       []struct{ id, state string }
}

func (c *unavailableLaneMetricsConnector) CurrentLaneEntry(_ context.Context, issue connector.Issue) (connector.IssueStateTransition, bool, error) {
	if issue.ID == c.unavailableID {
		return c.entry, !c.entry.EnteredAt.IsZero(), c.laneError
	}
	return connector.IssueStateTransition{}, false, nil
}

func (c *unavailableLaneMetricsConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	return cloneIssue(issue), nil
}

func (c *unavailableLaneMetricsConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	var issues []connector.Issue
	for _, id := range ids {
		for _, issue := range c.stateIssues {
			if issue.ID == id {
				issues = append(issues, cloneIssue(issue))
			}
		}
	}
	return issues, nil
}

func (c *unavailableLaneMetricsConnector) UpdateIssueState(ctx context.Context, id, state string) error {
	c.updates = append(c.updates, struct{ id, state string }{id, state})
	return c.workflowMetricsConnector.UpdateIssueState(ctx, id, state)
}

func TestTickPreservesOrchestratorTransitionSnapshot(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name                           string
		canonicalFailure, rejectedHead bool
	}{
		{name: "tracker snapshot"},
		{name: "unreadable peer does not stop reconciliation", canonicalFailure: true},
		{name: "unreadable human Rework excludes unchanged head", canonicalFailure: true, rejectedHead: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			canonicalFailure := scenario.canonicalFailure
			now := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
			issue := connector.Issue{
				ID:           "issue-closed",
				Identifier:   "digitaldrywood/detent#1131",
				State:        "Human Review",
				Closed:       true,
				ClosedReason: "completed",
			}
			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
				ObservedStates:      []string{"Blocked", "Done"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			tracker := &workflowMetricsConnector{stateIssues: []connector.Issue{cloneIssue(issue)}}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			state := newState(cfg)
			state.BoardIssues = []connector.Issue{cloneIssue(issue)}
			state.Pipeline = []connector.Issue{cloneIssue(issue)}

			var held connector.Issue
			var priorEntry time.Time
			var priorOrigin provenance.Attribution
			var priorKey string
			var canonicalSource *unavailableLaneMetricsConnector
			if canonicalFailure {
				recorded := now.Add(-2 * time.Hour)
				held = connector.Issue{ID: "issue-history-unavailable", Identifier: "owner/repository#2", Title: "Held work", State: "Todo", AssignedToWorker: true, Comments: []connector.IssueComment{{Body: implementProgressWorkpadComment("", "manual takeover hold"), CreatedAt: &recorded}}}
				if scenario.rejectedHead {
					held.State, held.Comments = "Rework", nil
					held.PullRequest = &connector.PullRequest{Number: 42, URL: "https://github.com/owner/repository/pull/42", State: "OPEN", HeadSHA: "rejected", MergeableState: "CLEAN", CIStatus: "success"}
					cfg.AutoPromote = normalizeAutoPromoteConfig(AutoPromoteConfig{Enabled: true, PassState: "Human Review", GateWaitState: "review", Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}})
					cfg.WorkerHosts = []string{"worker-a"}
					orch.cfg = cfg
					orch.workflowMetrics = &autoPromoteWorkflowMetricsRecorder{}
					orch.supervisor = newTestSupervisor(t, newWorkerHostRunner(), cfg)
					orch.runResults = make(chan runpkg.Completion, 2)
					t.Cleanup(func() {
						for _, running := range state.Running {
							running.cancel()
						}
						for _, running := range state.Running {
							<-orch.runResults
							<-running.done
						}
					})
					prior := cloneIssue(held)
					prior.State = "Human Review"
					state.Pipeline = append(state.Pipeline, prior)
					state.epicTransitionWatch = []connector.Issue{prior}
					state.pendingEpicParentLookups = map[string]connector.Issue{held.ID: prior}
					state.Retry[held.ID] = Retry{Issue: held, Attempt: 4, DueAt: now.Add(-time.Minute)}
				}
				tracker.candidates = []connector.Issue{held, cloneIssue(issue)}
				state.BoardIssues = append([]connector.Issue{held}, state.BoardIssues...)
				if scenario.rejectedHead {
					state.BoardIssues[0].State = "Human Review"
					tracker.stateIssues = append([]connector.Issue{held}, tracker.stateIssues...)
				}
				priorKey = workflowLaneEntryKey(state.BoardIssues[0])
				orch.refreshCurrentLaneEntries(t.Context(), &state, now.Add(-time.Hour))
				priorEntry = state.laneEntries[priorKey]
				priorOrigin = state.laneProvenance[priorKey]
				state.BoardIssues[0] = held
				canonicalSource = &unavailableLaneMetricsConnector{workflowMetricsConnector: tracker, unavailableID: held.ID, laneError: errors.New("current lane entry history is incomplete")}
				orch.connector = canonicalSource
			}
			orch.tick(context.Background(), &state, now)

			snapshot := state.Snapshot(now.Add(time.Second))
			wantIssues := 1
			if canonicalFailure {
				wantIssues = 2
			}
			if len(snapshot.BoardIssues) != wantIssues {
				t.Fatalf("snapshot BoardIssues len = %d, want %d", len(snapshot.BoardIssues), wantIssues)
			}
			findIssue := func(issues []telemetry.Issue, id string) telemetry.Issue {
				t.Helper()
				for _, current := range issues {
					if current.ID == id {
						return current
					}
				}
				t.Fatalf("issue %s missing from snapshot", id)
				return telemetry.Issue{}
			}
			if got := findIssue(snapshot.BoardIssues, issue.ID).State; got != "Done" {
				t.Fatalf("snapshot BoardIssues state = %q, want Done", got)
			}
			wantPipeline := 1
			if scenario.rejectedHead {
				wantPipeline = 2
			}
			if len(snapshot.Pipeline) != wantPipeline {
				t.Fatalf("snapshot Pipeline len = %d, want %d", len(snapshot.Pipeline), wantPipeline)
			}
			if got := findIssue(snapshot.Pipeline, issue.ID).State; got != "Done" {
				t.Fatalf("snapshot Pipeline state = %q, want Done", got)
			}

			external := cloneIssue(issue)
			external.Closed = false
			external.ClosedReason = ""
			tracker.stateIssues = []connector.Issue{external}
			if scenario.rejectedHead {
				tracker.stateIssues = append([]connector.Issue{held}, tracker.stateIssues...)
			}
			if canonicalFailure {
				tracker.candidates = []connector.Issue{held, cloneIssue(external)}
			}
			orch.tick(context.Background(), &state, now.Add(time.Minute))

			snapshot = state.Snapshot(now.Add(time.Minute + time.Second))
			if len(snapshot.BoardIssues) != wantIssues || findIssue(snapshot.BoardIssues, issue.ID).State != "Human Review" {
				t.Fatalf("snapshot BoardIssues = %#v, want external Human Review state", snapshot.BoardIssues)
			}
			if len(snapshot.Pipeline) != wantPipeline || findIssue(snapshot.Pipeline, issue.ID).State != "Human Review" {
				t.Fatalf("snapshot Pipeline = %#v, want external Human Review state", snapshot.Pipeline)
			}
			if canonicalFailure {
				unknown := findIssue(snapshot.BoardIssues, held.ID)
				if !state.laneEntries[priorKey].Equal(priorEntry) || state.laneProvenance[priorKey] != priorOrigin {
					t.Fatalf("failed history changed prior lane evidence: entries=%+v provenance=%+v", state.laneEntries, state.laneProvenance)
				}
				if scenario.rejectedHead {
					if unknown.CurrentLaneEnteredAt != nil {
						t.Fatalf("failed human Rework gained a fabricated lane time: %+v", unknown)
					}
				} else if unknown.CurrentLaneEnteredAt == nil || !unknown.CurrentLaneEnteredAt.Equal(priorEntry) {
					t.Fatalf("failed history changed the prior lane time: %+v", unknown)
				}
				if _, running := state.Running[held.ID]; running {
					t.Fatal("unreadable history released the human hold")
				}
				if scenario.rejectedHead {
					for _, update := range canonicalSource.updates {
						if update.id == held.ID {
							t.Fatalf("failed human Rework was mutated: %+v", update)
						}
					}
					if unknown.State != "Rework" {
						t.Fatalf("unobserved human Rework was promoted: %+v", unknown)
					}
					if retry, exists := state.Retry[held.ID]; !exists || retry.Attempt != 4 || !retry.DueAt.Equal(now.Add(-time.Minute)) {
						t.Fatalf("failed observation changed retry ownership: %+v, %t", retry, exists)
					}
					if prior := findIssue(snapshot.Pipeline, held.ID); prior.State != "Human Review" {
						t.Fatalf("failed observation lost prior pipeline context: %+v", prior)
					}
					if len(state.epicTransitionWatch) != 1 || state.epicTransitionWatch[0].ID != held.ID || state.epicTransitionWatch[0].State != "Human Review" || state.pendingEpicParentLookups[held.ID].State != "Human Review" {
						t.Fatalf("failed observation lost retained watch context: watch=%+v parents=%+v", state.epicTransitionWatch, state.pendingEpicParentLookups)
					}
					canonicalSource.laneError = nil
					canonicalSource.entry = connector.IssueStateTransition{EnteredAt: now.Add(-time.Minute), Actor: connector.IssueActor{Kind: "human", Login: "example-user"}}
					orch.tick(t.Context(), &state, now.Add(2*time.Minute))
					if entry := state.laneEntries[workflowLaneEntryKey(held)]; !entry.Equal(canonicalSource.entry.EnteredAt) {
						t.Fatalf("recovered history did not refresh the lane entry: %s", entry)
					}
					for _, update := range canonicalSource.updates {
						if update.id == held.ID && update.state != "Rework" {
							t.Fatalf("recovered human rejection promoted the same head: %+v", update)
						}
					}
				} else {
					decisionState := newState(cfg)
					decision := orch.liveDispatchPlanner(t.Context(), nil).dispatchableIssueDecision(held, &decisionState, false, now, "")
					if decision.dispatchable || decision.reason != dispatchSkipBlockedByDependency {
						t.Fatalf("unreadable history changed human authorization: %+v", decision)
					}
				}
			}
		})
	}
}

func TestApplyAutoPromoteDecisionUpdatesSnapshotBeforePoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		trackerName string
	}{
		{name: "label", trackerName: "github_label"},
		{name: "ProjectV2", trackerName: "github_project_v2"},
		{name: "issue field", trackerName: "github_issue_field"},
		{name: "local", trackerName: "local"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transitionAt := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
			previousStageAt := transitionAt.Add(-5 * time.Minute)
			issue := connector.Issue{
				ID:             "issue-promote",
				Identifier:     "digitaldrywood/detent#1131",
				State:          "In Progress",
				StageUpdatedAt: &previousStageAt,
			}
			tracker := &workflowMetricsConnector{name: tt.trackerName, stateEnteredAt: transitionAt, stateEnteredAtFound: true}
			orch := &Orchestrator{connector: tracker}
			state := newState(Config{})
			state.BoardIssues = []connector.Issue{cloneIssue(issue)}
			state.Pipeline = []connector.Issue{cloneIssue(issue)}

			applied := orch.applyAutoPromoteDecision(
				context.Background(),
				&state,
				issue,
				AutoPromoteSummary{},
				autoPromoteDecision(AutoPromoteActionPromote, AutoPromoteReasonReady),
				"Merging",
				transitionAt,
			)
			if !applied {
				t.Fatal("applyAutoPromoteDecision() = false, want true")
			}

			snapshot := state.Snapshot(transitionAt.Add(time.Second))
			if len(snapshot.BoardIssues) != 1 {
				t.Fatalf("snapshot BoardIssues len = %d, want 1", len(snapshot.BoardIssues))
			}
			if got := snapshot.BoardIssues[0].State; got != "Merging" {
				t.Fatalf("snapshot BoardIssues state = %q, want Merging", got)
			}
			if snapshot.BoardIssues[0].StageUpdatedAt == nil || !snapshot.BoardIssues[0].StageUpdatedAt.Equal(transitionAt) {
				t.Fatalf("snapshot BoardIssues StageUpdatedAt = %v, want %v", snapshot.BoardIssues[0].StageUpdatedAt, transitionAt)
			}
			if len(snapshot.Pipeline) != 1 {
				t.Fatalf("snapshot Pipeline len = %d, want 1", len(snapshot.Pipeline))
			}
			if got := snapshot.Pipeline[0].State; got != "Merging" {
				t.Fatalf("snapshot Pipeline state = %q, want Merging", got)
			}
			if tracker.fetches != 0 {
				t.Fatalf("tracker fetches = %d, want none", tracker.fetches)
			}
		})
	}
}

func TestResolveCurrentLaneEnteredAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC)
	createdAt := now.Add(-4 * time.Hour)
	enteredAt := now.Add(-time.Hour)
	transitionAt := now.Add(-10 * time.Minute)
	updatedAt := now.Add(-5 * time.Minute)

	tests := []struct {
		name             string
		issue            connector.Issue
		previous         time.Time
		trackerEnteredAt time.Time
		observedAt       time.Time
		events           []store.WorkflowPhaseEvent
		want             time.Time
	}{
		{
			name: "same-lane update keeps previous entry",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			previous: enteredAt,
			want:     enteredAt,
		},
		{
			name: "same-lane event may not move entry forward",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			previous: enteredAt,
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: updatedAt},
			},
			want: enteredAt,
		},
		{
			name: "agent-moved lane uses tracker transition",
			issue: connector.Issue{
				State:     "Blocked",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			trackerEnteredAt: transitionAt,
			observedAt:       now,
			want:             transitionAt,
		},
		{
			name:       "missing tracker and phase event uses poll observation",
			issue:      connector.Issue{State: "Blocked"},
			observedAt: now,
			want:       now,
		},
		{
			name: "lane change uses transition event",
			issue: connector.Issue{
				State:     "Merging",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: enteredAt},
				{ID: 2, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "merging", Status: "ENTERED", StartedAt: transitionAt},
			},
			want: transitionAt,
		},
		{
			name: "leave and return uses latest durable entry",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			previous: enteredAt,
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: enteredAt},
				{ID: 2, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Merging", Status: "entered", StartedAt: transitionAt.Add(-time.Minute)},
				{ID: 3, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "In Progress", Status: "entered", StartedAt: transitionAt},
			},
			want: transitionAt,
		},
		{
			name: "tracker reentry supersedes stale durable entry",
			issue: connector.Issue{
				State:     "Human Review",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			previous:         enteredAt,
			trackerEnteredAt: transitionAt,
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Human Review", Status: "entered", StartedAt: enteredAt},
			},
			want: transitionAt,
		},
		{
			name: "restart restores durable entry",
			issue: connector.Issue{
				State:     "In Progress",
				CreatedAt: &createdAt,
				UpdatedAt: &updatedAt,
			},
			events: []store.WorkflowPhaseEvent{
				{ID: 1, PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "in progress", Status: "entered", StartedAt: enteredAt},
			},
			want: enteredAt,
		},
		{
			name: "missing phase events uses tracker fallback",
			issue: connector.Issue{
				State:     "Todo",
				CreatedAt: &createdAt,
				UpdatedAt: &enteredAt,
			},
			want: enteredAt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveCurrentLaneEnteredAt(tt.issue, tt.previous, tt.trackerEnteredAt, tt.observedAt, tt.events)
			if !got.Equal(tt.want) {
				t.Fatalf("resolveCurrentLaneEnteredAt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObservedLaneAttribution(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{ID: "issue-1761"}
	operatorActor := connector.IssueActor{Login: "example-user", Kind: "User"}
	activeAgent := newState(Config{})
	activeAgent.Running[issue.ID] = Running{Issue: issue}
	tests := []struct {
		name          string
		state         *State
		actor         connector.IssueActor
		wantOrigin    provenance.Origin
		wantInitiator provenance.Initiator
	}{
		{name: "active worker with user actor remains indeterminate", state: &activeAgent, actor: operatorActor, wantOrigin: provenance.OriginIndeterminate, wantInitiator: provenance.InitiatorIndeterminate},
		{name: "unverified user actor", state: statePointer(newState(Config{})), actor: operatorActor, wantOrigin: provenance.OriginIndeterminate, wantInitiator: provenance.InitiatorIndeterminate},
		{name: "external bot", state: statePointer(newState(Config{})), actor: connector.IssueActor{Login: "audit[bot]", Kind: "Bot"}, wantOrigin: provenance.OriginAutomation, wantInitiator: provenance.InitiatorExternalAutomation},
		{name: "missing state and actor", wantOrigin: provenance.OriginIndeterminate, wantInitiator: provenance.InitiatorIndeterminate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observedLaneAttribution(tt.state, issue, tt.actor)
			if got.Origin != tt.wantOrigin || got.Initiator != tt.wantInitiator {
				t.Fatalf("observedLaneAttribution() = %#v, want origin %q initiator %q", got, tt.wantOrigin, tt.wantInitiator)
			}
		})
	}
}

func statePointer(state State) *State {
	return &state
}

func TestRefreshCurrentLaneEntriesPersistsPollObservationAcrossRestart(t *testing.T) {
	ctx := t.Context()
	enteredAt := time.Date(2026, 7, 10, 14, 36, 55, 0, time.UTC)
	updatedAt := enteredAt.Add(2 * time.Hour)

	backend := newRuntimeFixture()
	state := newState(normalizeConfig(Config{}))
	state.BoardIssues = []connector.Issue{{
		ID:         "issue-1162",
		Identifier: "digitaldrywood/detent#1162",
		State:      "Blocked",
	}}
	orch := &Orchestrator{cfg: normalizeConfig(Config{}), workflowMetrics: backend}
	orch.refreshCurrentLaneEntries(ctx, &state, enteredAt)

	timeline, err := backend.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: defaultWorkflowMetricsProjectID, IssueID: "issue-1162"})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	if len(timeline.Events) != 0 {
		t.Fatalf("initial observation wrote phase events: %#v", timeline.Events)
	}

	restarted := backend
	restartedState := newState(normalizeConfig(Config{}))
	restartedState.BoardIssues = []connector.Issue{{
		ID:         "issue-1162",
		Identifier: "digitaldrywood/detent#1162",
		State:      "Blocked",
		UpdatedAt:  &updatedAt,
	}}
	restartedOrch := &Orchestrator{cfg: normalizeConfig(Config{}), workflowMetrics: restarted}
	restartedOrch.refreshCurrentLaneEntries(ctx, &restartedState, updatedAt.Add(time.Minute))

	snapshot := restartedState.Snapshot(updatedAt.Add(time.Minute))
	if got := snapshot.BoardIssues[0].CurrentLaneEnteredAt; got == nil || !got.Equal(enteredAt) {
		t.Fatalf("CurrentLaneEnteredAt after restart = %v, want %v", got, enteredAt)
	}
}

func TestRefreshCurrentLaneEntriesUsesTrackerTransitionAcrossPollsAndRestart(t *testing.T) {
	ctx := t.Context()
	enteredAt := time.Date(2026, 7, 10, 14, 36, 55, 0, time.UTC)
	firstPollAt := enteredAt.Add(2 * time.Hour)
	secondPollAt := firstPollAt.Add(2 * time.Minute)
	updatedAt := firstPollAt
	tracker := &workflowMetricsConnector{stateEnteredAt: enteredAt, stateEnteredAtFound: true}

	backend := newRuntimeFixture()
	state := newState(normalizeConfig(Config{}))
	state.BoardIssues = []connector.Issue{{
		ID:         "issue-1162",
		Identifier: "digitaldrywood/detent#1162",
		State:      "Blocked",
		UpdatedAt:  &updatedAt,
	}}
	orch := &Orchestrator{
		cfg:             normalizeConfig(Config{AdmissionTargetState: "Blocked"}),
		connector:       tracker,
		workflowMetrics: backend,
	}
	orch.refreshCurrentLaneEntries(ctx, &state, firstPollAt)
	first := state.Snapshot(firstPollAt)
	if got := first.BoardIssues[0].CurrentLaneEnteredAt; got == nil || !got.Equal(enteredAt) {
		t.Fatalf("first CurrentLaneEnteredAt = %v, want %v", got, enteredAt)
	}
	if got := first.BoardIssues[0].Origin; got != string(provenance.OriginHuman) {
		t.Fatalf("first Origin = %q, want %q", got, provenance.OriginIndeterminate)
	}

	updatedAt = secondPollAt
	state.BoardIssues[0].UpdatedAt = &updatedAt
	orch.refreshCurrentLaneEntries(ctx, &state, secondPollAt)
	second := state.Snapshot(secondPollAt)
	if got := second.BoardIssues[0].CurrentLaneEnteredAt; got == nil || !got.Equal(enteredAt) {
		t.Fatalf("second CurrentLaneEnteredAt = %v, want %v", got, enteredAt)
	}
	if tracker.stateEnteredAtCalls != 1 {
		t.Fatalf("IssueStateEnteredAt() calls = %d, want 1 after durable healing", tracker.stateEnteredAtCalls)
	}

	restarted := backend
	restartedState := newState(normalizeConfig(Config{}))
	restartedState.BoardIssues = cloneIssues(state.BoardIssues)
	restartedOrch := &Orchestrator{cfg: normalizeConfig(Config{}), connector: tracker, workflowMetrics: restarted}
	restartedOrch.refreshCurrentLaneEntries(ctx, &restartedState, secondPollAt.Add(time.Minute))
	restartedSnapshot := restartedState.Snapshot(secondPollAt.Add(time.Minute))
	if got := restartedSnapshot.BoardIssues[0].CurrentLaneEnteredAt; got == nil || !got.Equal(enteredAt) {
		t.Fatalf("restarted CurrentLaneEnteredAt = %v, want %v", got, enteredAt)
	}
	if tracker.stateEnteredAtCalls != 1 {
		t.Fatalf("IssueStateEnteredAt() calls after restart = %d, want persisted event to avoid another call", tracker.stateEnteredAtCalls)
	}
}

func TestRefreshCurrentLaneEntriesUsesHydratedTrackerReentry(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"hydrated timestamp", "canonical entry"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			firstEntry := time.Date(2026, 8, 17, 4, 53, 35, 0, time.UTC)
			latestEntry := time.Date(2026, 8, 18, 22, 30, 8, 0, time.UTC)
			backend := &autoPromoteWorkflowMetricsRecorder{}
			if _, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
				ProjectID:  "corp",
				IssueID:    "I_74",
				Identifier: "gopherguides/corp#74",
				PhaseType:  store.WorkflowPhaseTypeLane,
				PhaseName:  "Human Review",
				Status:     "entered",
				StartedAt:  firstEntry,
			}); err != nil {
				t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
			}
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "corp"}})
			state := newState(cfg)
			state.laneEntries["id:I_74\x00human review"] = firstEntry
			state.BoardIssues = []connector.Issue{{
				ID:                "I_74",
				Identifier:        "gopherguides/corp#74",
				State:             "Human Review",
				StageUpdatedAt:    &latestEntry,
				StageUpdatedActor: connector.IssueActor{Login: "example-user", Kind: "User"},
			}}
			orch := &Orchestrator{cfg: cfg, workflowMetrics: backend}
			if source == "canonical entry" {
				state.BoardIssues[0].StageUpdatedAt = nil
				orch.connector = &laneEntryConnector{implementProgressConnector: &implementProgressConnector{refreshed: state.BoardIssues[0], hydrated: state.BoardIssues[0]}, entry: connector.IssueStateTransition{EnteredAt: latestEntry, Actor: state.BoardIssues[0].StageUpdatedActor}, found: true}
			}

			orch.refreshCurrentLaneEntries(ctx, &state, latestEntry.Add(time.Hour))
			snapshot := state.Snapshot(latestEntry.Add(time.Hour))
			if got := snapshot.BoardIssues[0].CurrentLaneEnteredAt; got == nil || !got.Equal(latestEntry) {
				t.Fatalf("CurrentLaneEnteredAt = %v, want %v", got, latestEntry)
			}
			timeline, err := backend.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: "corp", IssueID: "I_74"})
			if err != nil {
				t.Fatalf("IssueWorkflowTimeline() error = %v", err)
			}
			latest, ok := latestCurrentLaneEntryForAt(timeline.Events, "Human Review", latestEntry)
			if !ok {
				t.Fatalf("latest lane entry missing from timeline: %#v", timeline.Events)
			}
			metadata, ok := provenance.Parse(latest.MetadataJSON)
			if !ok || metadata.Provenance.Origin != provenance.OriginHuman {
				t.Fatalf("latest lane provenance = %#v, want hydrated tracker actor", metadata.Provenance)
			}
			restarted := &Orchestrator{cfg: cfg, connector: orch.connector}
			restartedState := newState(cfg)
			restartedState.BoardIssues = append([]connector.Issue(nil), state.BoardIssues...)
			restarted.refreshCurrentLaneEntries(ctx, &restartedState, latestEntry.Add(2*time.Hour))
			if got := restartedState.Snapshot(latestEntry.Add(2 * time.Hour)).BoardIssues[0].CurrentLaneEnteredAt; got == nil || !got.Equal(latestEntry) {
				t.Fatalf("restarted CurrentLaneEnteredAt = %v, want %v", got, latestEntry)
			}
		})
	}
}

func TestRefreshCurrentLaneEntriesSurvivesStoreRestart(t *testing.T) {
	ctx := t.Context()
	enteredAt := time.Date(2026, 7, 9, 17, 0, 0, 0, time.UTC)

	seed := newRuntimeFixture()
	if _, err := seed.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
		ProjectID: "detent",
		IssueID:   "issue-1130",
		PhaseType: store.WorkflowPhaseTypeLane,
		PhaseName: "In Progress",
		Status:    "entered",
		StartedAt: enteredAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}

	for index, updatedAt := range []time.Time{enteredAt.Add(30 * time.Minute), enteredAt.Add(time.Hour)} {
		backend := seed

		cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}})
		state := newState(cfg)
		state.BoardIssues = []connector.Issue{{
			ID:        "issue-1130",
			State:     "In Progress",
			UpdatedAt: &updatedAt,
		}}
		orch := &Orchestrator{cfg: cfg, workflowMetrics: backend}
		orch.refreshCurrentLaneEntries(ctx, &state, updatedAt)
		snapshot := state.Snapshot(updatedAt.Add(time.Minute))
		if snapshot.BoardIssues[0].CurrentLaneEnteredAt == nil || !snapshot.BoardIssues[0].CurrentLaneEnteredAt.Equal(enteredAt) {
			t.Errorf("restart %d CurrentLaneEnteredAt = %v, want %v", index+1, snapshot.BoardIssues[0].CurrentLaneEnteredAt, enteredAt)
		}
	}
}

func TestUpdateIssueStateByIDSkipsWorkflowMetricsForBlockedUpdate(t *testing.T) {
	t.Parallel()

	recorder := &workflowMetricsRecorderSpy{}
	orch := &Orchestrator{
		connector: &workflowMetricsConnector{
			err: &connector.StateUpdateBlockedError{
				IssueID:      "issue-blocked",
				CurrentState: "Done",
				TargetState:  "Todo",
			},
		},
		workflowMetrics: recorder,
	}
	state := newState(Config{})
	state.BoardIssues = []connector.Issue{{ID: "issue-blocked", State: "Done"}}

	err := orch.updateIssueStateByID(
		context.Background(),
		&state,
		"issue-blocked",
		connector.Issue{
			ID:         "issue-blocked",
			Identifier: "digitaldrywood/detent#100",
			State:      "Done",
		},
		"Todo",
		time.Now(),
		"test",
	)
	if err != nil {
		t.Fatalf("updateIssueStateByID() error = %v, want nil", err)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("workflow metric events = %#v, want none", recorder.events)
	}
	if got := state.BoardIssues[0].State; got != "Done" {
		t.Fatalf("snapshot BoardIssues state = %q, want Done", got)
	}
}

func TestUpdateIssueStateRecordsTrackerMutationConfirmation(t *testing.T) {
	t.Parallel()

	requestedAt := time.Date(2026, 8, 25, 20, 13, 0, 0, time.UTC)
	trackerStageAt := requestedAt.Add(2 * time.Second)
	responseAt := requestedAt.Add(3 * time.Second)
	issue := connector.Issue{
		ID:         "issue-mutation-confirmation",
		Identifier: "digitaldrywood/detent#1987",
		State:      "Human Review",
	}
	recorder := &autoPromoteWorkflowMetricsRecorder{}
	tracker := &workflowMetricsConnector{
		stateEnteredAt:      trackerStageAt,
		stateEnteredAtFound: true,
	}
	orch := &Orchestrator{
		cfg:             normalizeConfig(Config{}),
		connector:       tracker,
		workflowMetrics: recorder,
		now:             func() time.Time { return responseAt },
	}
	state := newState(orch.cfg)
	state.BoardIssues = []connector.Issue{cloneIssue(issue)}
	issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "Approve the requested scope"}
	metadata := workflowLaneMetadata{BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{Owner: "human", Cause: "human_action", SourceRevision: holdSourceRevision(issue), ExpiresAt: time.Now().Add(time.Hour)}}

	if err := orch.updateIssueStateByIDWithMetadata(
		t.Context(),
		&state,
		issue.ID,
		issue,
		blockedStatusState,
		requestedAt,
		"rework_limit",
		metadata,
	); err != nil {
		t.Fatalf("updateIssueStateByIDWithMetadata() error = %v", err)
	}

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("workflow events = %#v, want exit and entry", events)
	}
	entered := events[1]
	gotMetadata, ok := workflowLaneMetadataFromJSON(entered.MetadataJSON)
	if !ok || gotMetadata.TrackerMutationAt != trackerStageAt.Format(time.RFC3339Nano) {
		t.Fatalf("tracker mutation confirmation = %q, want tracker time %q instead of response time %q", gotMetadata.TrackerMutationAt, trackerStageAt.Format(time.RFC3339Nano), responseAt.Format(time.RFC3339Nano))
	}

	current := cloneIssue(issue)
	current.State = blockedStatusState
	current.StageUpdatedAt = &trackerStageAt
	state.BoardIssues = []connector.Issue{current}
	orch.refreshCurrentLaneEntries(t.Context(), &state, requestedAt.Add(time.Minute))
	if got := len(recorder.snapshot()); got != len(events) {
		t.Fatalf("workflow event count after tracker reconciliation = %d, want %d", got, len(events))
	}
}

func TestDetentLaneWriteEchoKeepsWriter(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name            string
		target          string
		labelTransition bool
	}{
		{"label blocked", "Blocked", true},
		{"label rework", "Rework", true},
		{"label merging", "Merging", true},
		{"project blocked", "Blocked", false},
		{"project rework", "Rework", false},
		{"project merging", "Merging", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := tt.target
			t.Parallel()
			requestedAt := time.Date(2026, 9, 4, 4, 11, 0, 0, time.UTC)
			trackerAt := requestedAt.Add(2 * time.Second)
			issue := laneRevocationIssue("echo", "digitaldrywood/detent#2138", "In Progress")
			if target == blockedStatusState {
				issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "Approve requested scope"}
			}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			confirmed := cloneIssue(issue)
			confirmed.State = target
			confirmed.StageUpdatedAt = &trackerAt
			tracker := &workflowMetricsConnector{stateEnteredAt: trackerAt, stateEnteredAtFound: tt.labelTransition, stateIssues: []connector.Issue{confirmed}}
			orch := &Orchestrator{cfg: normalizeConfig(Config{}), connector: tracker, workflowMetrics: metrics}
			state := newState(orch.cfg)
			state.BoardIssues = []connector.Issue{issue}
			if err := orch.updateIssueState(t.Context(), &state, issue, target, requestedAt, "machine_transition"); err != nil {
				t.Fatal(err)
			}
			issue.State = target
			issue.StageUpdatedAt = &trackerAt
			issue.StageUpdatedActor = connector.IssueActor{Login: "shared-token", Kind: "User"}
			state.BoardIssues = []connector.Issue{issue}
			orch.refreshCurrentLaneEntries(t.Context(), &state, requestedAt.Add(time.Minute))
			if got := len(metrics.snapshot()); got != 2 {
				t.Fatalf("events after write echo = %d, want only the original exit and entry", got)
			}
			if got := state.laneProvenance[workflowLaneEntryKey(issue)]; got.Origin != provenance.OriginDetent || got.Basis != provenance.BasisDetentOperation {
				t.Fatalf("write echo attribution = %#v, want Detent", got)
			}
			later := trackerAt.Add(time.Minute)
			issue.StageUpdatedAt = &later
			state.BoardIssues = []connector.Issue{issue}
			orch.refreshCurrentLaneEntries(t.Context(), &state, later)
			if got := state.laneProvenance[workflowLaneEntryKey(issue)]; got.Origin != provenance.OriginHuman {
				t.Fatalf("later shared-token reentry attribution = %#v, want human", got)
			}
		})
	}
}

func TestReworkTransitionsKeepDatabaseOwnership(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)
	recorder := &workflowMetricsRecorderSpy{}
	for _, projectID := range []string{"first-project", "second-project"} {
		cfg := Config{Project: scheduler.ProjectCandidate{ID: projectID}}
		orch := &Orchestrator{cfg: cfg, connector: &workflowMetricsConnector{}, workflowMetrics: recorder}
		state := newState(cfg)
		issue := connector.Issue{
			ID: projectID + "-issue", Identifier: projectID + "#1", State: "In Progress",
			PullRequest: &connector.PullRequest{Number: 42, HeadSHA: projectID + "-head"},
		}
		if err := orch.updateIssueStateByID(t.Context(), &state, issue.ID, issue, "Rework", at, "merge_conflicts"); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.events) != 4 {
		t.Fatalf("events = %d, want both lane exit/entry pairs", len(recorder.events))
	}
	for index, projectID := range []string{"first-project", "second-project"} {
		event := recorder.events[index*2+1]
		if event.ProjectID != projectID || event.IssueID != projectID+"-issue" || event.PhaseName != "Rework" || event.Status != "entered" {
			t.Fatalf("entry = %#v, want project-owned rework entry", event)
		}
		var metadata workflowLaneMetadata
		if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Provenance.Origin != provenance.OriginDetent || metadata.PullRequest == nil {
			t.Fatalf("metadata = %#v, want durable Detent/PR evidence", metadata)
		}
	}
}

type workflowMetricsRecorderSpy struct {
	events []store.WorkflowPhaseEvent
}

func (r *workflowMetricsRecorderSpy) RecordWorkflowPhaseEvent(_ context.Context, event store.WorkflowPhaseEvent) (int64, error) {
	r.events = append(r.events, event)
	return int64(len(r.events)), nil
}

type workflowMetricsConnector struct {
	name                string
	err                 error
	fetches             int
	candidates          []connector.Issue
	stateIssues         []connector.Issue
	stateEnteredAt      time.Time
	stateEnteredAtFound bool
	stateEnteredAtErr   error
	stateEnteredAtCalls int
}

func (c *workflowMetricsConnector) Name() string {
	return c.name
}

func (c *workflowMetricsConnector) IssueStateTransition(context.Context, connector.Issue) (connector.IssueStateTransition, bool, error) {
	c.stateEnteredAtCalls++
	return connector.IssueStateTransition{
		EnteredAt: c.stateEnteredAt,
		Actor: connector.IssueActor{
			Login: "ada",
			Kind:  "User",
		},
	}, c.stateEnteredAtFound, c.stateEnteredAtErr
}

func (c *workflowMetricsConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.fetches++
	return cloneIssues(c.candidates), nil
}

func (c *workflowMetricsConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetches++
	return issuesInStates(c.stateIssues, states), nil
}

func (c *workflowMetricsConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	c.fetches++
	return cloneIssues(c.stateIssues), nil
}

func (c *workflowMetricsConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *workflowMetricsConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	if c.err != nil {
		return c.err
	}
	for index := range c.candidates {
		if c.candidates[index].ID == issueID {
			c.candidates[index].State = state
		}
	}
	for index := range c.stateIssues {
		if c.stateIssues[index].ID == issueID {
			c.stateIssues[index].State = state
		}
	}
	return nil
}

func (c *workflowMetricsConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *workflowMetricsConnector) SetField(context.Context, string, string, string) error {
	return nil
}

func TestRefreshCurrentLaneEntriesOperatorMoveOnce(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"pipeline", "running", "retry", "blocked", "completed", "status drift"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 14, 18, 26, 14, 0, time.UTC)
			issue := connector.Issue{ID: "issue-2470", Identifier: "digitaldrywood/detent#2470", State: "Todo"}
			cfg := laneMutationTestConfig()
			recorder := &workflowMetricsRecorderSpy{}
			orch := &Orchestrator{cfg: cfg, workflowMetrics: recorder}
			state := newState(cfg)
			state.BoardIssues = []connector.Issue{issue}
			orch.refreshCurrentLaneEntries(t.Context(), &state, at.Add(-time.Minute))
			switch source {
			case "pipeline":
				state.Pipeline = []connector.Issue{issue}
			case "running":
				state.Running[issue.ID] = Running{Issue: issue}
			case "retry":
				state.Retry[issue.ID] = Retry{Issue: issue}
			case "blocked":
				state.Blocked[issue.ID] = Blocked{Issue: issue}
			case "completed":
				state.Completed[issue.ID] = Completed{Issue: issue}
			case "status drift":
				state.StatusDrift.UntrackedOpen = []connector.Issue{issue}
			}
			issue.State = "Backlog"
			state.BoardIssues = []connector.Issue{issue}
			for pass := range 3 {
				now := at.Add(time.Duration(pass) * 4 * time.Minute)
				// Refresh classifies fetched tracker issues before publishing lane entries.
				if _, _, err := orch.observeLane(t.Context(), &state, issue, now); err != nil {
					t.Fatal(err)
				}
				orch.refreshCurrentLaneEntries(t.Context(), &state, now)
			}
			var entered []store.WorkflowPhaseEvent
			for _, event := range recorder.events {
				if event.Status == "entered" {
					entered = append(entered, event)
				}
			}
			if len(entered) != 1 || entered[0].PhaseName != "Backlog" || entered[0].Reason != "operator_move" {
				t.Fatalf("entered events = %+v, want one Backlog operator_move", entered)
			}
			if len(state.laneEntries) != 1 || !state.laneEntries[workflowLaneEntryKey(issue)].Equal(at) {
				t.Fatalf("lane entries = %v, want only original Backlog entry", state.laneEntries)
			}
			// A later genuine move must still be recorded, even when returning to Todo.
			issue.State = "Todo"
			state.BoardIssues = []connector.Issue{issue}
			orch.refreshCurrentLaneEntries(t.Context(), &state, at.Add(12*time.Minute))
			if len(recorder.events) != 4 || recorder.events[3].PhaseName != "Todo" || recorder.events[3].Status != "entered" {
				t.Fatalf("events after return = %+v, want two exited/entered pairs", recorder.events)
			}
		})
	}
}

// Fail the first ledger read so a retained copy would otherwise succeed.
type failingRefreshLaneLedger struct {
	recordingLaneLedger
	calls int
}

func (l *failingRefreshLaneLedger) LaneObservation(context.Context, store.IssueIdentity) (store.LaneObservation, error) {
	l.calls++
	if l.calls == 1 {
		return store.LaneObservation{}, errors.New("temporary ledger failure")
	}
	return store.LaneObservation{}, store.ErrNotFound
}

func TestRefreshCurrentLaneEntriesPreservesCacheOnFailure(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Todo", "Backlog"} {
		t.Run(lane, func(t *testing.T) {
			for _, source := range []string{"ledger", "canonical history"} {
				t.Run(source, func(t *testing.T) {
					t.Parallel()
					at := time.Date(2026, 9, 14, 18, 26, 14, 0, time.UTC)
					issue := connector.Issue{ID: "issue-2470", State: "Todo"}
					cfg := laneMutationTestConfig()
					recorder := &workflowMetricsRecorderSpy{}
					orch := &Orchestrator{cfg: cfg, workflowMetrics: recorder}
					state := newState(cfg)
					state.BoardIssues = []connector.Issue{issue}
					orch.refreshCurrentLaneEntries(t.Context(), &state, at)
					key := workflowLaneEntryKey(issue)
					origin := state.laneProvenance[key]
					state.Pipeline = []connector.Issue{issue}
					issue.State = lane
					state.BoardIssues = []connector.Issue{issue}
					ledger := &failingRefreshLaneLedger{}
					reader := &laneEntryConnector{implementProgressConnector: &implementProgressConnector{refreshed: issue, hydrated: issue}, entry: connector.IssueStateTransition{EnteredAt: at.Add(time.Minute)}, found: true, err: errors.New("canonical history unavailable")}
					if source == "ledger" {
						orch.laneLedger = ledger
					} else {
						orch.connector = reader
					}
					orch.laneObservations = nil
					orch.refreshCurrentLaneEntries(t.Context(), &state, at.Add(time.Minute))
					if len(state.laneEntries) != 1 || !state.laneEntries[key].Equal(at) {
						t.Fatalf("entries after failure = %v, want prior entry %v", state.laneEntries, at)
					}
					if len(state.laneProvenance) != 1 || state.laneProvenance[key] != origin {
						t.Fatalf("provenance after failure = %v, want %v", state.laneProvenance, origin)
					}
					if source == "ledger" && ledger.calls != 1 {
						t.Fatalf("ledger calls = %d, want one authoritative observation", ledger.calls)
					}
					reader.err = nil
					orch.refreshCurrentLaneEntries(t.Context(), &state, at.Add(2*time.Minute))
					if len(state.laneEntries) != 1 || state.laneEntries[workflowLaneEntryKey(issue)].IsZero() {
						t.Fatalf("entries after recovery = %v, want current board lane", state.laneEntries)
					}
				})
			}
		})
	}
}

// Catches treating an intermediate ready promotion as a shipped artifact, and
// losing configured terminal artifact evidence when a receipt is refreshed.
func TestRecordLaneTransitionArtifactOutcome(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, target, reason string
		kind                 string
		want                 string
	}{
		{"accepted artifact", "Published", "ready", gate.KindArtifact, "artifact"},
		{"intermediate promotion", "Merging", "ready", gate.KindArtifact, ""},
		{"abandoned", "Cancelled", "ready", gate.KindArtifact, ""},
		{"code gate", "Published", "ready", gate.KindCommand, ""},
		{"observed import", "Published", "operator_move", gate.KindArtifact, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &autoPromoteWorkflowMetricsRecorder{}
			orch := &Orchestrator{cfg: normalizeConfig(Config{TerminalStates: []string{"Published", "Cancelled"}, AutoPromote: AutoPromoteConfig{Gate: gate.Config{Kind: tt.kind}}}), workflowMetrics: recorder}
			orch.recordLaneTransition(t.Context(), connector.Issue{ID: "artifact", State: "In Progress"}, tt.target, time.Now(), tt.reason, workflowLaneMetadata{})
			events := recorder.snapshot()
			if len(events) != 2 {
				t.Fatalf("events=%#v, want exit and entry", events)
			}
			metadata, ok := workflowLaneMetadataFromJSON(events[1].MetadataJSON)
			if !ok || metadata.TerminalOutcome != tt.want {
				t.Fatalf("outcome=%q, want %q", metadata.TerminalOutcome, tt.want)
			}
		})
	}
}

func TestLaneWriterClosedList(t *testing.T) {
	for _, tt := range []struct {
		name, target, action, reason string
		wantError                    bool
	}{
		{name: "machine", target: "Blocked", wantError: true},
		{name: "normalized machine", target: "Human Review", wantError: true},
		{name: "typed human", target: "Blocked", action: "Approve the requested scope"},
		{name: "normalized human", target: "Human Review", action: "Approve the requested scope"},
		{name: "operator stop", target: "Blocked", reason: "operator_stopped"},
		{name: "normalized operator stop", target: "Human Review", reason: "operator_stopped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := terminalRetryTestIssue("closed-list")
			if tt.action != "" {
				issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: tt.action}
			}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{issue.ID: issue}}
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{HumanReview: new(false), SourceState: "Human Review", ReworkState: "Blocked"}})
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			o := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: metrics}
			state := newState(cfg)
			err := o.updateIssueState(t.Context(), &state, issue, tt.target, time.Now(), tt.reason)
			if (err != nil) != tt.wantError {
				t.Fatalf("write error = %v", err)
			}
			if tt.wantError {
				if len(tracker.transitions) != 0 || len(metrics.snapshot()) != 0 {
					t.Fatal("rejected target caused side effects")
				}
			} else if !slices.Equal(tracker.transitions, []string{"Blocked"}) {
				t.Fatalf("transitions = %v", tracker.transitions)
			}
		})
	}
}
