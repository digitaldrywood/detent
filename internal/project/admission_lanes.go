package project

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
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

func admissionDependencyIssues(owner func() *orchestrator.Orchestrator) func(context.Context) []connector.Issue {
	return func(context.Context) []connector.Issue {
		orch := owner()
		if orch == nil {
			return nil
		}
		return orch.DependencyIssues()
	}
}

func admissionIntakeEnabled(owner func() *orchestrator.Orchestrator) func() bool {
	return func() bool {
		orch := owner()
		return orch != nil && orch.LocalIntakeEnabled()
	}
}

func admissionContractCheck(owner func() *orchestrator.Orchestrator) func(context.Context, string, string) (bool, error) {
	return func(ctx context.Context, issueID, target string) (bool, error) {
		orch := owner()
		if orch == nil {
			return false, errors.New("admission orchestrator is unavailable")
		}
		result, err := orch.ReconcileOperatorMove(ctx, orchestrator.OperatorMoveRequest{IssueID: issueID, ToState: target, CheckIssueContract: true})
		return result.ContractSatisfied, err
	}
}
