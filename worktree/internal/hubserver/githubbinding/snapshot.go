package githubbinding

import (
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
)

func ValidateSnapshot(snapshot githubcontract.ReconcileSnapshot) error {
	repository := snapshot.Repository
	if strings.TrimSpace(repository.NodeID) == "" || strings.TrimSpace(repository.Owner) == "" || strings.TrimSpace(repository.Name) == "" || repository.UpdatedAt.IsZero() {
		return errors.New("GitHub reconciliation returned an incomplete repository")
	}
	for _, issue := range snapshot.Issues {
		if strings.TrimSpace(issue.NodeID) == "" || issue.Number <= 0 || strings.TrimSpace(issue.Title) == "" || strings.TrimSpace(issue.URL) == "" || strings.TrimSpace(issue.State) == "" || issue.CreatedAt.IsZero() || issue.UpdatedAt.IsZero() {
			return fmt.Errorf("GitHub reconciliation returned incomplete issue %d", issue.Number)
		}
	}
	for _, pullRequest := range snapshot.PullRequests {
		if strings.TrimSpace(pullRequest.NodeID) == "" || pullRequest.Number <= 0 || strings.TrimSpace(pullRequest.Title) == "" || strings.TrimSpace(pullRequest.URL) == "" || strings.TrimSpace(pullRequest.State) == "" || strings.TrimSpace(pullRequest.HeadRef) == "" || strings.TrimSpace(pullRequest.HeadSHA) == "" || strings.TrimSpace(pullRequest.BaseRef) == "" || strings.TrimSpace(pullRequest.BaseSHA) == "" || pullRequest.CreatedAt.IsZero() || pullRequest.UpdatedAt.IsZero() {
			return fmt.Errorf("GitHub reconciliation returned incomplete pull request %d", pullRequest.Number)
		}
	}
	return nil
}
