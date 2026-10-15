package connector

import (
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func SecurityAuditSnapshot(projectID string, issue Issue, diff ValidationDiff, maxBytes int) (securityaudit.Snapshot, error) {
	if strings.TrimSpace(diff.Repository) == "" || strings.TrimSpace(diff.BaseSHA) == "" || strings.TrimSpace(diff.HeadSHA) == "" {
		return securityaudit.Snapshot{}, errors.New("security audit diff identity is unavailable")
	}
	if issue.PullRequest != nil && (issue.PullRequest.BaseSHA != diff.BaseSHA || issue.PullRequest.HeadSHA != diff.HeadSHA) {
		return securityaudit.Snapshot{}, errors.New("security audit diff differs from the current change")
	}
	if maxBytes <= 0 {
		maxBytes = securityaudit.DefaultMaxDiffBytes
	}
	snapshot := securityaudit.Snapshot{ProjectID: projectID, IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL, IssueTitle: issue.Title, IssueDescription: issue.Description, Repository: diff.Repository, PRNumber: diff.PRNumber, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Diff: diff.Patch, DiffBytes: len(diff.Patch)}
	if snapshot.DiffBytes > maxBytes {
		snapshot.DiffTruncated = true
		snapshot.Diff = snapshot.Diff[:maxBytes]
	}
	return snapshot, nil
}
