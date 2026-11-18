package operatorchat

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Identity struct {
	operatortool.Identity
	Origin string `json:"origin,omitempty"`
}

type OrganizationKey struct{}

type Store struct{ Repository tenantstore.Repository }

func (store Store) Load(ctx context.Context, id string, now time.Time, ttl time.Duration) (chat.SessionState, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	organization := tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID)
	if organization == "" {
		if scoped, ok := ctx.Value(OrganizationKey{}).(tracker.OrganizationID); ok {
			organization = scoped
		}
	}
	if organization == "" || store.Repository == nil {
		return chat.SessionState{}, false, operatortool.ErrAccessDenied
	}
	var row accessstore.OperatorSession
	err := store.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
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

func (store Store) Save(ctx context.Context, state chat.SessionState, _ time.Time, _ time.Duration, limit int) error {
	if !state.Identity.Valid() || store.Repository == nil {
		return operatortool.ErrAccessDenied
	}
	row := accessstore.OperatorSession{OrganizationID: tracker.OrganizationID(state.Identity.OrganizationID), ID: state.ID, PrincipalID: state.Identity.PrincipalID, CredentialID: state.Identity.CredentialID, SessionID: state.Identity.SessionID, Client: state.Client, LastUsedAt: state.LastUsedAt}
	if state.Origin != "" {
		if err := json.Unmarshal([]byte(state.Origin), &row.Origin); err != nil || row.Origin.KeyID == "" {
			return operatortool.ErrAccessDenied
		}
	}
	err := store.Repository.WriteTenant(ctx, row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { return w.SaveOperatorSession(ctx, row, limit) })
	if errors.Is(err, accessstore.ErrDenied) {
		return operatortool.ErrAccessDenied
	}
	return err
}
