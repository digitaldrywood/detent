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

func (s intakeLaneStore) CreateComment(ctx context.Context, issueID, body string) error {
	commenter, ok := s.IssueStore.(intake.OccurrenceCommenter)
	if !ok {
		return connector.ErrNotImplemented
	}
	return commenter.CreateComment(ctx, issueID, body)
}

func (s intakeLaneStore) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) ([]connector.Issue, error) {
	reader, ok := s.IssueStore.(interface {
		FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error)
	})
	if !ok {
		return nil, connector.ErrNotImplemented
	}
	return reader.FetchIssueStatesByIDs(ctx, issueIDs)
}

type routineLaneStore struct {
	routine.IssueStore
	write func(context.Context, string, string) error
}

func (s routineLaneStore) SetIntakeIssueState(ctx context.Context, issueID, target string) error {
	return s.write(ctx, issueID, target)
}

func (s routineLaneStore) CreateComment(ctx context.Context, issueID, body string) error {
	commenter, ok := s.IssueStore.(intake.OccurrenceCommenter)
	if !ok {
		return connector.ErrNotImplemented
	}
	return commenter.CreateComment(ctx, issueID, body)
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
