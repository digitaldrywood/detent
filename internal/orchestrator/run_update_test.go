package orchestrator

import (
	"context"
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestUsageUpdateHandlerDoesNotBlockWhenBufferIsFull(t *testing.T) {
	t.Parallel()

	orch := &Orchestrator{
		runUpdates: make(chan runUpdate, 1),
	}
	orch.runUpdates <- runUpdate{issueID: "queued"}

	done := make(chan error, 1)
	go func() {
		done <- orch.usageUpdateHandler(context.Background(), "issue-1")(runpkg.UsageUpdate{
			TurnCount: 1,
			Tokens: runpkg.CodexTotals{
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

func TestUsageUpdateHandlerReturnsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	orch := &Orchestrator{
		runUpdates: make(chan runUpdate, 1),
	}

	err := orch.usageUpdateHandler(ctx, "issue-1")(runpkg.UsageUpdate{})
	if err == nil {
		t.Fatal("usage update error = nil, want context canceled")
	}
}
