package orchestrator

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestTickAuthorizationBeforeRecovery(t *testing.T) {
	now := time.Date(2026, 9, 19, 16, 26, 0, 0, time.UTC)
	entered := now.Add(-30 * time.Minute)
	for _, tc := range []struct {
		name, lane, target string
		early              bool
	}{
		{"stranded active", "In Progress", "Todo", false},
		{"stale todo PR", "Todo", "Human Review", false},
		{"blocked recovery", "Blocked", "Rework", false},
		{"auto promote", "Human Review", "Merging", false},
		{"dependency late", "Blocked", "Todo", false},
		{"dependency early", "Blocked", "Todo", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done", "Cancelled"}, MaxConcurrentAgents: 1,
				Authorization:         selector.Selector{Labels: selector.Labels{Include: []string{"detent:a"}}},
				BlockedRecovery:       BlockedRecoveryConfig{Enabled: true, SourceStates: []string{"Blocked"}, TargetState: "Rework", ReasonCodes: []string{blockedRecoveryReasonMergeConflict}},
				DependencyAutoUnblock: DependencyAutoUnblockConfig{Enabled: strings.HasPrefix(tc.name, "dependency")},
			})
			if tc.name == "auto promote" {
				cfg.AutoPromote.Enabled = true
			}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			issues := []connector.Issue{}
			for i, label := range []string{"detent:a", "detent:b"} {
				id := []string{"owned", "unowned"}[i]
				issue := dependencyAutoUnblockIssue(id, tc.lane)
				issue.Labels = []string{label}
				issue.StageUpdatedAt = &entered
				if tc.name == "auto promote" {
					issue.PullRequest = &connector.PullRequest{Number: i + 1, State: "OPEN", MergeableState: "clean", CIStatus: "success", CodexReviewState: "COMMENTED", CodexReviewSubmittedAt: &entered}
				}
				if tc.name == "stale todo PR" {
					issue.PullRequest = &connector.PullRequest{Number: i + 1, State: "OPEN", URL: "https://github.test/digitaldrywood/detent/pull/1"}
				}
				if tc.name == "blocked recovery" {
					issue = blockedRecoverySignatureIssue(id, "head", "diff", "base", "dirty")
					issue.Labels = []string{label}
					recordBlockedRecoveryReasonEvent(t, metrics, issue, entered, blockedRecoveryReasonMergeConflict)
				}
				if strings.HasPrefix(tc.name, "dependency") {
					issue.BlockedBy = []connector.BlockedRef{{ID: "done", Identifier: "digitaldrywood/detent#123", State: "Done", Source: connector.BlockedRefSourceNative}}
				}
				issues = append(issues, issue)
			}
			tracker := &autoPromoteTickConnector{stateIssues: issues, resolvedIssues: []connector.Issue{{ID: "done", Identifier: "digitaldrywood/detent#123", State: "Done", Closed: true}}}
			tracker.resolvedIssues = append(tracker.resolvedIssues, issues...)
			var logs bytes.Buffer
			orch := &Orchestrator{cfg: cfg, connector: tracker, workflowMetrics: metrics, logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), recoveryInspector: strandedActiveRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "missing"}}}
			state := newState(cfg)
			state.StrandedActiveThreshold = 10 * time.Minute
			state.BoardIssues = cloneIssues(issues)
			state.Pipeline = cloneIssues(issues)
			state.dependencyUnblockEarly = tc.early
			orch.tick(t.Context(), &state, now)
			if len(tracker.updates) != 1 || tracker.updates[0].issueID != "owned" || tracker.updates[0].state != tc.target {
				t.Errorf("updates = %+v; want only owned -> %s", tracker.updates, tc.target)
			}
			for _, c := range tracker.comments {
				if c.issueID != "owned" {
					t.Errorf("unauthorized comment: %+v", c)
				}
			}
			for _, f := range tracker.setFields {
				if f.issueID != "owned" {
					t.Errorf("unauthorized field write: %+v", f)
				}
			}
			declined := 0
			for _, d := range state.SchedulerDecisions {
				if d.Reason == dispatchSkipAuthorizationSelector && d.IssueID == "unowned" {
					declined++
				}
			}
			wantDeclined := 0
			if tc.lane == "Todo" || tc.lane == "In Progress" {
				wantDeclined = 1
			}
			if declined != wantDeclined {
				t.Errorf("authorization declines = %d, want %d", declined, wantDeclined)
			}
			if !strings.Contains(logs.String(), "authorization_selector_declined") || !strings.Contains(logs.String(), "skipped_count=1") {
				t.Error("missing aggregate skip log")
			}
		})
	}
}

func TestTickAuthorizationSelectorSemantics(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		auth selector.Selector
		want int
	}{
		{"unconfigured", selector.Selector{}, 2},
		{"include", selector.Selector{Labels: selector.Labels{Include: []string{"detent:a"}}}, 1},
		{"exclude", selector.Selector{Labels: selector.Labels{Exclude: []string{"detent:b"}}}, 1},
		{"field", selector.Selector{Fields: []selector.FieldEquals{{Name: "Team", Value: "a"}}}, 1},
		{"identity", selector.Selector{AuthorIn: []string{"@me"}}, 1},
		{"nested", selector.Selector{And: []selector.Selector{{Labels: selector.Labels{Include: []string{"detent:a"}}}}, Or: []selector.Selector{{Fields: []selector.FieldEquals{{Name: "Team", Value: "a"}}}, {Labels: selector.Labels{Include: []string{"other"}}}}}, 1},
	} {
		for _, blocked := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/retry", true: "/blocked_retry"}[blocked], func(t *testing.T) {
				cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}, Authorization: tc.auth, SelectorContext: selector.Context{InstanceLogin: "alice"}})
				owned := connector.Issue{ID: "owned", State: "Todo", Labels: []string{"detent:a"}, AuthorID: "alice", Fields: map[string]string{"Team": "a"}}
				unowned := connector.Issue{ID: "unowned", State: "Todo", Labels: []string{"detent:b"}, AuthorID: "bob", Fields: map[string]string{"Team": "b"}}
				issues := []connector.Issue{owned, unowned}
				state := newState(cfg)
				state.Retry[unowned.ID] = Retry{Issue: unowned, Attempt: 2, DueAt: now.Add(-time.Minute)}
				state.Claimed[unowned.ID] = Claimed{Issue: unowned}
				state.BudgetRefusals[unowned.ID] = BudgetRefusal{}
				if blocked {
					state.Blocked[unowned.ID] = Blocked{Issue: unowned}
				}
				previous := tickPreviousState{pipeline: issues, epicTransitionWatch: issues, blockedStatusIssues: issues}
				var logs bytes.Buffer
				orch := &Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
				got := orch.filterAuthorizedTickIssues(t.Context(), &state, tickFetchedIssues{candidates: issues, status: issues, statusOK: true}, &previous, now)
				if len(got.candidates) != tc.want || len(got.status) != tc.want || len(previous.pipeline) != tc.want || len(previous.epicTransitionWatch) != tc.want || len(previous.blockedStatusIssues) != tc.want || !got.statusOK {
					t.Fatalf("filter did not retain %d authorized issues: %+v", tc.want, got)
				}
				if len(state.SchedulerDecisions) != 2-tc.want {
					t.Fatalf("decisions = %+v", state.SchedulerDecisions)
				}
				if tc.want == 1 {
					if len(state.Retry) != 0 || len(state.Claimed) != 0 || len(state.BudgetRefusals) != 0 {
						t.Fatal("declined retry retains ownership")
					}
					if _, ok := state.Blocked[unowned.ID]; ok != blocked {
						t.Fatal("blocked state changed")
					}
					if strings.Contains(logs.String(), "unowned") || strings.Count(logs.String(), "authorization_selector_declined") != 1 {
						t.Fatalf("expected count-only log: %s", logs.String())
					}
				}
				if len(issues) != 2 || issues[1].ID != "unowned" {
					t.Fatal("input snapshots mutated")
				}
			})
		}
	}
}

func TestTickAuthorizationDeclineMixedLanes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active []string
		want   map[string]bool
	}{
		{"standard lanes", []string{"Todo", "In Progress", "Rework", "Merging"}, map[string]bool{"Todo": true, "In Progress": true, "Rework": true, "Merging": true}},
		{"configured lane", []string{"Ready"}, map[string]bool{"Ready": true}},
		{"terminal overlap", []string{"Todo", "Done", "Cancelled"}, map[string]bool{"Todo": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: tc.active, TerminalStates: []string{"Done", "Cancelled"}, Authorization: selector.Selector{Labels: selector.Labels{Include: []string{"detent:a"}}}})
			var issues []connector.Issue
			for _, lane := range []string{"Todo", "In Progress", "Rework", "Merging", "Ready", "Done", "Cancelled", "Backlog", "Blocked", "Human Review", "Plan Review"} {
				issue := dependencyAutoUnblockIssue(lane, lane)
				issues = append(issues, issue)
			}
			closed := dependencyAutoUnblockIssue("closed", "Todo")
			closed.Closed = true
			issues = append(issues, closed)
			owned := dependencyAutoUnblockIssue("owned", "Todo")
			owned.Labels = []string{"detent:a"}
			issues = append(issues, owned)
			state := newState(cfg)
			state.Pipeline = cloneIssues(issues)
			state.LaneSignalCandidates = cloneIssues(issues)
			state.Retry["closed"] = Retry{Issue: closed, Attempt: 2}
			state.Claimed["closed"] = Claimed{Issue: closed}
			previous := tickPreviousState{pipeline: cloneIssues(issues)}
			var logs bytes.Buffer
			orch := &Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			got := orch.filterAuthorizedTickIssues(t.Context(), &state, tickFetchedIssues{candidates: issues, status: issues}, &previous, time.Now())
			for _, filtered := range [][]connector.Issue{got.candidates, got.status, state.Pipeline, state.LaneSignalCandidates, previous.pipeline} {
				if len(filtered) != 1 || filtered[0].ID != "owned" {
					t.Errorf("filtered = %+v, want only owned", filtered)
				}
			}
			if len(state.SchedulerDecisions) != len(tc.want) {
				t.Errorf("declines = %d, want %d", len(state.SchedulerDecisions), len(tc.want))
			}
			seen := map[string]bool{}
			for _, d := range state.SchedulerDecisions {
				if d.Reason != dispatchSkipAuthorizationSelector || !tc.want[d.IssueID] || seen[d.IssueID] {
					t.Errorf("unexpected decision: %+v", d)
				}
				seen[d.IssueID] = true
			}
			if len(state.Retry) != 0 || len(state.Claimed) != 0 {
				t.Error("closed excluded retry retains ownership")
			}
			if strings.Count(logs.String(), "authorization_selector_declined") != 1 || !strings.Contains(logs.String(), fmt.Sprintf("skipped_count=%d", len(issues)-1)) {
				t.Errorf("aggregate log = %s", logs.String())
			}
		})
	}
}
