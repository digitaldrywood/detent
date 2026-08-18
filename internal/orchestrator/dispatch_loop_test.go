package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestEvaluateDispatchLoopProgress(t *testing.T) {
	t.Parallel()

	clean := implementProgressDiffStats{Status: "clean"}
	dirty := implementProgressDiffStats{FilesChanged: 1, AddedLines: 4, Fingerprint: "same-diff", Status: "changed"}
	signature := autoPromoteReworkSignature{PRNumber: 42, HeadSHA: "same-head"}
	tests := []struct {
		name            string
		history         []store.WorkAttempt
		running         Running
		decision        implementCompletionProgressDecision
		wantCount       int
		wantBlock       bool
		wantReason      string
		wantBlockReason string
		limit           int
	}{
		{
			name: "successful terminal states count",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, clean, nil, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, clean, nil, 1),
			},
			running:         dispatchLoopRunning("Rework", DiffStats{Status: "clean"}),
			decision:        dispatchLoopDecision("Rework", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, DiffStats{Status: "clean"}),
			wantCount:       3,
			wantBlock:       true,
			wantReason:      dispatchLoopDetectedReason,
			wantBlockReason: dispatchLoopDetectedReason,
		},
		{
			name: "failure terminal states count",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalFailure, autoPromoteReworkSignature{}, clean, nil, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalFailure, autoPromoteReworkSignature{}, clean, nil, 1),
			},
			running:         dispatchLoopRunning("Rework", DiffStats{Status: "clean"}),
			decision:        dispatchLoopDecision("Rework", store.WorkAttemptTerminalFailure, autoPromoteReworkSignature{}, DiffStats{Status: "clean"}),
			wantCount:       3,
			wantBlock:       true,
			wantReason:      dispatchLoopDetectedReason,
			wantBlockReason: dispatchLoopDetectedReason,
		},
		{
			name:       "single completed run does not trip even with limit one",
			running:    dispatchLoopRunning("Rework", DiffStats{Status: "clean"}),
			decision:   dispatchLoopDecision("Rework", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, DiffStats{Status: "clean"}),
			wantCount:  1,
			wantReason: implementDependencyDeferralReason,
			limit:      1,
		},
		{
			name: "diff advancement resets",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, dirty, nil, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, dirty, nil, 1),
			},
			running:    dispatchLoopRunning("Rework", DiffStats{FilesChanged: 2, AddedLines: 8, Fingerprint: "new-diff", Status: "changed"}),
			decision:   dispatchLoopDecision("Rework", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, DiffStats{FilesChanged: 2, AddedLines: 8, Fingerprint: "new-diff", Status: "changed"}),
			wantReason: implementDependencyDeferralReason,
		},
		{
			name: "commit advancement resets",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, implementProgressDiffStats{HeadSHA: "old-head", Status: "clean"}, nil, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, implementProgressDiffStats{HeadSHA: "old-head", Status: "clean"}, nil, 1),
			},
			running:    dispatchLoopRunning("Rework", DiffStats{HeadSHA: "new-head", Status: "clean"}),
			decision:   dispatchLoopDecision("Rework", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, DiffStats{HeadSHA: "new-head", Status: "clean"}),
			wantReason: implementDependencyDeferralReason,
		},
		{
			name: "pull request advancement resets",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalSuccess, signature, clean, nil, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalSuccess, signature, clean, nil, 1),
			},
			running:    dispatchLoopRunning("Rework", DiffStats{Status: "clean"}),
			decision:   dispatchLoopDecision("Rework", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{PRNumber: 42, HeadSHA: "new-head"}, DiffStats{Status: "clean"}),
			wantReason: implementDependencyDeferralReason,
		},
		{
			name: "lane advancement resets",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, clean, nil, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, clean, nil, 1),
			},
			running:    dispatchLoopRunning("Rework", DiffStats{Status: "clean"}),
			decision:   dispatchLoopDecision("In Progress", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, DiffStats{Status: "clean"}),
			wantReason: implementDependencyDeferralReason,
		},
		{
			name: "workpad-only changes do not reset",
			history: []store.WorkAttempt{
				dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, clean, []string{"audit_artifact"}, 2),
				dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, clean, []string{"workpad_predicate"}, 1),
			},
			running:         dispatchLoopRunning("Rework", DiffStats{Status: "clean"}),
			decision:        dispatchLoopDecision("Rework", store.WorkAttemptTerminalSuccess, autoPromoteReworkSignature{}, DiffStats{Status: "clean"}),
			wantCount:       3,
			wantBlock:       true,
			wantReason:      dispatchLoopDetectedReason,
			wantBlockReason: dispatchLoopDetectedReason,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.limit > 0 {
				tt.decision.NoProgressLimit = tt.limit
			}
			orch := &Orchestrator{
				cfg: Config{
					Project:                 scheduler.ProjectCandidate{ID: "detent"},
					NoProgressSpendLimitUSD: 0,
				},
				workAttempts: &implementProgressAttemptStore{history: tt.history},
			}

			got := orch.evaluateDispatchLoopProgress(t.Context(), tt.running, tt.decision)

			if got.ConsecutiveNoProgress != tt.wantCount || got.Block != tt.wantBlock || got.Reason != tt.wantReason || got.BlockReason != tt.wantBlockReason {
				t.Fatalf("evaluateDispatchLoopProgress() = count %d block %v reason %q block reason %q, want %d %v %q %q", got.ConsecutiveNoProgress, got.Block, got.Reason, got.BlockReason, tt.wantCount, tt.wantBlock, tt.wantReason, tt.wantBlockReason)
			}
		})
	}
}

func TestHandleRunResultTripsDispatchLoopAfterFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.UTC)
	issue := implementProgressIssueWithoutPR()
	issue.State = "Rework"
	history := []store.WorkAttempt{
		dispatchLoopHistoryAttempt(2, store.WorkAttemptTerminalFailure, autoPromoteReworkSignature{}, implementProgressDiffStats{Status: "clean"}, nil, 2),
		dispatchLoopHistoryAttempt(1, store.WorkAttemptTerminalFailure, autoPromoteReworkSignature{}, implementProgressDiffStats{Status: "clean"}, nil, 1),
	}
	tracker := &implementProgressConnector{refreshed: issue}
	attempts := &implementProgressAttemptStore{history: history}
	cfg := normalizeConfig(Config{
		Project:        scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote:    AutoPromoteConfig{NoProgressLimit: 3},
		ActiveStates:   []string{"Rework"},
		ObservedStates: []string{"Blocked"},
		TerminalStates: []string{"Done"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:               issue,
		Attempt:             1,
		WorkAttemptID:       42,
		Mode:                runpkg.RunModeImplement,
		DispatchSourceState: "Rework",
		StartedAt:           now.Add(-time.Minute),
		DiffStats:           DiffStats{Status: "clean"},
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result:      runpkg.RunResult{FinalState: runpkg.FinalStateFailed, DiffStats: DiffStats{Status: "clean"}},
		Err:         errors.New("worker failed without progress"),
	})

	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress || attempts.completions[0].ErrorClass != dispatchLoopDetectedReason {
		t.Fatalf("completions = %#v, want dispatch-loop no-progress terminal", attempts.completions)
	}
	if blocked, ok := state.Blocked[issue.ID]; !ok || blocked.Reason != dispatchLoopDetectedReason {
		t.Fatalf("Blocked[%q] = %#v, %v", issue.ID, blocked, ok)
	}
	if len(tracker.comments) != 1 || tracker.comments[0].body == "" {
		t.Fatalf("comments = %#v, want loop-specific park comment", tracker.comments)
	}
}

func dispatchLoopRunning(lane string, diff DiffStats) Running {
	issue := connector.Issue{ID: "issue-loop", Identifier: "digitaldrywood/detent#1886", State: lane}
	return Running{Issue: issue, DispatchSourceState: lane, DiffStats: diff, Mode: runpkg.RunModeImplement}
}

func dispatchLoopDecision(lane string, outcome store.WorkAttemptTerminalState, signature autoPromoteReworkSignature, diff DiffStats) implementCompletionProgressDecision {
	issue := connector.Issue{ID: "issue-loop", Identifier: "digitaldrywood/detent#1886", State: lane}
	return implementCompletionProgressDecision{
		Issue:              issue,
		Outcome:            outcome,
		Reason:             implementDependencyDeferralReason,
		CurrentSignature:   signature,
		WorkspaceDiffStats: diff,
		TrackerState:       lane,
		NoProgressLimit:    3,
	}
}

func dispatchLoopHistoryAttempt(
	id int64,
	terminal store.WorkAttemptTerminalState,
	signature autoPromoteReworkSignature,
	diff implementProgressDiffStats,
	progressKinds []string,
	count int,
) store.WorkAttempt {
	return store.WorkAttempt{
		ID:            id,
		ProjectID:     "detent",
		IssueID:       "issue-loop",
		Identifier:    "digitaldrywood/detent#1886",
		WorkerType:    "agent",
		Lane:          "Rework",
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: terminal,
		CompletedAt:   time.Date(2026, 8, 18, 14, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			implementProgressMetadataKey: implementProgressRecord{
				Outcome:               string(terminal),
				Reason:                implementDependencyDeferralReason,
				CurrentSignature:      implementProgressSignatureRecordFromSignature(signature),
				WorkspaceDiffStats:    diff,
				TrackerState:          "Rework",
				ConsecutiveNoProgress: count,
				NoProgressLimit:       3,
				ProgressKinds:         progressKinds,
			},
		}),
	}
}
