package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDependencyAuthoritySurvivesRefresh(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, source string
		refs         []connector.BlockedRef
	}{
		{name: "native removal", source: connector.BlockedRefSourceNative},
		{name: "native relation", source: connector.BlockedRefSourceNative, refs: []connector.BlockedRef{{Identifier: "owner/repo#100", Source: connector.BlockedRefSourceNative}}},
		{name: "body fallback", source: connector.BlockedRefSourceProse, refs: []connector.BlockedRef{{Identifier: "owner/repo#100", Source: connector.BlockedRefSourceProse}}},
		{name: "body removal", source: connector.BlockedRefSourceProse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := connector.Issue{ID: "issue-101", Identifier: "owner/repo#101", State: "Blocked", Description: "Depends on: #100", Comments: []connector.IssueComment{{Body: "Blocked by: #99"}}, BlockedBy: []connector.BlockedRef{{Identifier: "owner/repo#100"}}}
			fresh := connector.Issue{ID: old.ID, Identifier: old.Identifier, State: old.State, DependencySource: tt.source, BlockedBy: tt.refs, DependencyNotes: []string{"owner/repo#99: prose dependency ignored: native relation absent"}}
			tracker := &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{fresh}}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
			got, found, err := orch.hydrateDependencyAutoUnblockIssue(t.Context(), old, []string{"blocked"})
			if err != nil || !found {
				t.Fatalf("hydrate = %t, %v", found, err)
			}
			if len(got.BlockedBy) != len(tt.refs) || got.DependencySource != tt.source {
				t.Fatalf("dependency selection = %+v (%s)", got.BlockedBy, got.DependencySource)
			}
			if len(got.DependencyNotes) != 1 || got.DependencyNotes[0] != fresh.DependencyNotes[0] {
				t.Fatalf("notes = %v", got.DependencyNotes)
			}
			// Both dispatch hydration and dependency recovery use this tracker merge.
			merged := issueWithTextDependencyRefs(mergeIssueTrackerFields(old, fresh))
			if len(merged.BlockedBy) != len(tt.refs) {
				t.Fatalf("merge restored stale blockers: %+v", merged.BlockedBy)
			}
		})
	}
}

func TestDependencyTextIgnoresHistoricalComments(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"Blocked by: #100", "## Codex Workpad\nBlocked by: #100",
		"## Codex Workpad\n### Blockers\nDepends on: #100 and #200",
		"## Codex Workpad\n### Blockers\nDepends on: [#100](https://github.com/owner/repo/issues/100) or #200",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			issue := issueWithTextDependencyRefs(connector.Issue{Identifier: "owner/repo#101", Comments: []connector.IssueComment{{Body: body}}})
			if len(issue.BlockedBy) != 0 {
				t.Fatalf("historical blockers = %+v", issue.BlockedBy)
			}
		})
	}
}

func TestNativeAuthorityIgnoresWorkpadPredicates(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		t.Run(nativeRelationTestName(native), func(t *testing.T) {
			issue := connector.Issue{Identifier: "owner/repo#101", DependencySource: connector.BlockedRefSourceNative, State: "Blocked", Comments: []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '#100'\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\n    recheck_interval: tick\nhuman_action: null\n```"}}}
			if native {
				issue.BlockedBy = []connector.BlockedRef{{Identifier: "owner/repo#100", Source: connector.BlockedRefSourceNative, State: "Todo"}}
			}
			signal, ok := autoPromoteIssueWorkpadSignal(issue)
			if !ok {
				t.Fatal("missing signal")
			}
			want := 0
			if native {
				want = 1
			}
			if len(signal.Blockers) != want {
				t.Fatalf("blockers = %+v, want %d", signal.Blockers, want)
			}
			issue.WorkpadSignal = signal
			if !native && reworkBreakerIssueHeld(issue, []string{"Done"}) {
				t.Fatal("empty historical status held native-empty issue")
			}
			o := &Orchestrator{}
			result := o.evaluateRecordedBlockers(t.Context(), nil, issue, nil, time.Now())
			if !native && result.Holds {
				t.Fatalf("removed native dependency still holds: %+v", result)
			}
			if signal.Status != workpad.StatusBlocked {
				t.Fatal("rewrote worker status")
			}
		})
	}
}

func nativeRelationTestName(v bool) string {
	if v {
		return "native relation present"
	}
	return "native relation removed"
}

func TestNativeAuthorityIgnoresPersistedDeferral(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
	}{{name: "persisted dependency deferral"}, {name: "history unavailable", err: errors.New("history unavailable")}} {
		t.Run(tt.name, func(t *testing.T) {
			issue := dispatchTestIssue("issue", "Rework")
			issue.DependencySource = connector.BlockedRefSourceNative
			attempts := &recordingWorkAttemptStore{history: []store.WorkAttempt{implementProgressDependencyDeferralHistoryAttempt(1, "owner/repo#100", "Todo")}, historyErr: tt.err}
			o := &Orchestrator{cfg: normalizeConfig(Config{}), workAttempts: attempts, connector: hydratingDispatchConnector{blockers: []connector.Issue{{Identifier: "owner/repo#100", State: "Todo"}}}}
			got := o.filterImplementDependencyDeferrals(t.Context(), []connector.Issue{issue})
			if len(got) != 1 || len(attempts.decisions) != 0 {
				t.Fatalf("stale attempt held native-empty candidate: %+v %+v", got, attempts.decisions)
			}
		})
	}
}

func TestNativeAuthorityRecoversRemovedWorkpadDependency(t *testing.T) {
	t.Parallel()
	for _, present := range []bool{false, true} {
		t.Run(nativeRelationTestName(present), func(t *testing.T) {
			waiting := dependencyAutoUnblockIssue("issue-101", "Blocked")
			waiting.DependencySource = connector.BlockedRefSourceNative
			waiting.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '#100'\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\n    recheck_interval: tick\nhuman_action: null\n```"}}
			waiting.WorkpadSignal, _ = rawIssueWorkpadSignal(waiting)
			if present {
				waiting.BlockedBy = []connector.BlockedRef{{Identifier: "digitaldrywood/detent#100", Source: connector.BlockedRefSourceNative, State: "Todo"}}
			}
			tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{waiting}, blockers: []connector.Issue{{ID: "blocker", Identifier: "digitaldrywood/detent#100", State: "Todo"}}}
			o := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{Enabled: true, SourceStates: []string{"Blocked"}, TargetState: "Todo"})
			state := newState(o.cfg)
			state.Blocked[waiting.ID] = Blocked{Issue: waiting, Source: BlockedSourceProjectStatus}
			got := o.recoverCauseBlockedIssue(t.Context(), &state, waiting, time.Now())
			if got == present {
				t.Fatalf("recovered=%t, updates=%+v", got, tracker.updates)
			}
			if !present && (len(tracker.updates) != 1 || tracker.updates[0].state != "Todo") {
				t.Fatalf("updates=%+v", tracker.updates)
			}
		})
	}
}

func TestNativeAuthorityIgnoresLegacyDependencyWorkpad(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"## Codex Workpad\n### Blockers\nBlocked by: #100", "## Codex Workpad\nBlocked by: #100"} {
		t.Run(body, func(t *testing.T) {
			issue := connector.Issue{Identifier: "owner/repo#101", DependencySource: connector.BlockedRefSourceNative, Comments: []connector.IssueComment{{Body: body}}}
			signal, _ := autoPromoteIssueWorkpadSignal(issue)
			if signal != nil && (len(signal.Blockers) > 0 || signal.HumanAction != "") {
				t.Fatalf("historical dependency holds: %+v", signal)
			}
		})
	}
}

func TestNativeAuthorityPreservesLegacyHumanAction(t *testing.T) {
	t.Parallel()
	issue := connector.Issue{Identifier: "owner/repo#101", DependencySource: connector.BlockedRefSourceNative, Comments: []connector.IssueComment{{Body: "## Codex Workpad\n### Human Action Needed\nApprove access for #100"}}}
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.HumanAction != "Approve access for #100" {
		t.Fatalf("human action lost: %+v", signal)
	}
}

func TestNativeAuthorityPreservesLegacyApprovalBlockers(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"## Codex Workpad\n### Blockers\n- Owner approval is still required before merge.",
		"## Codex Workpad\n### Blockers\nBlocked by: #100\n- Owner approval is still required before merge.",
		"## Codex Workpad\nBlocked by: #100\nOwner approval is still required before merge.",
	} {
		t.Run(body, func(t *testing.T) {
			issue := connector.Issue{Identifier: "owner/repo#101", DependencySource: connector.BlockedRefSourceNative, Comments: []connector.IssueComment{{Body: body}}}
			signal, ok := autoPromoteIssueWorkpadSignal(issue)
			if !ok || signal == nil || signal.HumanAction != "Owner approval is still required before merge." {
				t.Fatalf("approval lost: %+v", signal)
			}
		})
	}
}

func TestNativeAuthorityPreservesMixedDependencyApproval(t *testing.T) {
	t.Parallel()
	issue := connector.Issue{Identifier: "owner/repo#101", DependencySource: connector.BlockedRefSourceNative, Comments: []connector.IssueComment{{Body: "## Codex Workpad\n### Blockers\nDepends on: #100 and owner approval"}}}
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.HumanAction == "" {
		t.Fatalf("approval lost: %+v", signal)
	}
}
