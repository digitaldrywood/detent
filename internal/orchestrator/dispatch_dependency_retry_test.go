package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDispatchDependencyRetry(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"native", "body", "workpad"} {
		for _, retry := range []bool{false, true} {
			name := source + "/stranded"
			if retry {
				name = source + "/retry"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
				issue := dispatchTestIssue("issue-2699", "In Progress")
				issue.Identifier = "digitaldrywood/detent#2699"
				ref := "digitaldrywood/detent#2680"
				switch source {
				case "native":
					issue.DependencySource = connector.BlockedRefSourceNative
					issue.BlockedBy = []connector.BlockedRef{{Identifier: ref, State: "Todo", Source: connector.BlockedRefSourceNative}}
				case "body":
					issue.Description = "Depends on: " + ref
				case "workpad":
					issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers:\n  - ref: 'digitaldrywood/detent#2680'\n    predicate: {type: issue_state, ref: 'digitaldrywood/detent#2680', states: [open]}\n    recheck_interval: tick\n```"}}
				}
				state := newState(cfg)
				now := time.Now()
				planner := newDispatchPlanner(cfg)
				if retry {
					planner.scheduleRetryAfter(&state, issue, 2, now, 0, "", "")
				}
				for _, closed := range []bool{false, true} {
					o := &Orchestrator{cfg: cfg, connector: hydratingDispatchConnector{blockers: []connector.Issue{{ID: "blocker", Identifier: ref, State: "Todo", Closed: closed}}}}
					var decisions []dispatchPlanDecision
					cache := make(map[string]dependencyBlocker)
					plan := planner.plan(&state, []connector.Issue{issue}, now, dispatchPlanHooks{
						hydrate: func(issue connector.Issue) (connector.Issue, bool) {
							issue = o.hydrateDispatchDependencies(t.Context(), issue, cache)
							if blocked, ok := state.Blocked[issue.ID]; ok && blockedFromDependency(blocked) && !issueBlockedByNonTerminal(issue, cfg.TerminalStates) {
								delete(state.Blocked, issue.ID)
							}
							return issue, true
						},
						decision: func(d dispatchPlanDecision) { decisions = append(decisions, d) },
					})
					if !closed {
						if len(plan.Dispatches) != 0 || len(decisions) != 1 || decisions[0].SkipReason != dispatchSkipBlockedByDependency {
							t.Fatalf("open dependency: dispatches=%+v decisions=%+v", plan.Dispatches, decisions)
						}
						if retry {
							if got, ok := state.Retry[issue.ID]; !ok || got.Attempt != 2 {
								t.Fatalf("dependency wait lost retry: %+v", state.Retry)
							}
						}
					} else if len(plan.Dispatches) != 1 {
						t.Fatalf("closed dependency: dispatches=%+v decisions=%+v", plan.Dispatches, decisions)
					}
				}
			})
		}
	}
}

func TestDispatchWorkpadDependencySelection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, status, predicate, source string
		wantBlocked                     bool
	}{
		{name: "open in progress", status: "in_progress", predicate: "{type: issue_state, states: [open]}", wantBlocked: true},
		{name: "open blocked", status: "blocked", predicate: "{type: issue_state, states: [open]}", wantBlocked: true},
		{name: "legacy issue predicate", status: "blocked", predicate: "{type: issue_state}", wantBlocked: true},
		{name: "closed predicate", status: "in_progress", predicate: "{type: issue_state, states: [closed]}"},
		{name: "pull request predicate", status: "in_progress", predicate: "{type: pull_request_state, states: [open]}"},
		{name: "completed workpad", status: "complete", predicate: "{type: issue_state, states: [open]}"},
		{name: "native removal", status: "in_progress", predicate: "{type: issue_state, states: [open]}", source: connector.BlockedRefSourceNative},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := dispatchTestIssue("issue", "In Progress")
			issue.Identifier = "digitaldrywood/detent#2699"
			issue.DependencySource = tt.source
			issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: " + tt.status + "\nblockers:\n  - ref: '#2680'\n    predicate: " + tt.predicate + "\n```"}}
			cfg := normalizeConfig(Config{})
			o := &Orchestrator{cfg: cfg, connector: hydratingDispatchConnector{blockers: []connector.Issue{{ID: "blocker", Identifier: "digitaldrywood/detent#2680", State: "Todo"}}}}
			hydrated := o.hydrateDispatchDependencies(t.Context(), issue, make(map[string]dependencyBlocker))
			if got := issueBlockedByNonTerminal(hydrated, cfg.TerminalStates); got != tt.wantBlocked {
				t.Fatalf("blocked=%t, want %t; refs=%+v", got, tt.wantBlocked, hydrated.BlockedBy)
			}
		})
	}
}

func TestDispatchWorkpadDependencyEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		blockers    []connector.Issue
		err         error
		body        bool
		wantBlocked bool
	}{
		{name: "lookup error", err: errors.New("lookup unavailable"), wantBlocked: true},
		{name: "missing issue", wantBlocked: true},
		{name: "body and workpad open terminal lane", body: true, blockers: []connector.Issue{{Identifier: "digitaldrywood/detent#2680", State: "Done"}}, wantBlocked: true},
		{name: "open terminal lane", blockers: []connector.Issue{{Identifier: "digitaldrywood/detent#2680", State: "Done"}}, wantBlocked: true},
		{name: "closed terminal lane", blockers: []connector.Issue{{Identifier: "digitaldrywood/detent#2680", State: "Done", Closed: true}}},
		{name: "open empty lane", blockers: []connector.Issue{{Identifier: "digitaldrywood/detent#2680"}}, wantBlocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}})
			issue := dispatchTestIssue("issue", "In Progress")
			issue.Identifier = "digitaldrywood/detent#2699"
			issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers:\n  - ref: '#2680'\n    predicate: {type: issue_state, states: [open]}\n```"}}
			if tt.body {
				issue.Description = "Depends on: digitaldrywood/detent#2680"
			}
			o := &Orchestrator{cfg: cfg, connector: dispatchEvidenceConnector{hydratingDispatchConnector: hydratingDispatchConnector{blockers: tt.blockers}, err: tt.err}}
			hydrated := o.hydrateDispatchDependencies(t.Context(), issue, make(map[string]dependencyBlocker))
			if got := issueBlockedByNonTerminal(hydrated, cfg.TerminalStates); got != tt.wantBlocked {
				t.Fatalf("blocked=%t, want %t; refs=%+v", got, tt.wantBlocked, hydrated.BlockedBy)
			}

			for _, retry := range []bool{false, true} {
				state := newState(cfg)
				planner := newDispatchPlanner(cfg)
				now := time.Now()
				if retry {
					planner.scheduleRetryAfter(&state, issue, 2, now, 0, "", "")
				}
				plan := planner.plan(&state, []connector.Issue{issue}, now, dispatchPlanHooks{
					hydrate: func(connector.Issue) (connector.Issue, bool) { return hydrated, true },
				})
				if got := len(plan.Dispatches) == 0; got != tt.wantBlocked {
					t.Fatalf("retry=%t: dispatches=%+v, want blocked=%t", retry, plan.Dispatches, tt.wantBlocked)
				}
				if retry && tt.wantBlocked {
					if entry, ok := state.Retry[issue.ID]; !ok || entry.Attempt != 2 {
						t.Fatalf("lost retry: %+v", state.Retry)
					}
				}
			}
		})
	}
}

type dispatchEvidenceConnector struct {
	hydratingDispatchConnector
	err error
}

func (c dispatchEvidenceConnector) FetchIssueStatesByIdentifiers(ctx context.Context, refs []string) ([]connector.Issue, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.hydratingDispatchConnector.FetchIssueStatesByIdentifiers(ctx, refs)
}
