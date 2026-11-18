package githubcontract

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
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
	Profile              string
	SkipIssues           bool
	SkipRepository       bool
	Repository           RepositoryTarget
	Mode                 ReconcileMode
	Since                *time.Time
	Through              time.Time
	TargetedPullRequests bool
	PullRequestPage      int
	Hydrations           []HydrationRequest
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
	DefaultBranch string
	NodeID        string
	DatabaseID    *int64
	Owner         string
	Name          string
	UpdatedAt     time.Time
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
	PullRequestsMore   bool
}

type ReconcileBackend interface {
	Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error)
}
