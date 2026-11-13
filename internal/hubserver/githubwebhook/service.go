package githubwebhook

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

const maxBodyBytes = 2 << 20

type Integration struct {
	Scope      hubapi.Scope
	Repository string
	Import     func(context.Context, string, string, tracker.GitHubIssuePreview, tracker.GitHubIssueSnapshot) error
	Apply      func(context.Context, string, tracker.NativeWorkItemID, string, tracker.GitHubIssueSnapshot) error
}

type Deps struct {
	Repository       tenantstore.Repository
	Organization     tracker.OrganizationID
	Details          attachment.DetailStore
	Now              func() time.Time
	Jitter           func() float64
	Logger           *slog.Logger
	Secret           []byte
	Retention        time.Duration
	Unavailable      error
	DeliveryConflict error
	Context          func(context.Context) context.Context
	Error            func(echo.Context, error, ...any) error
	Integration      func(context.Context, tracker.OrganizationID, tracker.ProjectID) (Integration, error)
	ValidCommit      func(string) bool
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

type webhookResponse struct {
	DeliveryID string `json:"delivery_id"`
	Duplicate  bool   `json:"duplicate"`
}

type webhookErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func SnapshotFromSource(source githubcontract.IssueSource, now time.Time) tracker.GitHubIssueSnapshot {
	author := source.AuthorID
	if author == "" {
		author = "unavailable"
	}
	return tracker.GitHubIssueSnapshot{URL: source.URL, Title: source.Title, Body: source.Body,
		Provenance: tracker.Provenance{Provider: "github", ExternalID: source.NodeID, AuthorID: author, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt, ObservedAt: now}}
}
