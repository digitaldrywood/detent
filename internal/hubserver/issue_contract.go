package hubserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeIssueContract(ctx context.Context, q nativeQueryer, scope nativeScope) (config.IssueContract, error) {
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		var nativeErr *nativeError
		if !errors.As(err, &nativeErr) || nativeErr.Code != "policy_mismatch" {
			return config.IssueContract{}, err
		}
	}
	return config.ResolvePolicyIssueContract(approval.Policy)
}

func nativeContractIssue(issue tracker.NativeIssue) connector.Issue {
	return connector.Issue{Title: issue.Title, Description: issue.Body, IssueContract: issue.IssueContract}
}

func nativeIssueContractConfig(ctx context.Context, q nativeQueryer, scope nativeScope) (config.Config, error) {
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return config.Config{}, err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, approval.Policy)
	return workflow.Config, err
}

func nativeIssueContractClaimable(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID) (bool, error) {
	var native string
	if err := q.QueryRowContext(ctx, "SELECT native_id, project_id FROM issues WHERE id = ? AND organization_id = ?", id, scope.organization).Scan(&native, &scope.project); err != nil {
		return false, err
	}
	issue, _, err := readNativeIssue(ctx, q, scope, native)
	if err != nil {
		return false, err
	}
	if strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return false, nil
	}
	if issue.LinkedSource != nil && issue.LinkedSource.Status != "complete" {
		return true, nil
	}
	if issue.IssueContract.Exempt {
		return true, nil
	}
	cfg, err := nativeIssueContractConfig(ctx, q, scope)
	if err != nil {
		return false, err
	}
	target := cfg.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) {
		return true, nil
	}
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return false, err
	}
	return contract.Evaluate(nativeContractIssue(issue)).Satisfied(), nil
}

func requireNativeIssueContractAtFiling(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue) error {
	cfg, err := nativeIssueContractConfig(ctx, q, scope)
	if err != nil {
		return nil
	}
	target := cfg.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return nil
	}
	contract, err := nativeIssueContract(ctx, q, scope)
	if err != nil {
		return fmt.Errorf("resolve issue contract: %w", err)
	}
	if missing := contract.Evaluate(nativeContractIssue(issue)).Missing; len(missing) > 0 {
		return nativeWorkflowInvalid(fmt.Sprintf("Add these required issue sections before filing into %s: %s. File into Backlog to draft it first.", issue.State, strings.Join(missing, ", ")))
	}
	return nil
}
