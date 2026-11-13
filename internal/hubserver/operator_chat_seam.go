package hubserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
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
	var credential hubapi.APICredential
	var err error
	if origin != "" {
		if identity.SessionID != "" {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		credential, err = s.currentEntryOriginCredential(ctx, origin)
	} else if identity.SessionID == "" {
		if s.config.TenantRepository != nil {
			credential, _, err = s.authenticateTenantAPIHash(ctx, tracker.OrganizationID(identity.OrganizationID), identity.CredentialID, "", "")
		} else {
			credential, _, err = s.authenticateAPIHash(ctx, identity.CredentialID, "", "")
		}
	} else if s.config.Hosted != nil && strings.HasPrefix(identity.PrincipalID, "account:") {
		credential, err = s.hostedAccountCredential(ctx, identity.SessionID)
	} else if s.config.Hosted != nil {
		session, sessionErr := s.WebSession(ctx, identity.SessionID, s.config.now())
		if sessionErr != nil {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		credential, _, err = s.hostedSessionCredential(ctx, session, identity.SessionID)
	} else {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	if err != nil || hubscope.OperatorIdentity(credential, identity.OrganizationID) != identity {
		return operatortool.Authority{}, operatortool.ErrAccessDenied
	}
	return s.operatorCurrentAuthority(ctx, credential, identity.OrganizationID)
}

func (s *Service) currentEntryOriginClaims(ctx context.Context, encoded string) (cloudassert.Claims, error) {
	var origin entryauthority.Origin
	if !s.hostedShared() || s.config.Hosted.SharedEntry.ResolveKey == nil || json.Unmarshal([]byte(encoded), &origin) != nil || origin.Issuer != s.config.Hosted.SharedEntry.Issuer || origin.Generation != s.config.Hosted.SharedEntry.Generation || len(origin.CredentialBinding) != 64 || origin.KeyID == "" || origin.Subject == "" {
		return cloudassert.Claims{}, operatortool.ErrAccessDenied
	}
	claims, err := s.config.Hosted.SharedEntry.ResolveKey(ctx, origin)
	if err != nil {
		return cloudassert.Claims{}, hubscope.OperatorAuthorizationError(err)
	}
	if claims.Key == nil || claims.Key.ID != origin.KeyID || claims.Key.CredentialBinding != origin.CredentialBinding || claims.Subject != origin.Subject || claims.Issuer != origin.Issuer || claims.Generation != origin.Generation {
		return cloudassert.Claims{}, operatortool.ErrAccessDenied
	}
	return claims, nil
}

func (s *Service) currentEntryOriginCredential(ctx context.Context, encoded string) (hubapi.APICredential, error) {
	claims, err := s.currentEntryOriginClaims(ctx, encoded)
	if err != nil {
		return hubapi.APICredential{}, err
	}
	credential, _, err := s.entryKeyCredential(ctx, claims)
	return credential, err
}
