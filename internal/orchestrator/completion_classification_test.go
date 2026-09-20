package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestUnfinishedCompletionClassification(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"In Progress", "Rework"} {
		for _, changed := range []bool{false, true} {
			for _, prose := range []string{"", "Rebased and lease-pushed. No production implementation added in this pass."} {
				t.Run(fmt.Sprintf("%s/changed=%t/%s", lane, changed, prose), func(t *testing.T) {
					t.Parallel()
					issue := implementProgressIssue("rebased-head")
					issue.State = lane
					issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```\n" + prose}}
					before := cloneIssue(issue)
					before.PullRequest.HeadSHA = "before"
					before.PullRequest.DiffFingerprint = "same-diff"
					issue.PullRequest.DiffFingerprint = "same-diff"
					want := store.WorkAttemptTerminalNoProgress
					if changed {
						issue.PullRequest.DiffFingerprint = "changed-diff"
						want = store.WorkAttemptTerminalSuccess
					}
					tracker := &implementProgressConnector{refreshed: issue, hydrated: issue}
					attempts := &implementProgressAttemptStore{history: []store.WorkAttempt{implementProgressHistoryAttempt(1, autoPromoteReworkSignature{PRNumber: 1070, HeadSHA: "before"}, store.WorkAttemptTerminalSuccess)}}
					cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}})
					orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
					state := newState(cfg)
					now := time.Now()
					state.Running[issue.ID] = Running{Issue: before, DispatchProgress: implementProgressArtifactSnapshot{PullRequestDiffFingerprint: "same-diff"}, WorkAttemptID: 42, Attempt: 1, Mode: runpkg.RunModeImplement, DispatchSourceState: lane, StartedAt: now.Add(-time.Minute)}
					state.Claimed[issue.ID] = Claimed{Issue: issue}
					orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement}, Result: runpkg.RunResult{FinalState: FinalStateCompleted, DiffStats: DiffStats{Status: "clean", HeadSHA: "rebased-head"}}})
					if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != want {
						t.Fatalf("completion = %#v; want %s", attempts.completions, want)
					}
					if attempts.completions[0].Phase == "awaiting_gate" || autoPromoteActiveGatePendingIssue(issue, &state, cfg, cfg.AutoPromote) {
						t.Fatal("unfinished session entered gate wait")
					}
					orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)
					if len(tracker.updates) != 0 {
						t.Fatalf("unexpected lane updates: %#v", tracker.updates)
					}
					if changed && (len(state.FailureBreaker.Failures) != 0 || len(state.RepeatedFailures) != 0 || len(state.InstantFailures) != 0 || attempts.completions[0].ErrorClass != "") {
						t.Fatal("genuine progress recorded failure evidence")
					}
					if _, ok := state.Retry[issue.ID]; !ok {
						t.Fatal("missing allowance-controlled continuation")
					}
				})
			}
		}
	}
}

func TestCompletionRebaseProgress(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, before, after, status string
		want                        store.WorkAttemptTerminalState
	}{
		{"identical diff after rebase", "same-diff", "same-diff", "", store.WorkAttemptTerminalNoProgress},
		{"identical diff with completion assertion", "same-diff", "same-diff", "complete", store.WorkAttemptTerminalNoProgress},
		{"implementation changes", "old-diff", "new-diff", "complete", store.WorkAttemptTerminalSuccess},
		{"unfinished with genuine progress", "old-diff", "new-diff", "in_progress", store.WorkAttemptTerminalSuccess},
		{"unavailable baseline", "", "new-diff", "", store.WorkAttemptTerminalSuccess},
		{"unavailable current diff", "old-diff", "", "", store.WorkAttemptTerminalSuccess},
	} {
		for _, lane := range []string{"In Progress", "Rework"} {
			t.Run(tt.name+"/"+lane, func(t *testing.T) {
				t.Parallel()
				before := implementProgressIssue("before")
				before.State = lane
				before.PullRequest.DiffFingerprint = tt.before
				after := implementProgressIssue("rebased")
				after.State = lane
				after.PullRequest.DiffFingerprint = tt.after
				if tt.status != "" {
					after.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: " + tt.status + "\nblockers: []\nhuman_action: null\n```"}}
				}
				tracker := &implementProgressConnector{refreshed: after, hydrated: after}
				cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}}})
				orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: &implementProgressAttemptStore{}}
				running := Running{Issue: before, DispatchSourceState: lane, Mode: runpkg.RunModeImplement, DiffStats: DiffStats{Status: "clean", HeadSHA: "rebased", CommitsAhead: 2}}
				running.DispatchProgress.PullRequestDiffFingerprint = orch.implementCompletionDiffFingerprint(t.Context(), before)
				decision := orch.evaluateImplementCompletionProgress(t.Context(), running, FinalStateCompleted, true)
				// Published commits ahead of main must not mask rebase-only classification.
				// Also test routing with fully clean workspace evidence, so the gate
				// would otherwise upgrade a complete Workpad into a successful wait.
				decision.WorkspaceDiffStats.CommitsAhead = 0
				decision, wait := completedReworkGateWaitProgress(running, decision, cfg, FinalStateCompleted)
				if decision.Outcome != tt.want {
					t.Fatalf("outcome = %s (%s), want %s", decision.Outcome, decision.Reason, tt.want)
				}
				if tt.want == store.WorkAttemptTerminalNoProgress && wait != "" {
					t.Fatalf("no-progress session entered gate wait: %s", wait)
				}
				if lane == "Rework" && tt.status == "complete" && tt.want == store.WorkAttemptTerminalSuccess && wait == "" {
					t.Fatal("ready control did not exercise successful gate wait")
				}
			})
		}
	}
}

func TestUnfinishedSessionsExhaustAttemptAllowance(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"In Progress", "Rework"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			issue := implementProgressIssue("initial")
			issue.PullRequest.DiffFingerprint = "unchanged-diff"
			issue.State = lane
			issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```\nRebased only; implementation remains outstanding."}}
			cfg := laneMutationTestConfig()
			db, id := openLaneMutationTestStore(t, t.Context(), cfg.Project.ID, issue, now.Add(-time.Hour))
			tracker := &attemptTriageConnector{implementProgressConnector: implementProgressConnector{refreshed: issue, hydrated: issue}}
			orch := newLaneMutationTestOrchestrator(cfg, tracker, db, db, now)
			state := newState(cfg)
			for i := range sessionsWithoutMergeAllowance {
				started := now.Add(time.Duration(i) * time.Minute)
				if i > 0 {
					var err error
					id, err = db.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, WorkerType: "agent", Lane: lane, AttemptNumber: i + 1, StartedAt: started})
					if err != nil {
						t.Fatal(err)
					}
				}
				before := cloneIssue(issue)
				issue.PullRequest.HeadSHA += "-rebased"
				tracker.refreshed = issue
				tracker.hydrated = issue
				state.Running[issue.ID] = Running{Issue: before, DispatchProgress: implementProgressArtifactSnapshot{PullRequestDiffFingerprint: "unchanged-diff"}, WorkAttemptID: id, Attempt: i + 1, Mode: runpkg.RunModeImplement, DispatchSourceState: lane, StartedAt: started}
				state.Claimed[issue.ID] = Claimed{Issue: issue}
				orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: started.Add(time.Second), Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement}, Result: runpkg.RunResult{FinalState: FinalStateCompleted, PullRequestUpdated: true, DiffStats: DiffStats{Status: "clean", HeadSHA: issue.PullRequest.HeadSHA}}})
				attempt, err := db.WorkAttempt(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if attempt.TerminalState != store.WorkAttemptTerminalNoProgress || attempt.Phase == "awaiting_gate" {
					t.Fatalf("attempt = %s/%s", attempt.TerminalState, attempt.Phase)
				}
				if len(tracker.updates) != 0 {
					t.Fatalf("unexpected transition: %#v", tracker.updates)
				}
			}
			// Restart from durable history; the fourth dispatch must be triage, never implementation.
			orch = newLaneMutationTestOrchestrator(cfg, tracker, db, db, now.Add(time.Hour))
			orch.supervisor = newTestSupervisor(t, attemptTriageRunner{}, cfg)
			orch.runResults = make(chan runpkg.Completion, 1)
			state = newState(cfg)
			if !orch.dispatchIssue(t.Context(), &state, issue, 4, now.Add(time.Hour), "") {
				t.Fatal("triage dispatch refused")
			}
			select {
			case result := <-orch.runResults:
				if result.Request.Mode != runpkg.RunModeTriage {
					t.Fatalf("fourth mode = %s", result.Request.Mode)
				}
				orch.handleRunResult(t.Context(), &state, result)
			case <-time.After(5 * time.Second):
				t.Fatal("triage did not complete")
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != blockedStatusState {
				t.Fatalf("allowance did not stop issue: %#v", tracker.updates)
			}
			timeline, err := db.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: cfg.Project.ID, IssueID: issue.ID})
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range timeline.Events {
				if event.Reason == attemptAllowanceExhaustedReason {
					return
				}
			}
			t.Fatal("allowance stop reason missing")
		})
	}
}

type completionFingerprintConnector struct {
	implementProgressConnector
	fingerprint string
	err         error
	calls       int
}

func (c *completionFingerprintConnector) PullRequestDiffFingerprint(_ context.Context, _ connector.Issue) (string, error) {
	c.calls++
	return c.fingerprint, c.err
}

func TestCompletionDiffFingerprint(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, cached, fetched, want string
		noPR, failure               bool
		calls                       int
	}{
		{name: "cached", cached: "same-diff", want: "same-diff"},
		{name: "fetch", fetched: " new-diff ", want: "new-diff", calls: 1},
		{name: "no PR", noPR: true},
		{name: "unavailable", calls: 1},
		{name: "instance read failure", failure: true, calls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := implementProgressIssue("head")
			issue.PullRequest.DiffFingerprint = tt.cached
			if tt.noPR {
				issue.PullRequest = nil
			}
			tracker := &completionFingerprintConnector{fingerprint: tt.fetched}
			if tt.failure {
				tracker.err = errors.New("forge unavailable")
			}
			orch := &Orchestrator{connector: tracker}
			if got := orch.implementCompletionDiffFingerprint(t.Context(), issue); got != tt.want {
				t.Fatalf("fingerprint = %q, want %q", got, tt.want)
			}
			if tracker.calls != tt.calls {
				t.Fatalf("reads = %d, want %d", tracker.calls, tt.calls)
			}
		})
	}
}
