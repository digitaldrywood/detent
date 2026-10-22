package hubserver

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func releaseDeliverySectionsMatch(body, approved string) bool {
	if approved == "" {
		return false
	}
	actual := config.IssueContractSectionDigests(body)
	for section, digest := range config.IssueContractSectionDigests(approved) {
		if actual[section] != digest {
			return false
		}
	}
	return true
}

func approveReleaseDeliveryContract(issue *tracker.NativeIssue, approved policy.Descriptor, now time.Time) error {
	if _, machine := issueorigin.Parse(issue.Body); !machine || approved.Authored == nil {
		return nil
	}
	if issue.IssueContract == nil {
		issue.IssueContract = &issuecontract.State{}
	}
	if len(issue.IssueContract.ConfirmedSections) > 0 || issue.IssueContract.HumanAction != "" || issue.State == "Blocked" || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approved)
	if err != nil {
		return err
	}
	body := contract.ReleaseDeliveryBody()
	if !releaseDeliverySectionsMatch(issue.Body, body) {
		return nil
	}
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.ReleaseDelivery = &issuecontract.ReleaseDeliveryApproval{PolicyID: approved.ID, BodySHA256: policy.Digest([]byte(issue.Body))}
	issue.IssueContract.RecordedAt = now
	return nil
}

func releaseDeliveryContractCurrent(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue, contract config.IssueContract) (bool, error) {
	if issue.IssueContract == nil || issue.IssueContract.ReleaseDelivery == nil {
		return true, nil
	}
	delegated := issue.IssueContract.ReleaseDelivery
	approved, err := requireProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return false, err
	}
	if approved.Policy.Authored == nil || approved.Policy.ID != delegated.PolicyID || policy.Digest([]byte(issue.Body)) != delegated.BodySHA256 || !releaseDeliverySectionsMatch(issue.Body, contract.ReleaseDeliveryBody()) {
		return false, nil
	}
	delivery, found, err := readReleaseDelivery(ctx, q, scope, string(issue.WorkItemID))
	if err != nil || !found {
		return false, err
	}
	if issue.IgnoreDependencies || !slices.Contains(issue.Dependencies, delivery.SourceIssueID) {
		return false, nil
	}
	project, err := readNativeProject(ctx, q, scope)
	if err != nil || !project.RequireDependencies {
		return false, err
	}
	source, _, err := readNativeIssue(ctx, q, scope, string(delivery.SourceIssueID))
	if err != nil || !source.Terminal {
		return false, err
	}
	change, err := readChange(ctx, q, scope, string(delivery.SourceIssueID), delivery.SourceChangeID)
	if err != nil {
		return false, err
	}
	landing := change.CurrentLanding()
	return landing != nil && landing.BaseRef == "develop" && landing.MergeSHA == delivery.SourceMergeSHA, nil
}

type nativeReleaseDeliveryDelegation struct {
	PolicyID string `json:"policy_id"`
	Body     string `json:"body"`
}

func readReleaseDeliveryDelegation(ctx context.Context, q nativeQueryer, scope nativeScope) (nativeReleaseDeliveryDelegation, error) {
	approval, found, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return nativeReleaseDeliveryDelegation{}, err
	}
	if !found || approval.Policy.Authored == nil {
		return nativeReleaseDeliveryDelegation{}, nil
	}
	contract, err := config.ResolvePolicyIssueContract(approval.Policy)
	if err != nil {
		return nativeReleaseDeliveryDelegation{}, err
	}
	body := contract.ReleaseDeliveryBody()
	if body == "" {
		return nativeReleaseDeliveryDelegation{}, nil
	}
	return nativeReleaseDeliveryDelegation{PolicyID: approval.Policy.ID, Body: body}, nil
}
