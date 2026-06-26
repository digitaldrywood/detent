package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

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

	waiting := dependencyAutoUnblockIssue("issue-body-blocked", "Blocked")
	waiting.Description = "Depends on: #415"
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
		Number:         prNumber,
		URL:            "https://github.com/digitaldrywood/detent/pull/426",
		State:          "OPEN",
		HeadSHA:        "sha-current",
		MergeableState: "dirty",
	}
	waiting.BlockerReason = "PR #426 conflicts with main and needs a rebase."
	tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{waiting}}
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{
		Enabled:      true,
		SourceStates: []string{"Blocked"},
		TargetState:  "Todo",
		Readiness:    DependencyReadinessTerminalOrMerged,
	})
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

func dependencyAutoUnblockOrchestrator(
	tracker *dependencyAutoUnblockConnector,
	autoUnblock DependencyAutoUnblockConfig,
) *Orchestrator {
	cfg := normalizeConfig(Config{
		PollInterval:               time.Minute,
		MaxConcurrentAgents:        1,
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done", "Cancelled"},
		DependencyAutoUnblock:      autoUnblock,
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
