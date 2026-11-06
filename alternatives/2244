package hubserver

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func applyHumanWorkflowContract(issue *tracker.NativeIssue, previousState string, request tracker.Transition, descriptor policy.Descriptor, workflow config.Workflow, now time.Time) error {
	contract, err := config.ResolvePolicyIssueContract(descriptor)
	if err != nil {
		return err
	}
	target := workflow.Config.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	signal, hold := workpad.SignalFromComment(request.ReasonDetail, "", "")
	hold = hold && signal != nil && signal.Invalid == nil && signal.Fields["issue_contract_return_state"] != ""
	if hold {
		if signal.Status != workpad.StatusBlocked || signal.HumanAction == "" {
			return nativePublicInvalid("Issue contract hold requires a human action")
		}
		if issue.IssueContract != nil && issue.IssueContract.Exempt || contract.Evaluate(nativeContractIssue(*issue)).Satisfied() {
			return nativePublicInvalid("Issue contract hold does not match the issue")
		}
		if signal.Fields["issue_contract_return_state"] != target || previousState != target && !slices.Contains(workflow.Config.BacklogAdmission.Sources.States, previousState) {
			return nativePublicInvalid("Issue contract hold must return to the configured admission lane")
		}
		if request.State != workflow.Config.Agent.StopRun.TargetState {
			return nativePublicInvalid("Issue contract failure destination does not match the workflow")
		}
		if issue.IssueContract == nil {
			issue.IssueContract = &issuecontract.State{}
		}
		issue.IssueContract.HumanAction = signal.HumanAction
		issue.IssueContract.ReturnState = target
		issue.IssueContract.RecordedAt = now
	} else if issue.IssueContract != nil && (strings.EqualFold(previousState, "Blocked") || previousState == workflow.Config.Agent.StopRun.TargetState) && previousState != request.State {
		issue.IssueContract.HumanAction = ""
	}
	if issue.IssueContract != nil && issue.IssueContract.ReturnState == request.State {
		if issue.IssueContract.HumanAction != "" || !contract.Evaluate(nativeContractIssue(*issue)).Satisfied() {
			return nativePublicInvalid("Human confirmation is required to clear the issue contract hold")
		}
		issue.IssueContract.ReturnState = ""
	}
	if issue.IssueContract != nil && issue.IssueContract.ReleaseDelivery != nil && strings.EqualFold(request.State, target) {
		return nativeWorkflowInvalid("The verified release delivery contract requires its current typed delivery authority")
	}
	return nil
}
