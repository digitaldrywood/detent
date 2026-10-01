package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestCompletionForgeEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	older, newer := now.Add(-time.Minute), now.Add(time.Minute)
	for _, tt := range []struct {
		name           string
		pr             *connector.PullRequest
		recorded       *time.Time
		wantUnfinished bool
	}{
		{"merged", &connector.PullRequest{State: "MERGED", HeadSHA: "head"}, &now, false},
		{"newer open head", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &newer}, &now, false},
		{"no pull request", nil, &now, true},
		{"current in progress", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &older}, &now, true},
		{"equal timestamp", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &now}, &now, true},
		{"unknown status time", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &newer}, nil, true},
		{"draft", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &newer, Draft: true}, &now, true},
		{"closed unmerged", &connector.PullRequest{State: "CLOSED", HeadSHA: "head", HeadCommittedAt: &newer}, &now, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := connector.Issue{PullRequest: tt.pr, WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusInProgress, RecordedAt: tt.recorded}}
			if got := completionWorkpadUnfinished(issue); got != tt.wantUnfinished {
				t.Fatalf("unfinished = %v, want %v", got, tt.wantUnfinished)
			}
			if issue.WorkpadSignal.Status != workpad.StatusInProgress {
				t.Fatal("mutated source assertion")
			}
		})
	}
}

func TestCompletionForgeEvidenceClassification(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	for _, tt := range []struct {
		name, prState string
		noPR          bool
		want          store.WorkAttemptTerminalState
	}{
		{"merged", "MERGED", false, store.WorkAttemptTerminalSuccess},
		{"newer open head", "OPEN", false, store.WorkAttemptTerminalSuccess},
		{"no pull request", "", true, store.WorkAttemptTerminalNoProgress},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := implementProgressIssue("new-head")
			issue.PullRequest.State = tt.prState
			issue.PullRequest.HeadCommittedAt = &now
			issue.PullRequest.MergeableState = "clean"
			issue.PullRequest.CIStatus = "success"
			issue.PullRequest.CheckRunCount = 1
			issue.PullRequest.DiffFingerprint = "same-diff"
			if tt.noPR {
				issue = implementProgressIssueWithoutPR()
			}
			issue.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil), UpdatedAt: &old}}
			before := cloneIssue(issue)
			if before.PullRequest != nil {
				before.PullRequest.HeadSHA = "old-head"
			}
			tracker := &implementProgressConnector{refreshed: issue, hydrated: issue}
			attempts := &implementProgressAttemptStore{history: []store.WorkAttempt{implementProgressHistoryAttempt(1, autoPromoteReworkSignature{PRNumber: 1070, HeadSHA: "old-head"}, store.WorkAttemptTerminalSuccess)}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand}}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			decision := orch.evaluateImplementCompletionProgress(t.Context(), Running{Issue: before, DispatchProgress: implementProgressArtifactSnapshot{PullRequestDiffFingerprint: "same-diff"}, WorkAttemptID: 42, StartedAt: old.Add(time.Minute), DiffStats: DiffStats{Status: "clean", HeadSHA: "new-head"}}, FinalStateCompleted, false)
			if decision.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s: %#v", decision.Outcome, tt.want, decision)
			}
			if !tt.noPR && decision.WorkpadStatus != workpad.StatusComplete {
				t.Fatalf("status = %q", decision.WorkpadStatus)
			}
			if tt.noPR && !completionWorkpadUnfinished(decision.Issue) {
				t.Fatal("no PR no longer continues implementation")
			}
		})
	}
}

func TestCompletionForgeEvidenceDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	for _, lane := range []string{"In Progress", "Rework"} {
		for _, tt := range []struct {
			name, prState string
			wantDispatch  bool
		}{
			{"merged", "MERGED", false},
			{"newer open head", "OPEN", false},
			{"no pull request", "", true},
		} {
			t.Run(lane+"/"+tt.name, func(t *testing.T) {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}})
				issue := dispatchTestIssueWithPullRequest("forge-evidence", lane, tt.prState)
				if tt.prState == "" {
					issue.PullRequest = nil
					issue.PRNumber = nil
				} else {
					issue.PullRequest.HeadSHA = "new-head"
					issue.PullRequest.HeadCommittedAt = &now
					issue.PullRequest.CIStatus = "success"
				}
				issue.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil), UpdatedAt: &old}}
				tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
				orch := &Orchestrator{cfg: cfg, connector: tracker}
				state := newState(cfg)
				state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted, CompletedAt: now, successfulAttemptPersisted: true}
				orch.reconcileStaleLinkedPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, now)
				orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)
				for _, update := range tracker.updates {
					if update.issueID == issue.ID {
						issue.State = update.state
					}
				}
				var selected bool
				newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, now, dispatchPlanHooks{decision: func(d dispatchPlanDecision) { selected = d.Selected }})
				if selected != tt.wantDispatch {
					t.Fatalf("dispatch = %v, want %v; state=%s updates=%#v", selected, tt.wantDispatch, issue.State, tracker.updates)
				}
				if !tt.wantDispatch && len(tracker.updates) == 0 {
					t.Fatal("finished PR did not leave implementation")
				}
			})
		}
	}
}

func TestCompletionForgeEvidencePreservesReceipt(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"OPEN", "MERGED"} {
		t.Run(state, func(t *testing.T) {
			old := time.Date(2026, 9, 21, 18, 59, 29, 0, time.UTC)
			head := old.Add(time.Hour)
			issue := implementProgressIssue("head")
			issue.PullRequest.State = state
			issue.PullRequest.HeadCommittedAt = &head
			issue.Comments = []connector.IssueComment{{UpdatedAt: &old, Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: in_progress\nfields:\n  completion_work_attempt_id: \"42\"\n  completion_generation: \"7\"\nblockers: []\nhuman_action: null\n```"}}
			running := Running{Issue: issue, WorkAttemptID: 42, Generation: 7}
			snapshot := implementProgressArtifactSnapshotFromIssue(issue, true)
			if snapshot.WorkpadStatus != workpad.StatusInProgress || snapshot.WorkpadReceiptHash != "" {
				t.Errorf("forge evidence fabricated a receipt: %#v", snapshot)
			}
			orch := &Orchestrator{cfg: normalizeConfig(Config{})}
			fingerprint, _ := orch.spendProgressArtifactFingerprint(running)
			if fingerprint.ReceiptHash != "" {
				t.Error("forge evidence credited an artifact receipt")
			}
			if completionCleanlinessAttempted(running, issue) {
				t.Error("forge evidence accepted an authored current-attempt claim")
			}
		})
	}
}

func TestMergedCompletionReconcilesClosedDraft(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Blocked", "Rework", "In Progress"} {
		for _, tt := range []struct {
			name   string
			change func(*connector.Issue, *connector.PullRequest, *connector.PullRequest)
			want   string
		}{
			{"verified merged replacement", nil, "Done"},
			{"actual merged checks red", func(_ *connector.Issue, merged, _ *connector.PullRequest) { merged.CIStatus = "failure" }, "Rework"},
			{"cached open draft", func(issue *connector.Issue, _, _ *connector.PullRequest) { issue.PullRequest.State = "OPEN" }, ""},
			{"native reopened draft", func(_ *connector.Issue, _, previous *connector.PullRequest) { previous.State = "OPEN" }, ""},
			{"mismatched old association", func(issue *connector.Issue, _, _ *connector.PullRequest) { number := 25; issue.PRNumber = &number }, ""},
			{"mismatched native old number", func(_ *connector.Issue, _, previous *connector.PullRequest) { previous.Number = 25 }, ""},
			{"unmerged claim", func(_ *connector.Issue, merged, _ *connector.PullRequest) { merged.State = "CLOSED" }, ""},
			{"missing ancestry", func(issue *connector.Issue, _, _ *connector.PullRequest) {
				issue.Comments[0].Body = strings.ReplaceAll(issue.Comments[0].Body, "completion_ancestry: verified", "completion_ancestry: unknown")
			}, ""},
			{"human action", func(issue *connector.Issue, _, _ *connector.PullRequest) {
				issue.Comments[0].Body = strings.ReplaceAll(issue.Comments[0].Body, "human_action: null", "human_action: decide")
			}, ""},
			{"different repository", func(issue *connector.Issue, _, _ *connector.PullRequest) {
				issue.Comments[0].Body = strings.ReplaceAll(issue.Comments[0].Body, "example/repo/pull/12", "other/repo/pull/12")
			}, ""},
			{"different native URL", func(_ *connector.Issue, merged, _ *connector.PullRequest) {
				merged.URL = "https://github.com/example/repo/pull/13"
			}, ""},
			{"different native number", func(_ *connector.Issue, merged, _ *connector.PullRequest) { merged.Number = 13 }, ""},
			{"different base", func(_ *connector.Issue, merged, _ *connector.PullRequest) { merged.BaseRef = "develop" }, ""},
			{"missing native head", func(_ *connector.Issue, merged, _ *connector.PullRequest) { merged.HeadSHA = "" }, ""},
			{"native hydration unavailable", func(_ *connector.Issue, merged, _ *connector.PullRequest) {
				merged.HydrationUnavailableReason = "unavailable"
			}, ""},
		} {
			t.Run(lane+"/"+tt.name, func(t *testing.T) {
				issue := completionTransitionIssue(lane, "CLOSED")
				issue.Identifier = "example/repo#1"
				issue.PullRequest.Number = 24
				issue.PullRequest.Draft = true
				issue.PullRequest.HeadSHA = "stale"
				issue.PRNumber = &issue.PullRequest.Number
				issue.Comments = []connector.IssueComment{{Body: mergedCompletionWorkpadBody()}}
				merged := &connector.PullRequest{Number: 12, URL: "https://github.com/example/repo/pull/12", State: "MERGED", HeadSHA: "merged-head", BaseRef: "main", CIStatus: "success"}
				previous := *issue.PullRequest
				if tt.change != nil {
					tt.change(&issue, merged, &previous)
				}
				originalNumber := *issue.PRNumber
				tracker := &dependencyAutoUnblockConnector{blockers: []connector.Issue{
					{Identifier: "example/repo#12", PullRequest: merged},
					{Identifier: "example/repo#24", PullRequest: &previous},
				}}
				orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
				orch.cfg = normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}, ObservedStates: []string{"Blocked"}, TerminalStates: []string{"Done"}})
				state := newState(orch.cfg)
				orch.reconcileStaleLinkedPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
				if tt.want == "" || tt.want == lane {
					if len(tracker.updates) != 0 {
						t.Fatalf("unexpected transition: %#v", tracker.updates)
					}
				} else if len(tracker.updates) != 1 || tracker.updates[0].state != tt.want {
					t.Fatalf("updates = %#v, want %s", tracker.updates, tt.want)
				}
				if issue.PullRequest.Number != 24 || *issue.PRNumber != originalNumber {
					t.Fatal("mutated source association")
				}
				if len(state.Running) != 0 || len(state.Completed) != 0 {
					t.Fatal("recovery required a worker receipt")
				}
			})
		}
	}
}

func TestMergedCompletionRefreshReplacesClosedDraft(t *testing.T) {
	t.Parallel()
	// Replay #3533's accepted attempt 7516 before continuation redispatched
	// attempt 7522 on the same clean workspace head four seconds later.
	for _, tc := range []struct {
		name     string
		change   func(*connector.Issue, *connector.Issue)
		wantDone bool
	}{
		{name: "recorded terminal success before redispatch", wantDone: true},
		{name: "stale attempt", change: func(i, _ *connector.Issue) {
			i.Comments[0].Body = strings.Replace(i.Comments[0].Body, `"7516"`, `"7514"`, 1)
		}},
		{name: "stale generation", change: func(i, _ *connector.Issue) {
			i.Comments[0].Body = strings.Replace(i.Comments[0].Body, `"118"`, `"117"`, 1)
		}},
		{name: "missing generation", change: func(i, _ *connector.Issue) {
			i.Comments[0].Body = strings.Replace(i.Comments[0].Body, "  completion_generation: \"118\"\n", "", 1)
		}},
		{name: "missing ancestry", change: func(i, _ *connector.Issue) {
			i.Comments[0].Body = strings.Replace(i.Comments[0].Body, "completion_ancestry: verified", "completion_ancestry: unknown", 1)
		}},
		{name: "missing evidence", change: func(i, _ *connector.Issue) {
			i.Comments[0].Body = strings.Replace(i.Comments[0].Body, "completion_evidence:", "unsupported_evidence:", 1)
		}},
		{name: "native evidence missing", change: func(_, pr *connector.Issue) { pr.PullRequest = nil }},
		{name: "native evidence unmerged", change: func(_, pr *connector.Issue) { pr.PullRequest.State = "CLOSED" }},
		{name: "native evidence unavailable", change: func(_, pr *connector.Issue) { pr.PullRequest.HydrationUnavailableReason = "unavailable" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finished := time.Date(2026, 10, 1, 1, 51, 8, 0, time.UTC)
			issue := completionTransitionIssue("In Progress", "")
			issue.Identifier = "digitaldrywood/detent#3533"
			body := strings.Replace(operationalCompletionWorkpadBody("TestCompletionFenceRevocationMigrationAndAccounting passed; true passed; git merge-base --is-ancestor exited 0."), "  completion_kind: operational", `  completion_kind: operational
  completion_work_attempt_id: "7516"
  completion_generation: "118"
  completion_merged_pr: https://github.com/digitaldrywood/detent/pull/3555
  completion_merge_commit: 16315bad5112b57efd68c9807edaebc1f9e7041a
  completion_branch: origin/develop
  completion_branch_head: 9eefd2335f86ea205c3b346cf9cd62281948b64a
  completion_ancestry: verified`, 1)
			issue.Comments = []connector.IssueComment{{Body: body, URL: "https://github.com/digitaldrywood/detent/issues/3533#issuecomment-5922344839"}}
			merged := connector.Issue{Identifier: "digitaldrywood/detent#3555", PullRequest: &connector.PullRequest{Number: 3555, URL: "https://github.com/digitaldrywood/detent/pull/3555", State: "MERGED", HeadSHA: "c4a26380b96eef9a432107c443cc634596077b7c", BaseRef: "develop"}}
			if tc.change != nil {
				tc.change(&issue, &merged)
			}
			tracker := &implementProgressConnector{refreshed: issue, hydrated: issue, resolvedBlockers: []connector.Issue{merged}}
			attempts := &recordingWorkAttemptStore{}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done", "Cancelled"}, AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand}}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: &hubSchedulingSource{}}
			state := newState(cfg)
			running := Running{Issue: issue, WorkAttemptID: 7516, Generation: 118, Attempt: 1, Mode: runpkg.RunModeImplement, StartedAt: time.Date(2026, 10, 1, 1, 47, 59, 0, time.UTC), DiffStats: DiffStats{Status: "clean", HeadSHA: "2a59b9ee06c2581461456e1fea0d0d75fcaa5022", RecoveryStateExpected: true, RecoveryStateAvailable: true}}
			state.Running[issue.ID] = running
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: finished, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement, WorkAttemptID: 7516, Generation: 118}, Result: runpkg.RunResult{FinalState: FinalStateCompleted, DiffStats: running.DiffStats}})
			if len(attempts.completions) != 1 {
				t.Fatalf("attempt receipts = %#v, want one terminal receipt", attempts.completions)
			}
			if !tc.wantDone {
				if len(tracker.updates) != 0 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress {
					t.Fatalf("unsupported completion accepted: updates=%#v receipt=%#v", tracker.updates, attempts.completions[0])
				}
				return
			}
			record := implementProgressRecordFromCompletion(t, attempts.completions[0])
			if attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess || record.Reason != implementMergedCompletionReason {
				t.Fatalf("receipt = %#v, want recorded merged terminal success", attempts.completions[0])
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
				t.Fatalf("updates = %#v, retry count = %d; want Done before another dispatch", tracker.updates, len(state.Retry))
			}
			if len(state.Retry) != 0 || len(state.Running) != 0 || len(state.Claimed) != 0 {
				t.Fatalf("completed merged delivery retained worker work: retry=%d running=%d claimed=%d", len(state.Retry), len(state.Running), len(state.Claimed))
			}
		})
	}
	for _, association := range []string{"closed", "number only", "absent", "missing native draft"} {
		t.Run(association, func(t *testing.T) {
			issue := completionTransitionIssue("Rework", "CLOSED")
			issue.Identifier = "example/repo#1"
			issue.PullRequest.Number = 24
			issue.PullRequest.HeadSHA = "stale"
			issue.PRNumber = &issue.PullRequest.Number
			previous := connector.Issue{Identifier: "example/repo#24", PullRequest: issue.PullRequest}
			issue.Comments = []connector.IssueComment{{Body: mergedCompletionWorkpadBody()}}
			merged := connector.Issue{Identifier: "example/repo#12", PullRequest: &connector.PullRequest{Number: 12, URL: "https://github.com/example/repo/pull/12", State: "MERGED", HeadSHA: "merged", BaseRef: "main", CIStatus: "success"}}
			if association == "number only" {
				issue.PullRequest = nil
			}
			if association == "absent" {
				issue.PullRequest = nil
				issue.PRNumber = nil
			}
			refs := []connector.Issue{merged, previous}
			if association == "missing native draft" {
				refs = refs[:1]
			}
			tracker := &implementProgressConnector{refreshed: issue, hydrated: issue, resolvedBlockers: refs}
			orch := &Orchestrator{cfg: normalizeConfig(Config{}), connector: tracker}
			refreshed, current := orch.refreshImplementCompletionIssue(t.Context(), issue)
			if !current {
				t.Fatal("workpad refresh failed")
			}
			if association == "missing native draft" {
				if refreshed.PullRequest.Number != 24 {
					t.Fatal("unverified old association replaced")
				}
				return
			}
			if !pullRequestMerged(refreshed.PullRequest) || refreshed.PullRequest.Number != 12 || *refreshed.PRNumber != 12 {
				t.Fatalf("replacement = %#v", refreshed.PullRequest)
			}
			if association == "closed" {
				decision := orch.evaluateImplementCompletionCandidate(t.Context(), Running{Issue: issue, DiffStats: DiffStats{Status: "clean"}}, FinalStateCompleted, false)
				if decision.Outcome != store.WorkAttemptTerminalSuccess || decision.Reason != implementMergedCompletionReason {
					t.Fatalf("completion = %s/%s, want success/%s", decision.Outcome, decision.Reason, implementMergedCompletionReason)
				}
			}
		})
	}
}
