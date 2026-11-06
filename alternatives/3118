package runnerstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ClaimStatePage struct {
	State      runnerauth.ClaimState
	Candidates []runnerauth.ReleasedExecution
	Next       string
}

type ClaimStateReader interface {
	RunnerClaimState(context.Context, string, tracker.ProjectID, string, int, time.Time) (ClaimStatePage, error)
}
