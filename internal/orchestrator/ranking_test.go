package orchestrator

import (
	"context"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestSortIssuesForDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name                  string
		dispatchStatePriority []string
		dispatchLabelPriority []string
		prioritizeUnblockers  bool
		issues                []connector.Issue
		want                  []string
	}{
		{
			name:                  "sorts by state dispatch rank before priority and age",
			dispatchStatePriority: []string{"Merging", "Rework"},
			issues: []connector.Issue{
				rankingIssue("todo-old-urgent", "Todo", 1, now.Add(-4*time.Hour)),
				rankingIssue("rework-new-low", "Rework", 4, now.Add(-time.Hour)),
				rankingIssue("merging-new-low", "Merging", 4, now.Add(-30*time.Minute)),
			},
			want: []string{"merging-new-low", "rework-new-low", "todo-old-urgent"},
		},
		{
			name:                  "sorts by priority within the same state rank",
			dispatchStatePriority: []string{"Todo"},
			issues: []connector.Issue{
				rankingIssue("todo-medium", "Todo", 3, now.Add(-3*time.Hour)),
				rankingIssue("todo-none", "Todo", 0, now.Add(-4*time.Hour)),
				rankingIssue("todo-urgent", "Todo", 1, now.Add(-time.Hour)),
				rankingIssue("todo-high", "Todo", 2, now.Add(-2*time.Hour)),
			},
			want: []string{"todo-urgent", "todo-high", "todo-medium", "todo-none"},
		},
		{
			name:                  "sorts by configured label rank within the same priority",
			dispatchStatePriority: []string{"Todo"},
			dispatchLabelPriority: []string{"bug", "regression", "enhancement"},
			issues: []connector.Issue{
				rankingIssueWithLabels("todo-unlisted-oldest", "Todo", 2, now.Add(-4*time.Hour), "question"),
				rankingIssueWithLabels("todo-enhancement-old", "Todo", 2, now.Add(-3*time.Hour), "enhancement"),
				rankingIssueWithLabels("todo-bug-new", "Todo", 2, now.Add(-time.Hour), "bug"),
			},
			want: []string{"todo-bug-new", "todo-enhancement-old", "todo-unlisted-oldest"},
		},
		{
			name:                  "keeps priority above configured label rank",
			dispatchStatePriority: []string{"Todo"},
			dispatchLabelPriority: []string{"bug", "enhancement"},
			issues: []connector.Issue{
				rankingIssueWithLabels("todo-bug-p2", "Todo", 2, now.Add(-3*time.Hour), "bug"),
				rankingIssueWithLabels("todo-enhancement-p1", "Todo", 1, now.Add(-time.Hour), "enhancement"),
			},
			want: []string{"todo-enhancement-p1", "todo-bug-p2"},
		},
		{
			name:                  "uses best configured rank for multi label issues",
			dispatchStatePriority: []string{"Todo"},
			dispatchLabelPriority: []string{"bug", "regression", "enhancement"},
			issues: []connector.Issue{
				rankingIssueWithLabels("todo-regression", "Todo", 2, now.Add(-3*time.Hour), "regression"),
				rankingIssueWithLabels("todo-multi", "Todo", 2, now.Add(-time.Hour), "enhancement", "bug"),
			},
			want: []string{"todo-multi", "todo-regression"},
		},
		{
			name:                  "sorts older issues first when priority and label rank match",
			dispatchStatePriority: []string{"Todo"},
			dispatchLabelPriority: []string{"bug"},
			issues: []connector.Issue{
				rankingIssueWithLabels("todo-new", "Todo", 2, now.Add(-time.Hour), "bug"),
				rankingIssueWithLabels("todo-old", "Todo", 2, now.Add(-3*time.Hour), "bug"),
				rankingIssueWithLabels("todo-middle", "Todo", 2, now.Add(-2*time.Hour), "bug"),
			},
			want: []string{"todo-old", "todo-middle", "todo-new"},
		},
		{
			name:                  "empty label priority preserves age tiebreak",
			dispatchStatePriority: []string{"Todo"},
			issues: []connector.Issue{
				rankingIssueWithLabels("todo-new-bug", "Todo", 2, now.Add(-time.Hour), "bug"),
				rankingIssueWithLabels("todo-old-enhancement", "Todo", 2, now.Add(-3*time.Hour), "enhancement"),
			},
			want: []string{"todo-old-enhancement", "todo-new-bug"},
		},
		{
			name:                  "normalizes state ranks and sorts unranked states last",
			dispatchStatePriority: []string{" Merging ", "Rework"},
			issues: []connector.Issue{
				rankingIssue("todo-old-urgent", "Todo", 1, now.Add(-4*time.Hour)),
				rankingIssue("rework-high", " rework ", 2, now.Add(-time.Hour)),
				rankingIssue("merging-low", "merging", 4, now.Add(-30*time.Minute)),
				rankingIssue("in-progress-high", "In Progress", 2, now.Add(-3*time.Hour)),
			},
			want: []string{"merging-low", "rework-high", "todo-old-urgent", "in-progress-high"},
		},
		{
			name: "uses deterministic identifier order after state rank priority and age",
			issues: []connector.Issue{
				rankingIssue("issue-c", "Todo", 2, now),
				rankingIssue("issue-a", "Todo", 2, now),
				rankingIssue("issue-b", "Todo", 2, now),
			},
			want: []string{"issue-a", "issue-b", "issue-c"},
		},
		{
			name:                  "state rank outranks unblocker boost",
			dispatchStatePriority: []string{"Merging", "Todo"},
			prioritizeUnblockers:  true,
			issues: []connector.Issue{
				rankingIssueWithUnblockers("todo-unblocker", "Todo", 0, now.Add(-time.Hour), 5),
				rankingIssue("merging-peer", "Merging", 0, now),
			},
			want: []string{"merging-peer", "todo-unblocker"},
		},
		{
			name:                 "tracker priority outranks unblocker boost",
			prioritizeUnblockers: true,
			issues: []connector.Issue{
				rankingIssueWithUnblockers("p2-unblocker", "Todo", 2, now.Add(-time.Hour), 5),
				rankingIssue("p1-peer", "Todo", 1, now),
			},
			want: []string{"p1-peer", "p2-unblocker"},
		},
		{
			name:                  "every configured label tier outranks unblocker boost",
			dispatchLabelPriority: []string{"hotfix", "priority", "bug"},
			prioritizeUnblockers:  true,
			issues: []connector.Issue{
				rankingIssueWithUnblockers("unlabeled-unblocker", "Todo", 0, now.Add(-4*time.Hour), 5),
				rankingIssueWithLabels("bug", "Todo", 0, now.Add(-3*time.Hour), "bug"),
				rankingIssueWithLabels("priority", "Todo", 0, now.Add(-2*time.Hour), "priority"),
				rankingIssueWithLabels("hotfix", "Todo", 0, now.Add(-time.Hour), "hotfix"),
			},
			want: []string{"hotfix", "priority", "bug", "unlabeled-unblocker"},
		},
		{
			name:                 "more direct dependents rank first among unlabeled peers",
			prioritizeUnblockers: true,
			issues: []connector.Issue{
				rankingIssueWithUnblockers("one", "Todo", 0, now.Add(-3*time.Hour), 1),
				rankingIssueWithUnblockers("three", "Todo", 0, now.Add(-time.Hour), 3),
				rankingIssue("plain", "Todo", 0, now.Add(-4*time.Hour)),
			},
			want: []string{"three", "one", "plain"},
		},
		{
			name: "disabled preserves age ordering exactly",
			issues: []connector.Issue{
				rankingIssueWithUnblockers("new-unblocker", "Todo", 0, now.Add(-time.Hour), 5),
				rankingIssue("old-peer", "Todo", 0, now.Add(-4*time.Hour)),
			},
			want: []string{"old-peer", "new-unblocker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issues := cloneRankingIssues(tt.issues)
			sortIssuesForDispatch(issues, tt.dispatchStatePriority, tt.dispatchLabelPriority, tt.prioritizeUnblockers)

			if got := rankingIssueIDs(issues); !equalStrings(got, tt.want) {
				t.Fatalf("sortIssuesForDispatch() ids = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAnnotateUnblockerCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		enabled bool
		issues  []connector.Issue
		want    map[string]int
	}{
		{
			name:    "counts blocked and dependency waiting dependents once",
			enabled: true,
			issues: []connector.Issue{
				rankingDependencyIssue("blocked", "Blocked", "blocker"),
				rankingDependencyIssue("waiting", "Todo", "blocker"),
				rankingDependencyIssue("duplicate-edge", "Blocked", "blocker", "blocker"),
			},
			want: map[string]int{"blocker": 3},
		},
		{
			name:    "cycle members remain dependency waiting and receive no boost",
			enabled: true,
			issues: []connector.Issue{
				rankingDependencyIssue("first", "Todo", "second"),
				rankingDependencyIssue("second", "Todo", "first"),
			},
			want: map[string]int{"first": 0, "second": 0},
		},
		{
			name:    "terminal dependencies do not create waiting work",
			enabled: true,
			issues: []connector.Issue{
				func() connector.Issue {
					issue := rankingDependencyIssue("done-dependent", "Todo", "blocker")
					issue.BlockedBy[0].State = "Done"
					return issue
				}(),
			},
			want: map[string]int{"blocker": 0},
		},
		{
			name:    "disabled clears existing counts",
			enabled: false,
			issues:  []connector.Issue{rankingDependencyIssue("blocked", "Blocked", "blocker")},
			want:    map[string]int{"blocker": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			targets := make([]connector.Issue, 0, len(tt.want))
			for id := range tt.want {
				target := rankingIssueWithUnblockers(id, "Todo", 0, time.Time{}, 99)
				for _, issue := range tt.issues {
					if issue.ID == id {
						target = cloneIssue(issue)
						target.UnblockerCount = 99
						break
					}
				}
				targets = append(targets, target)
			}
			issues := append(cloneRankingIssues(targets), cloneRankingIssues(tt.issues)...)
			annotateUnblockerCounts(targets, issues, []string{"todo"}, []string{"done"}, tt.enabled)
			for _, target := range targets {
				if target.UnblockerCount != tt.want[target.ID] {
					t.Fatalf("%s UnblockerCount = %d, want %d", target.ID, target.UnblockerCount, tt.want[target.ID])
				}
			}
		})
	}
}

func TestAnnotateUnblockerCountsIsInputOrderIndependent(t *testing.T) {
	t.Parallel()

	dependents := []connector.Issue{
		rankingDependencyIssue("blocked-a", "Blocked", "blocker-one", "blocker-two"),
		rankingDependencyIssue("blocked-b", "Blocked", "blocker-two"),
		rankingDependencyIssue("waiting-c", "Todo", "blocker-two"),
		rankingDependencyIssue("duplicate-edge", "Blocked", "blocker-one", "blocker-one"),
	}
	want := map[string]int{"blocker-one": 2, "blocker-two": 3}

	permutations := [][]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{2, 0, 3, 1},
		{1, 3, 0, 2},
	}
	for _, permutation := range permutations {
		targets := []connector.Issue{
			rankingIssue("blocker-one", "Todo", 0, time.Time{}),
			rankingIssue("blocker-two", "Todo", 0, time.Time{}),
		}
		issues := cloneRankingIssues(targets)
		for _, index := range permutation {
			issues = append(issues, cloneIssue(dependents[index]))
		}
		annotateUnblockerCounts(targets, issues, []string{"todo"}, []string{"done"}, true)
		for _, target := range targets {
			if target.UnblockerCount != want[target.ID] {
				t.Fatalf("permutation %v: %s UnblockerCount = %d, want %d", permutation, target.ID, target.UnblockerCount, want[target.ID])
			}
		}
	}
}

func TestDispatchPlannerRecordsUnblockerSelectionReason(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents:  1,
		ActiveStates:         []string{"Todo", "Blocked"},
		TerminalStates:       []string{"Done"},
		PrioritizeUnblockers: true,
	})
	blocker := rankingIssue("blocker", "Todo", 0, now.Add(-time.Hour))
	peer := rankingIssue("older-peer", "Todo", 0, now.Add(-4*time.Hour))
	blocker.Title = "Dependency blocker"
	peer.Title = "Older peer"
	dependent := rankingDependencyIssue("dependent", "Blocked", blocker.ID)
	state := newState(cfg)
	state.Blocked[dependent.ID] = Blocked{Issue: dependent, Source: BlockedSourceDependency}

	var decisions []dispatchPlanDecision
	newDispatchPlanner(cfg).plan(&state, []connector.Issue{peer, blocker}, now, dispatchPlanHooks{
		decision: func(decision dispatchPlanDecision) {
			decisions = append(decisions, decision)
		},
	})

	if len(decisions) != 2 {
		t.Fatalf("decisions = %d, want 2", len(decisions))
	}
	if !decisions[0].Selected || decisions[0].Issue.ID != blocker.ID || decisions[0].UnblockerCount != 1 {
		t.Fatalf("first decision = %#v, want selected blocker with one dependent", decisions[0])
	}
	orch := Orchestrator{cfg: cfg}
	orch.logDispatchPlanDecision(context.Background(), &state, now, decisions[0])
	if len(state.SchedulerDecisions) != 1 || state.SchedulerDecisions[0].Reason != "unblocks_1_issue" {
		t.Fatalf("scheduler decisions = %#v, want unblocker reason", state.SchedulerDecisions)
	}
}

func TestConfigFromWorkflowIncludesDispatchPriorityByState(t *testing.T) {
	t.Parallel()

	workflow := workflowconfig.Default()
	workflow.Agent.DispatchPriorityByState = []string{"Merging", "Rework"}
	workflow.Agent.DispatchPriorityByLabel = []string{"bug", "enhancement"}
	workflow.Agent.PrioritizeUnblockers = false
	workflow.Workpad.StructuredOnly = true

	cfg := ConfigFromWorkflow(workflow)
	workflow.Agent.DispatchPriorityByState[0] = "Todo"
	workflow.Agent.DispatchPriorityByLabel[0] = "question"

	wantStates := []string{"Merging", "Rework"}
	if !equalStrings(cfg.DispatchPriorityByState, wantStates) {
		t.Fatalf("ConfigFromWorkflow().DispatchPriorityByState = %#v, want %#v", cfg.DispatchPriorityByState, wantStates)
	}
	wantLabels := []string{"bug", "enhancement"}
	if !equalStrings(cfg.DispatchPriorityByLabel, wantLabels) {
		t.Fatalf("ConfigFromWorkflow().DispatchPriorityByLabel = %#v, want %#v", cfg.DispatchPriorityByLabel, wantLabels)
	}
	if cfg.PrioritizeUnblockers {
		t.Fatal("ConfigFromWorkflow().PrioritizeUnblockers = true, want false")
	}
	if !cfg.AutoPromote.WorkpadStructuredOnly {
		t.Fatal("ConfigFromWorkflow().AutoPromote.WorkpadStructuredOnly = false, want true")
	}
}

func TestConfigFromWorkflowIncludesBlockedCauseStatusLabelPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "label status source", source: workflowconfig.GitHubStatusSourceLabel, want: "workflow:"},
		{name: "project status source", source: workflowconfig.GitHubStatusSourceProjectV2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workflow := workflowconfig.Default()
			workflow.Tracker.GitHubStatusSource = tt.source
			workflow.Tracker.StatusLabelPrefix = " Workflow: "
			if got := ConfigFromWorkflow(workflow).StatusLabelPrefix; got != tt.want {
				t.Fatalf("StatusLabelPrefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func rankingIssue(id string, state string, priority int, createdAt time.Time) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = id
	issue.State = state
	issue.CreatedAt = &createdAt
	if priority > 0 {
		issue.Priority = &priority
	}
	return issue
}

func rankingIssueWithLabels(id string, state string, priority int, createdAt time.Time, labels ...string) connector.Issue {
	issue := rankingIssue(id, state, priority, createdAt)
	issue.Labels = append([]string(nil), labels...)
	return issue
}

func rankingIssueWithUnblockers(id string, state string, priority int, createdAt time.Time, count int) connector.Issue {
	issue := rankingIssue(id, state, priority, createdAt)
	issue.UnblockerCount = count
	return issue
}

func rankingDependencyIssue(id string, state string, blockerIDs ...string) connector.Issue {
	issue := rankingIssue(id, state, 0, time.Time{})
	for _, blockerID := range blockerIDs {
		issue.BlockedBy = append(issue.BlockedBy, connector.BlockedRef{ID: blockerID, Identifier: blockerID})
	}
	return issue
}

func cloneRankingIssues(issues []connector.Issue) []connector.Issue {
	cloned := make([]connector.Issue, len(issues))
	for i, issue := range issues {
		cloned[i] = cloneIssue(issue)
	}
	return cloned
}

func rankingIssueIDs(issues []connector.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
