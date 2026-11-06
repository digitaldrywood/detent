package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type operatorChatIdentity struct {
	operatortool.Identity
	Origin string `json:"origin,omitempty"`
}

type operatorChatOrganizationKey struct{}

type operatorChatStore struct{ database *database }

func (store operatorChatStore) Load(ctx context.Context, id string, now time.Time, ttl time.Duration) (chat.SessionState, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	organization := tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID)
	if organization == "" {
		if scoped, ok := ctx.Value(operatorChatOrganizationKey{}).(tracker.OrganizationID); ok {
			organization = scoped
		}
	}
	if organization == "" || store.database.postgres == nil {
		return chat.SessionState{}, false, operatortool.ErrAccessDenied
	}
	var row accessstore.OperatorSession
	err := store.database.postgres.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.OperatorSession(ctx, id, now.Add(-ttl))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return chat.SessionState{}, false, nil
	}
	if err != nil {
		return chat.SessionState{}, false, err
	}
	state := chat.SessionState{ID: id, Identity: operatortool.Identity{OrganizationID: string(organization), PrincipalID: row.PrincipalID, CredentialID: row.CredentialID, SessionID: row.SessionID}, Client: row.Client, LastUsedAt: row.LastUsedAt}
	if row.Origin.KeyID != "" {
		raw, err := json.Marshal(row.Origin)
		if err != nil {
			return state, false, err
		}
		state.Origin = string(raw)
	}
	return state, true, nil
}

func (store operatorChatStore) Save(ctx context.Context, state chat.SessionState, _ time.Time, _ time.Duration, limit int) error {
	if !state.Identity.Valid() || store.database.postgres == nil {
		return operatortool.ErrAccessDenied
	}
	row := accessstore.OperatorSession{OrganizationID: tracker.OrganizationID(state.Identity.OrganizationID), ID: state.ID, PrincipalID: state.Identity.PrincipalID, CredentialID: state.Identity.CredentialID, SessionID: state.Identity.SessionID, Client: state.Client, LastUsedAt: state.LastUsedAt}
	if state.Origin != "" {
		if err := json.Unmarshal([]byte(state.Origin), &row.Origin); err != nil || row.Origin.KeyID == "" {
			return operatortool.ErrAccessDenied
		}
	}
	err := store.database.postgres.WriteTenant(ctx, row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { return w.SaveOperatorSession(ctx, row, limit) })
	if errors.Is(err, accessstore.ErrDenied) {
		return operatortool.ErrAccessDenied
	}
	return err
}

func (s *Service) resolveOperatorChatAuthority(ctx context.Context, identity operatortool.Identity, origin string) (operatortool.Authority, error) {
	var credential apiCredential
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
	if err != nil || operatorIdentity(credential, identity.OrganizationID) != identity {
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
	if err != nil || claims.Key == nil || claims.Key.ID != origin.KeyID || claims.Key.CredentialBinding != origin.CredentialBinding || claims.Subject != origin.Subject || claims.Issuer != origin.Issuer || claims.Generation != origin.Generation {
		return cloudassert.Claims{}, operatortool.ErrAccessDenied
	}
	return claims, nil
}

func (s *Service) currentEntryOriginCredential(ctx context.Context, encoded string) (apiCredential, error) {
	claims, err := s.currentEntryOriginClaims(ctx, encoded)
	if err != nil {
		return apiCredential{}, err
	}
	credential, _, err := s.entryKeyCredential(ctx, claims)
	return credential, err
}
