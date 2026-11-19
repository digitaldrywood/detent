package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorchat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type operatorChatOrganizationKey = operatorchat.OrganizationKey

type operatorChatStore struct{ database *database }

func (store operatorChatStore) feature() operatorchat.Store {
	if store.database.postgres == nil {
		return operatorchat.Store{}
	}
	return operatorchat.Store{Repository: store.database.postgres}
}

func (store operatorChatStore) Load(ctx context.Context, id string, now time.Time, ttl time.Duration) (chat.SessionState, bool, error) {
	return store.feature().Load(ctx, id, now, ttl)
}

func (store operatorChatStore) Save(ctx context.Context, state chat.SessionState, now time.Time, ttl time.Duration, limit int) error {
	return store.feature().Save(ctx, state, now, ttl, limit)
}

func (s *Service) resolveOperatorChatAuthority(ctx context.Context, identity operatortool.Identity, origin string) (operatortool.Authority, error) {
	deps := operatorchat.AuthorityDeps{
		Hosted: s.config.Hosted != nil, Identity: hubscope.OperatorIdentity, OriginCredential: s.currentEntryOriginCredential,
		AccountCredential: s.hostedAccountCredential, CurrentAuthority: s.operatorCurrentAuthority,
		TokenCredential: func(ctx context.Context, organization, hash string) (hubapi.APICredential, error) {
			if s.config.TenantRepository != nil {
				credential, _, err := s.authenticateTenantAPIHash(ctx, tracker.OrganizationID(organization), hash, "", "")
				return credential, err
			}
			credential, _, err := s.authenticateAPIHash(ctx, hash, "", "")
			return credential, err
		},
		SessionCredential: func(ctx context.Context, hash string) (hubapi.APICredential, error) {
			session, err := s.WebSession(ctx, hash, s.config.Clock())
			if err != nil {
				return hubapi.APICredential{}, err
			}
			credential, _, err := s.hostedSessionCredential(ctx, session, hash)
			return credential, err
		},
	}
	return deps.Resolve(ctx, identity, origin)
}

func (s *Service) currentEntryOriginClaims(ctx context.Context, encoded string) (cloudassert.Claims, error) {
	if !s.hostedShared() {
		return cloudassert.Claims{}, operatortool.ErrAccessDenied
	}
	entry := s.config.Hosted.SharedEntry
	origin := operatorchat.EntryOrigin{
		Issuer: entry.Issuer, Generation: entry.Generation, ResolveKey: entry.ResolveKey,
		ClassifyError: hubscope.OperatorAuthorizationError,
	}
	return origin.Claims(ctx, encoded)
}

func (s *Service) currentEntryOriginCredential(ctx context.Context, encoded string) (hubapi.APICredential, error) {
	claims, err := s.currentEntryOriginClaims(ctx, encoded)
	if err != nil {
		return hubapi.APICredential{}, err
	}
	credential, _, err := s.entryKeyCredential(ctx, claims)
	return credential, err
}
