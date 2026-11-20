package orchestrator

import (
	"testing"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestDeferredStandalonePlanRetriesInsideClaim(t *testing.T) {
	t.Parallel()
	record := newDeferredCompletion(runpkg.Completion{
		Request: runpkg.RunRequest{Mode: runpkg.RunModePlan, WorkAttemptID: 17},
		Result:  runpkg.RunResult{Output: "Stored standalone plan", FinalState: runpkg.FinalStateCompleted},
	}, Running{}, nil, time.Now())
	replayed := record.completion()
	if replayed.Err == nil || !replayed.Retryable || replayed.Request.Mode != runpkg.RunModeImplement || replayed.Request.WorkAttemptID != 17 || replayed.Result.Output != "" {
		t.Fatalf("standalone completion did not become an in-claim retry: %+v", replayed)
	}
}
