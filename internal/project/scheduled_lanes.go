package project

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/routine"
)

type intakeLaneStore struct {
	intake.IssueStore
	write func(context.Context, string, string) error
}

func (s intakeLaneStore) SetIntakeIssueState(ctx context.Context, issueID, target string) error {
	return s.write(ctx, issueID, target)
}

type routineLaneStore struct {
	routine.IssueStore
	write func(context.Context, string, string) error
}

func (s routineLaneStore) SetIntakeIssueState(ctx context.Context, issueID, target string) error {
	return s.write(ctx, issueID, target)
}

func scheduledLaneWriter(owner func() *orchestrator.Orchestrator, tracker connector.Connector, reason string) func(context.Context, string, string) error {
	return func(ctx context.Context, issueID, target string) error {
		orch := owner()
		if orch == nil {
			return errors.New("scheduled issue orchestrator lane writer is unavailable")
		}
		_, err := orch.ReconcileOperatorMove(ctx, orchestrator.OperatorMoveRequest{Tracker: tracker, IssueID: issueID, ToState: target, Reason: reason, WriteTracker: true})
		return err
	}
}
