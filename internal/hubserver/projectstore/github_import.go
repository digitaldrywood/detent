package projectstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type GitHubImport struct {
	OrganizationID  tracker.OrganizationID
	ProjectID       tracker.ProjectID
	ID              string
	IssueNumber     int64
	WorkItemID      string
	Stage           string
	Cursor          string
	Revision        int64
	Pages           int64
	Status          string
	Gaps            []string
	LastError       string
	RetryAfter      *time.Time
	SourceUpdatedAt *time.Time
	ObservedAt      time.Time
	IntakePending   bool
	EditSequence    int64
}

type GitHubImportRecord struct {
	OrganizationID       tracker.OrganizationID
	ProjectID            tracker.ProjectID
	ImportID             string
	Sequence             int64
	SourceKey            string
	Kind                 string
	CurrentDependency    bool
	DependencyID         string
	ProvenanceExternalID string
	Payload              eventstore.Receipt
}

type GitHubImportReader interface {
	GitHubImportForIssue(context.Context, tracker.ProjectID, int64) (GitHubImport, error)
	GitHubImport(context.Context, tracker.ProjectID, string) (GitHubImport, error)
}

type GitHubImportWriter interface {
	GitHubImportReader
	CreateGitHubImport(context.Context, GitHubImport) (GitHubImport, error)
	UpdateGitHubImport(context.Context, GitHubImport) (GitHubImport, error)
}

type GitHubImportRecordReader interface {
	GitHubImportRecords(context.Context, tracker.ProjectID, string, int64, int) ([]GitHubImportRecord, error)
	GitHubImportEditSource(context.Context, tracker.ProjectID, string, int64) (GitHubImportRecord, error)
}

type GitHubImportRecordWriter interface {
	GitHubImportRecordReader
	CreateGitHubImportRecord(context.Context, GitHubImportRecord) error
}
