package project

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/orchestrator"
)

func admissionLaneWriter(owner func() *orchestrator.Orchestrator) func(context.Context, string, string) error {
	return func(ctx context.Context, issueID, target string) error {
		orch := owner()
		if orch == nil {
			return errors.New("admission orchestrator lane writer is unavailable")
		}
		_, err := orch.ReconcileOperatorMove(ctx, orchestrator.OperatorMoveRequest{IssueID: issueID, ToState: target, Reason: "backlog_admission", WriteTracker: true})
		return err
	}
}
