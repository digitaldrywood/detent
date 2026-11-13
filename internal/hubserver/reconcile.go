package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	checkpointWebhook     = "webhook"
	checkpointIncremental = "incremental"
	checkpointFullRepair  = "full_repair"
)

type reconcileTarget struct {
	RepositoryTarget
	Profile           string
	RepositoryEnabled bool
	Cursor            *time.Time
	HydrationSince    *time.Time
	Hydrations        []HydrationRequest
}

func (s *Service) reconcileAllRepositories(ctx context.Context, mode ReconcileMode) {

}

func (s *Service) reconcileRepository(ctx context.Context, target reconcileTarget, mode ReconcileMode) error {
	return ErrPostgresFeatureUnavailable
}

func (d *database) reconcileTargets(ctx context.Context) ([]reconcileTarget, error) {
	return nil, ErrPostgresFeatureUnavailable
}

func (d *database) pendingHydrationRequests(ctx context.Context, repositoryID int64) ([]HydrationRequest, *time.Time, error) {
	return nil, nil, ErrPostgresFeatureUnavailable
}

func (d *database) applyReconcileSnapshot(ctx context.Context, target reconcileTarget, mode ReconcileMode, cursor time.Time, completedAt time.Time, snapshot ReconcileSnapshot) error {
	return ErrPostgresFeatureUnavailable
}

func validateReconcileSnapshot(snapshot ReconcileSnapshot) error {
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

func (d *database) recordCheckpointStarted(ctx context.Context, repositoryID int64, name string, now time.Time) error {
	return ErrPostgresFeatureUnavailable
}

func (d *database) recordCheckpointFailure(ctx context.Context, repositoryID int64, name string, now time.Time, failure error) error {
	return ErrPostgresFeatureUnavailable
}

func parseCheckpointTime(value sql.NullString) (time.Time, bool, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, false, err
	}
	parsed = parsed.UTC()
	return parsed, true, nil
}

func earliestTime(values ...*time.Time) *time.Time {
	var earliest *time.Time
	for _, value := range values {
		if value == nil || value.IsZero() {
			continue
		}
		if earliest == nil || value.Before(*earliest) {
			copy := value.UTC()
			earliest = &copy
		}
	}
	return earliest
}
