package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
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
			DiffStats:              DiffStats{HeadSHA: "dispatch-head", TreeSHA: "dispatch-tree", Status: "clean"},
		}},
	})
	if len(attempts.heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1", len(attempts.heartbeats))
	}
	persistedStart = dispatchLoopStartFromMetadata(t, attempts.heartbeats[0].WorkerMetadataJSON)
	if !persistedStart.Captured || !persistedStart.Persisted || persistedStart.Fingerprint.WorkspaceHead != "dispatch-head" || persistedStart.Fingerprint.WorkspaceTree != "dispatch-tree" {
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
			DiffStats:              DiffStats{HeadSHA: "dispatch-head", TreeSHA: "dispatch-tree", Status: "clean"},
		}},
	})

	if state.Running[issue.ID].DispatchLoopStart.Persisted {
		t.Fatalf("dispatch loop start = %#v, want untrusted after persistence failure", state.Running[issue.ID].DispatchLoopStart)
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
