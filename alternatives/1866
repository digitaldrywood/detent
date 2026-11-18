package dailysummary

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reports interface {
	ValidateUsage([]reportstore.UsageFact) error
}

type WorkItems interface {
	AttemptSnapshot(context.Context, tenantstore.Reader, leasestore.Attempt, leasestore.Lease, eventstore.Event) (any, error)
	Attempt(context.Context, any, time.Time) (tracker.NativeAttempt, error)
	Issue(context.Context, corestore.WorkItem, eventstore.CollaborationVersion) (tracker.NativeIssue, error)
}

type Deps struct {
	Repository       tenantstore.Repository
	AccessRepository tenantstore.Repository
	Now              func() time.Time
	Organization     func() tracker.OrganizationID
	EmailSender      auth.EmailSender
	PublicURL        string
	HostedPath       func(string) string
	IssuePath        func(organization, project string, number int) string
	RenderText       func(string) string
	Reports          Reports
	WorkItems        WorkItems
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

type usageWindow struct{ From, To time.Time }

type shippedIssue struct {
	ID       string
	ClosedAt time.Time
}
