package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) accessRead(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	if s.config.AccessRepository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.config.AccessRepository.ReadTenant(ctx, organization, corestore.Serving, read)
}

func (s *Service) accessWrite(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	if s.config.AccessRepository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.config.AccessRepository.WriteTenant(ctx, organization, corestore.Serving, write)
}

func (s *Service) accessOrganization() tracker.OrganizationID {
	if s.config.Hosted == nil {
		return tracker.OrganizationID(s.localOrganization)
	}
	return tracker.OrganizationID(s.config.Hosted.OrganizationID)
}

func (s *Service) accessAuthority(scope nativeScope, write bool) accessstore.Authority {
	return hubauth.Authority(s.authInput(scope), write)
}

func (s *Service) authorizeAccessScope(ctx context.Context, r accessstore.Reader, scope nativeScope, write bool) error {
	return hubapi.FromFeatureError(s.requestAuth().Authorize(ctx, r, s.authInput(scope), write))
}

func (s *Service) accessWebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.requestAuth().StoredSession(ctx, hash, now)
}

func (s *Service) authenticateAccessHash(ctx context.Context, hash, renewalRunner, renewalOrganization string) (hubapi.APICredential, int, error) {
	return nativeCredentialResult(s.requestAuth().AuthenticateAccessHash(ctx, hash, renewalRunner, renewalOrganization))
}

func (s *Service) activeAccessMember(ctx context.Context, user string) (accessstore.Member, error) {
	return s.requestAuth().ActiveMember(ctx, user)
}
