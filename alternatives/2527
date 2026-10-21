package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestHandleRunUpdateRecordsWorkerGitHubActor(t *testing.T) {
	t.Parallel()

	state := newState(Config{})
	issue := connector.Issue{ID: "issue-1988"}
	state.Running[issue.ID] = Running{Issue: issue}
	orch := &Orchestrator{}

	orch.handleRunUpdate(&state, runUpdate{
		issueID: issue.ID,
		usage: runpkg.UsageUpdate{
			WorkerGitHubActor: connector.IssueActor{Login: "detent-worker[bot]", Kind: "Bot"},
		},
	})

	if got := state.Running[issue.ID].WorkerGitHubActor; got.Login != "detent-worker[bot]" || got.Kind != "Bot" {
		t.Fatalf("WorkerGitHubActor = %#v, want Detent worker bot", got)
	}
}

func TestUsageUpdateHandlerDoesNotBlockWhenBufferIsFull(t *testing.T) {
	t.Parallel()

	orch := &Orchestrator{
		runUpdates: make(chan runUpdate, 1),
	}
	orch.runUpdates <- runUpdate{issueID: "queued"}

	done := make(chan error, 1)
	go func() {
		done <- orch.usageUpdateHandler(context.Background(), "issue-1", nil)(runpkg.UsageUpdate{
			TurnCount: 1,
			Tokens: runpkg.TokenTotals{
				InputTokens:  10,
				OutputTokens: 5,
				TotalTokens:  15,
			},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("usage update error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("usage update blocked with a full buffer")
	}
}

func TestUsageUpdateHandlerDeliversWorkerGitHubActor(t *testing.T) {
	t.Parallel()

	orch := &Orchestrator{
		runUpdates: make(chan runUpdate, 1),
	}
	orch.runUpdates <- runUpdate{issueID: "queued"}
	done := make(chan error, 1)

	go func() {
		done <- orch.usageUpdateHandler(t.Context(), "issue-1988", nil)(runpkg.UsageUpdate{
			WorkerGitHubActor: connector.IssueActor{Login: "detent-worker[bot]", Kind: "Bot"},
		})
	}()

	<-orch.runUpdates
	update := <-orch.runUpdates
	if update.issueID != "issue-1988" || update.usage.WorkerGitHubActor.Login != "detent-worker[bot]" {
		t.Fatalf("run update = %#v, want worker GitHub actor", update)
	}
	if err := <-done; err != nil {
		t.Fatalf("usage update error = %v", err)
	}
}

func TestUsageUpdateHandlerWaitsForDispatchLoopStartApplication(t *testing.T) {
	t.Parallel()

	orch := &Orchestrator{runUpdates: make(chan runUpdate, 1)}
	done := make(chan error, 1)
	go func() {
		done <- orch.usageUpdateHandler(t.Context(), "issue-2084", nil)(runpkg.UsageUpdate{
			DispatchLoopStart: &runpkg.DispatchLoopStartSnapshot{WorkspaceDiffAvailable: true},
		})
	}()

	update := <-orch.runUpdates
	if update.applied == nil || update.usage.DispatchLoopStart == nil {
		t.Fatalf("run update = %#v, want acknowledged dispatch loop start", update)
	}
	select {
	case err := <-done:
		t.Fatalf("usage update returned before application acknowledgment: %v", err)
	default:
	}
	close(update.applied)
	if err := <-done; err != nil {
		t.Fatalf("usage update error = %v", err)
	}
}

func TestUsageUpdateHandlerReturnsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	orch := &Orchestrator{
		runUpdates: make(chan runUpdate, 1),
	}

	err := orch.usageUpdateHandler(ctx, "issue-1", nil)(runpkg.UsageUpdate{})
	if err == nil {
		t.Fatal("usage update error = nil, want context canceled")
	}
}
