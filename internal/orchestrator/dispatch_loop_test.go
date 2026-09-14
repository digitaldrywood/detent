package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestDispatchLoopStartPersistence(t *testing.T) {
	t.Parallel()

	issue := implementProgressIssueWithoutPR()
	start := newDispatchLoopStartRecord(issue, runpkg.RunModeImplement)
	attempts := &implementProgressAttemptStore{}
	orch := &Orchestrator{
		cfg:          normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}}),
		workAttempts: attempts,
	}
	state := newState(orch.cfg)
	if _, ok := orch.startDurableWorkAttempt(t.Context(), &state, issue, 1, time.Now(), "local", runpkg.RunModeImplement, start); !ok {
		t.Fatal("startDurableWorkAttempt() = false")
	}
	if len(attempts.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(attempts.starts))
	}
	persistedStart := dispatchLoopStartFromMetadata(t, attempts.starts[0].WorkerMetadataJSON)
	if persistedStart.Captured || persistedStart.Persisted || !persistedStart.LaneAvailable || !persistedStart.PullRequestAvailable {
		t.Fatalf("initial dispatch loop start = %#v, want durable pre-workspace baseline", persistedStart)
	}

	state.Running[issue.ID] = Running{
		Issue:             issue,
		WorkAttemptID:     1,
		Mode:              runpkg.RunModeImplement,
		DispatchLoopStart: start,
	}
	orch.handleRunUpdate(&state, runUpdate{
		issueID: issue.ID,
		usage: runpkg.UsageUpdate{DispatchLoopStart: &runpkg.DispatchLoopStartSnapshot{
			WorkspaceDiffAvailable: true,
			WorkspaceHeadAvailable: true,
			DiffStats:              DiffStats{HeadSHA: "dispatch-head", Status: "clean"},
		}},
	})
	if len(attempts.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(attempts.heartbeats))
	}
	persistedStart = dispatchLoopStartFromMetadata(t, attempts.heartbeats[0].WorkerMetadataJSON)
	if !persistedStart.Captured || !persistedStart.Persisted || persistedStart.Fingerprint.WorkspaceHead != "dispatch-head" {
		t.Fatalf("heartbeat dispatch loop start = %#v, want complete persisted baseline", persistedStart)
	}
}

func TestDispatchLoopStartPersistenceFailureFailsOpen(t *testing.T) {
	t.Parallel()

	issue := implementProgressIssueWithoutPR()
	attempts := &implementProgressAttemptStore{heartbeatErr: errors.New("store unavailable")}
	orch := &Orchestrator{workAttempts: attempts}
	state := newState(Config{})
	state.Running[issue.ID] = Running{
		Issue:             issue,
		WorkAttemptID:     1,
		Mode:              runpkg.RunModeImplement,
		DispatchLoopStart: newDispatchLoopStartRecord(issue, runpkg.RunModeImplement),
	}

	orch.handleRunUpdate(&state, runUpdate{
		issueID: issue.ID,
		usage: runpkg.UsageUpdate{DispatchLoopStart: &runpkg.DispatchLoopStartSnapshot{
			WorkspaceDiffAvailable: true,
			WorkspaceHeadAvailable: true,
			DiffStats:              DiffStats{HeadSHA: "dispatch-head", Status: "clean"},
		}},
	})

	if state.Running[issue.ID].DispatchLoopStart.Persisted {
		t.Fatalf("dispatch loop start = %#v, want untrusted after persistence failure", state.Running[issue.ID].DispatchLoopStart)
	}
	decision := dispatchLoopDecision("In Progress", store.WorkAttemptTerminalFailure, autoPromoteReworkSignature{}, DiffStats{HeadSHA: "dispatch-head", Status: "clean"})
	got := orch.evaluateDispatchLoopProgress(t.Context(), state.Running[issue.ID], decision)
	if got.ConsecutiveNoProgress != 0 || got.Block {
		t.Fatalf("evaluateDispatchLoopProgress() = count %d block %v, want persistence failure to fail open", got.ConsecutiveNoProgress, got.Block)
	}
}

func dispatchLoopDecision(lane string, outcome store.WorkAttemptTerminalState, signature autoPromoteReworkSignature, diff DiffStats) implementCompletionProgressDecision {
	issue := connector.Issue{ID: "issue-loop", Identifier: "digitaldrywood/detent#1886", State: lane}
	diff = dispatchLoopTestRunnerDiff(diff)
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
	return dispatchLoopHistoryAttemptInLane(id, terminal, signature, diff, progressKinds, count, "Rework")
}

func dispatchLoopHistoryAttemptInLane(
	id int64,
	terminal store.WorkAttemptTerminalState,
	signature autoPromoteReworkSignature,
	diff implementProgressDiffStats,
	progressKinds []string,
	count int,
	lane string,
) store.WorkAttempt {
	if strings.TrimSpace(diff.HeadSHA) == "" {
		diff.HeadSHA = "same-workspace-head"
	}
	return store.WorkAttempt{
		ID:            id,
		ProjectID:     "detent",
		IssueID:       "issue-loop",
		Identifier:    "digitaldrywood/detent#1886",
		WorkerType:    "agent",
		Lane:          lane,
		Status:        store.WorkAttemptStatusTerminal,
		TerminalState: terminal,
		CompletedAt:   time.Date(2026, 8, 18, 14, int(id), 0, 0, time.UTC),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			dispatchLoopStartMetadataKey: dispatchLoopTestStart(lane, signature, diff),
			implementProgressMetadataKey: implementProgressRecord{
				Outcome:               string(terminal),
				Reason:                implementDependencyDeferralReason,
				CurrentSignature:      implementProgressSignatureRecordFromSignature(signature),
				WorkspaceDiffStats:    diff,
				TrackerState:          lane,
				ConsecutiveNoProgress: count,
				NoProgressLimit:       3,
				ProgressKinds:         progressKinds,
			},
		}),
	}
}

func dispatchLoopTestRunnerDiff(diff DiffStats) DiffStats {
	if strings.TrimSpace(diff.HeadSHA) == "" {
		diff.HeadSHA = "same-workspace-head"
	}
	return diff
}

func dispatchLoopTestStart(lane string, signature autoPromoteReworkSignature, diff implementProgressDiffStats) dispatchLoopStartRecord {
	return dispatchLoopStartRecord{
		Fingerprint:            dispatchLoopFingerprintFromValues(lane, signature, diff),
		Captured:               true,
		Persisted:              true,
		LaneAvailable:          true,
		PullRequestAvailable:   true,
		WorkspaceDiffAvailable: true,
		WorkspaceHeadAvailable: strings.TrimSpace(diff.HeadSHA) != "",
	}
}

func dispatchLoopStartFromMetadata(t *testing.T, metadata string) dispatchLoopStartRecord {
	t.Helper()
	var root struct {
		DispatchLoopStart dispatchLoopStartRecord `json:"dispatch_loop_start"`
	}
	if err := json.Unmarshal([]byte(metadata), &root); err != nil {
		t.Fatalf("unmarshal dispatch loop start metadata: %v", err)
	}
	return root.DispatchLoopStart
}
