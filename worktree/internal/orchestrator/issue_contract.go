package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (o *Orchestrator) issueContractApplies(issue connector.Issue) bool {
	if o.cfg.IssueContract == nil {
		return false
	}
	if issue.IssueContract != nil {
		return !issue.IssueContract.Exempt
	}
	at := issue.StageUpdatedAt
	if at == nil {
		at = issue.CreatedAt
	}
	return at != nil && !o.cfg.IssueContractRolloutAt.IsZero() && !at.Before(o.cfg.IssueContractRolloutAt)
}

func (o *Orchestrator) enforceIssueContract(ctx context.Context, state *State, issue connector.Issue, returnState string, now time.Time) (bool, error) {
	if o.cfg.IssueContract == nil || issue.IssueContract != nil && issue.IssueContract.Exempt {
		return true, nil
	}
	if strings.Contains(strings.ToLower(issue.Description), "detent:no-dispatch") {
		return false, nil
	}
	evaluation := o.cfg.IssueContract.Evaluate(issue)
	if evaluation.Satisfied() {
		return true, nil
	}
	action := o.cfg.IssueContract.HumanAction(issue, evaluation)
	status := struct {
		Schema      int               `yaml:"schema"`
		Status      string            `yaml:"status"`
		Blockers    []workpad.Blocker `yaml:"blockers"`
		HumanAction string            `yaml:"human_action"`
		Fields      map[string]string `yaml:"fields"`
	}{1, workpad.StatusBlocked, []workpad.Blocker{}, action, map[string]string{"issue_contract_return_state": returnState}}
	raw, err := yaml.Marshal(status)
	if err != nil {
		return false, err
	}
	body := "## Codex Workpad\n\n```detent-status\n" + string(raw) + "```"
	target := firstNonBlank(o.cfg.StopRunTargetState, blockedStatusState)
	metadata := o.newBlockedRecoveryMetadata(ctx, issue, RunModeImplement, workpadBlockedUnactionedReason, blockedRecoveryPredicateManaged, returnState, DiffStats{})
	metadata.ReasonDetail = body
	metadata.BlockedRecovery.Owner = blockedRecoveryOwnerHuman
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, target, now, "state_transition", metadata); err != nil {
		return false, err
	}
	if issue.IssueContract == nil {
		if err := o.connector.CreateComment(ctx, issue.ID, body); err != nil {
			return false, fmt.Errorf("publish issue contract human action: %w", err)
		}
	}
	issue.State = target
	issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: action, RecordedAt: &now}
	issue.IssueContract = &issuecontract.State{HumanAction: action, ReturnState: returnState, RecordedAt: now}
	if state != nil {
		state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus, Reason: workpadBlockedUnactionedReason, BlockedAt: now, NeedsHumanAttention: true, Recovery: metadata.BlockedRecovery, RecoveryTarget: returnState}
	}
	return false, nil
}

func (o *Orchestrator) filterIssueContracts(ctx context.Context, state *State, issues []connector.Issue, now time.Time) []connector.Issue {
	filtered := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		_, running := state.Running[issue.ID]
		if running || !o.issueContractApplies(issue) || !strings.EqualFold(issue.State, firstNonBlank(o.cfg.AdmissionTargetState, "Todo")) {
			filtered = append(filtered, issue)
			continue
		}
		ok, err := o.enforceIssueContract(ctx, state, issue, issue.State, now)
		if err != nil && o.logger != nil {
			o.logger.Warn("issue contract placement failed", "issue_id", issue.ID, "error", err)
		}
		if ok && err == nil {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}
