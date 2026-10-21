package orchestrator

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// Replays #3058's body publication before attempt 7018 ended, then the
// 14:14:11Z restart. A completed delivery must not need worker capacity again.
func TestOperationalBodyCompletionSurvivesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	finished := time.Date(2026, 9, 30, 13, 59, 24, 0, time.UTC)
	published := finished.Add(-27 * time.Second)
	restarted := time.Date(2026, 9, 30, 14, 14, 11, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		change      func(*connector.Issue)
		wantRestore bool
		wantState   string
	}{
		{name: "recorded authorized delivery", wantRestore: true, wantState: "Done"},
		{name: "body timestamp changes after acceptance", change: func(i *connector.Issue) { i.UpdatedAt = &restarted }, wantRestore: true, wantState: "Done"},
		{name: "stale attempt", change: func(i *connector.Issue) { i.Description = strings.Replace(i.Description, `"7018"`, `"7017"`, 1) }},
		{name: "stale generation", change: func(i *connector.Issue) { i.Description = strings.Replace(i.Description, `"25"`, `"24"`, 1) }},
		{name: "changed evidence", change: func(i *connector.Issue) {
			i.Description = strings.Replace(i.Description, "No Detent behavior change or PR required.", "Different work delivered.", 1)
		}},
		{name: "authorization withdrawn", change: func(i *connector.Issue) {
			i.Description = strings.Replace(i.Description, operationalCompletionAuthorizationBody(), "", 1)
		}},
		{name: "invalid receipt", change: func(i *connector.Issue) {
			i.Description = strings.Replace(i.Description, "status: complete", "status: unknown", 1)
		}},
		{name: "human review required", change: func(i *connector.Issue) { i.Labels = append(i.Labels, "requires-human-review") }, wantRestore: true, wantState: "Human Review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := completionDeferralIssue("I_kwDOSskuwc8AAAABS6rusg", "In Progress")
			issue.Identifier = "digitaldrywood/detent#3058"
			issue.Title = "fix(merge): honor required human visual approval for UI PRs"
			issue.URL = "https://github.com/digitaldrywood/detent/issues/3058"
			issue.Description = operationalCompletionAuthorizationBody()
			cfg := completionDeferralConfig()
			cfg.AutoPromote = normalizeAutoPromoteConfig(AutoPromoteConfig{Enabled: true, OptoutLabel: "requires-human-review", Gate: gate.Config{Kind: gate.KindCommand}})
			path := filepath.Join(t.TempDir(), "detent.db")
			backend := openCompletionDeferralStoreWithoutCleanup(t, path)
			id := startCompletionDeferralAttempt(t, backend, issue, finished)
			// Preserve the recorded attempt identity in the existing SQLite fixture.
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(t.Context(), "UPDATE work_attempts SET id = 7018, worker_type = 'agent', started_at = '2026-09-30T13:50:26Z' WHERE id = ?", id)
			if closeErr := db.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			running := completionDeferralRunning(issue, 7018, finished)
			running.Generation = 25
			running.StartedAt = time.Date(2026, 9, 30, 13, 50, 26, 0, time.UTC)
			running.DiffStats = DiffStats{Status: "clean", HeadSHA: "7398f3cb78f554e5d69126b50d0cc27e3be8c35c", RecoveryStateExpected: true, RecoveryStateAvailable: true}
			running.DispatchLoopStart = dispatchLoopTestStart(issue.State, autoPromoteReworkSignature{}, implementProgressDiffStatsFromDiffStats(running.DiffStats))
			running.DispatchProgress = implementProgressArtifactSnapshotFromIssue(issue, true)
			body := operationalCompletionWorkpadBody("No Detent behavior change or PR required.")
			body = strings.Replace(body, "fields:\n", "fields:\n  completion_work_attempt_id: \"7018\"\n  completion_generation: \"25\"\n", 1)
			issue.Description += "\n" + body
			issue.UpdatedAt = &published
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: backend}
			state := newState(cfg)
			state.Running[issue.ID] = running
			state.Claimed[issue.ID] = Claimed{Issue: running.Issue}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: finished, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement, WorkAttemptID: 7018, Generation: 25}, Result: runpkg.RunResult{FinalState: FinalStateCompleted, DiffStats: running.DiffStats}})
			receipt, err := backend.WorkAttempt(t.Context(), 7018)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("receipt = %s, want accepted operational success; metadata: %s", receipt.TerminalState, receipt.WorkerMetadataJSON)
			}
			unrelated := completionDeferralIssue("unrelated-reclaimed-at-restart", "In Progress")
			unrelatedID := startCompletionDeferralAttempt(t, backend, unrelated, finished)
			if tc.change != nil {
				tc.change(&issue)
			}
			tracker.stateIssues = []connector.Issue{issue}
			tracker.updates, tracker.comments = nil, nil
			// Cached acceptance must honor the same changed receipt as restart.
			orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, restarted)
			if !tc.wantRestore && len(tracker.updates) != 0 {
				t.Fatalf("cached invalid receipt changed lane: %#v", tracker.updates)
			}
			tracker.stateIssues = []connector.Issue{issue}
			tracker.updates, tracker.comments = nil, nil
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			backend = openCompletionDeferralStore(t, path)
			orch = Orchestrator{cfg: cfg, connector: tracker, workAttempts: backend}
			state = newState(cfg)
			orch.recoverDurableWorkAttempts(t.Context(), &state, restarted)
			reclaimed, err := backend.WorkAttempt(t.Context(), unrelatedID)
			if err != nil {
				t.Fatal(err)
			}
			if reclaimed.Status != store.WorkAttemptStatusTerminal {
				t.Fatalf("unrelated attempt was not reclaimed: %#v", reclaimed)
			}
			orch.restoreDurableGateWaitCompletions(t.Context(), &state, []connector.Issue{issue})
			completed, restored := state.Completed[issue.ID]
			if restored != tc.wantRestore {
				t.Fatalf("restored = %t, want %t", restored, tc.wantRestore)
			}
			if restored && completed.CompletionKind != workpad.CompletionOperational {
				t.Fatalf("completion kind = %q", completed.CompletionKind)
			}
			result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, restarted)
			if len(result.dispatchCandidates) != 0 || len(state.Running) != 0 {
				t.Fatal("operational delivery requested another worker")
			}
			if tc.wantState == "" {
				if len(tracker.updates) != 0 {
					t.Fatalf("invalid receipt changed lane: %#v", tracker.updates)
				}
			} else if len(tracker.updates) != 1 || tracker.updates[0].state != tc.wantState {
				t.Fatalf("updates = %#v, want %s", tracker.updates, tc.wantState)
			}
		})
	}
}
