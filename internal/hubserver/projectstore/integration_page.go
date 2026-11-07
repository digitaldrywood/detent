package projectstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Integration struct {
	Icon                      *projecticon.Icon     `json:"project_icon"`
	AgingEnabled              bool                  `json:"aging_enabled"`
	Name                      string                `json:"name"`
	DuplicateChallenge        bool                  `json:"duplicate_challenge"`
	ArchiveCompletedAfterDays *int                  `json:"archive_completed_after_days"`
	ArchiveCancelledAfterDays *int                  `json:"archive_cancelled_after_days"`
	WorkflowSource            string                `json:"workflow_source,omitempty"`
	WorkflowSourceRevision    string                `json:"workflow_source_revision,omitempty"`
	WorkflowMarkdown          string                `json:"workflow_markdown,omitempty"`
	States                    []tracker.NativeState `json:"states,omitempty"`
	Profile                   string                `json:"profile"`
	Revision                  tracker.Revision      `json:"revision,string"`
	Intake                    string                `json:"intake"`
	ManualImportEnabled       bool                  `json:"manual_import_enabled"`
	Projection                string                `json:"projection"`
	RepositoryEnabled         bool                  `json:"repository_enabled"`
	GitHubTransportAvailable  *bool                 `json:"github_transport_available,omitempty"`
	GitHubAppSlug             string                `json:"github_app_slug,omitempty"`
	GitHubAppInstallURL       string                `json:"github_app_install_url,omitempty"`
	GitHubAppInstalled        *bool                 `json:"github_app_installed,omitempty"`
	Repository                string                `json:"repository,omitempty"`
	CheckoutVerified          bool                  `json:"checkout_verified"`
	CheckoutRepository        string                `json:"checkout_repository,omitempty"`
	Authority                 map[string]string     `json:"authority"`
	RepositoryID              int64                 `json:"-"`
}

type IntegrationCandidate struct {
	ProjectID  tracker.ProjectID
	Generation int64
	SourceAt   time.Time
}

type IntegrationPageReader interface {
	IntegrationPage(context.Context, tracker.ProjectID) (Integration, error)
}

type IntegrationPageWriter interface {
	ClaimIntegrationPage(context.Context, time.Time) (IntegrationCandidate, error)
	PublishIntegrationPage(context.Context, IntegrationCandidate, *Integration, time.Time) error
}
