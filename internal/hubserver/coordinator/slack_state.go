package coordinator

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SlackReader interface {
	SlackIntegration(context.Context) (postgres.SlackIntegration, error)
	AccessSecret(context.Context, tracker.ProjectID, string) (accessstore.Secret, error)
}

type SlackState struct {
	Integration       postgres.SlackIntegration
	WebhookConfigured bool
}

func ReadSlackState(ctx context.Context, r SlackReader, organization tracker.OrganizationID, webhookKind string) (SlackState, error) {
	var state SlackState
	row, err := r.SlackIntegration(ctx)
	if err == nil {
		if row.OrganizationID != organization {
			return state, corestore.ErrTenantMismatch
		}
		state.Integration = row
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return state, err
	}
	secret, err := r.AccessSecret(ctx, "", webhookKind)
	if err == nil {
		if secret.OrganizationID != organization || secret.ProjectID != "" || secret.Kind != webhookKind {
			return state, corestore.ErrTenantMismatch
		}
		state.WebhookConfigured = true
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return state, err
	}
	return state, nil
}
