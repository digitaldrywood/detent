package accessinvites

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessmembers"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository   tenantstore.Repository
	Organization tracker.OrganizationID
	Now          func() time.Time
	Provider     auth.HostedProvider
	Auth         *hubauth.Authenticator
	Members      *accessmembers.Service
	StaffEmails  []string
	Classify     func(error) error
}
type Service struct{ deps Deps }

func New(deps Deps) *Service {
	if deps.Classify == nil {
		deps.Classify = hubauth.Classify
	}
	return &Service{deps: deps}
}
func (s *Service) read(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, read)
}
func (s *Service) write(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.WriteTenant(ctx, organization, corestore.Serving, write)
}
