package githubcontract

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ScheduledClassificationStage = "scheduled-classification"

type GitHubImportRequest struct {
	CompareBase       string
	CompareHead       string
	Discovery         *tracker.GitHubDiscovery
	Scheduled         *gate.ScheduledEvidence
	ScheduledWorkflow string
	ScheduledBranch   string
	SourceID          string
	Repository        string
	Profile           string
	IssueNumber       int
	Stage             string
	Cursor            string
}

type GitHubImportRecord struct {
	CurrentDependency *bool              `json:"current_dependency,omitempty"`
	SourceKey         string             `json:"source_key"`
	Kind              string             `json:"kind"`
	Data              json.RawMessage    `json:"data"`
	Provenance        tracker.Provenance `json:"provenance"`
	Body              string             `json:"body,omitempty"`
	DependencyID      string             `json:"dependency_id,omitempty"`
}

type GitHubImportPage struct {
	Commits       []string
	BatchPage     *tracker.GitHubDiscoveryPage
	Scheduled     *gate.ScheduledEvidence
	RetryAt       time.Time
	EditSequence  int64
	EditsFinished bool
	Issue         *IssueSource
	Records       []GitHubImportRecord
	NextCursor    string
	Gaps          []string
}

type ImportBackend interface {
	FetchImportPage(context.Context, GitHubImportRequest) (GitHubImportPage, error)
}
