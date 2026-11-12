package hubserver

import (
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

type nativeReleaseDeliveryDelegation struct {
	PolicyID string `json:"policy_id"`
	Body     string `json:"body"`
}
