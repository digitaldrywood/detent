package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	checkpointWebhook     = "webhook"
	checkpointIncremental = "incremental"
	checkpointFullRepair  = "full_repair"
)

type ReconcileMode string

const (
	ReconcileIncremental ReconcileMode = "incremental"
	ReconcileFullRepair  ReconcileMode = "full_repair"
)

type RepositoryTarget struct {
	ID         int64
	NodeID     string
	DatabaseID *int64
	Owner      string
	Name       string
}

type ReconcileRequest struct {
	Profile        string
	SkipIssues     bool
	SkipRepository bool
	Repository     RepositoryTarget
	Mode           ReconcileMode
	Since          *time.Time
	Through        time.Time
	Hydrations     []HydrationRequest
}

type HydrationRequest struct {
	ID           int64
	ObjectKind   string
	ObjectKey    string
	GitHubNodeID string
	GitHubNumber int
	HeadSHA      string
	RequestCount int
}

type RepositorySource struct {
	NodeID     string
	DatabaseID *int64
	Owner      string
	Name       string
	UpdatedAt  time.Time
}

type IssueSource struct {
	NodeID     string
	DatabaseID *int64
	Number     int
	Title      string
	Body       string
	URL        string
	State      string
	AuthorID   string
	Labels     []string
	Assignees  []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type PullRequestSource struct {
	MergeSHA   string
	MergedAt   time.Time
	MergedBy   string
	NodeID     string
	DatabaseID *int64
	Number     int
	Title      string
	URL        string
	State      string
	Draft      bool
	HeadRef    string
	HeadSHA    string
	BaseRef    string
	BaseSHA    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type PullRequestDetailSource struct {
	Number         int
	HeadSHA        string
	MergeableState *string
	Checks         *tracker.CheckSummary
	Reviews        *tracker.ReviewSummary
}

type ReconcileSnapshot struct {
	Repository         RepositorySource
	Issues             []IssueSource
	PullRequests       []PullRequestSource
	PullRequestDetails []PullRequestDetailSource
}

type ReconcileBackend interface {
	Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error)
}

type reconcileTarget struct {
	RepositoryTarget
	Profile           string
	RepositoryEnabled bool
	Cursor            *time.Time
	HydrationSince    *time.Time
	Hydrations        []HydrationRequest
}

func (s *Service) runGitHubReconciliation(ctx context.Context) {
	close(s.reconcileDone)
}

func (s *Service) reconcileAllRepositories(ctx context.Context, mode ReconcileMode) {

}

func (s *Service) reconcileRepository(ctx context.Context, target reconcileTarget, mode ReconcileMode) error {
	return ErrPostgresFeatureUnavailable
}

func (s *Service) stopGitHubReconciliation() error {
	if s == nil || s.reconcileCancel == nil || s.reconcileDone == nil {
		return nil
	}
	s.reconcileStopOnce.Do(func() {
		s.reconcileCancel()
		<-s.reconcileDone
	})
	return nil
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
