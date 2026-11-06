package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func (s *session) AccessAccountAudit(ctx context.Context, actor, retry string) (accessstore.Audit, error) {
	id, err := s.queries.AccessAccountAuditID(ctx, pgquery.AccessAccountAuditIDParams{OrganizationID: s.organization, ActualActor: actor, AccountRetry: retry})
	if err != nil {
		return accessstore.Audit{}, mapError(ctx, err)
	}
	return s.AccessAudit(ctx, id)
}

func (s *session) BindAccessTenantProvider(ctx context.Context, provider string) error {
	if provider == "" {
		return accessstore.ErrDenied
	}
	rows, err := s.queries.BindAccessTenantProvider(ctx, pgquery.BindAccessTenantProviderParams{OrganizationID: s.organization, ProviderID: provider})
	if err != nil {
		return mapError(ctx, err)
	}
	if rows != 1 {
		return mutation.ErrConflict
	}
	return nil
}

func (s *session) AccessMissingUsageGrant(ctx context.Context, user string) (bool, error) {
	missing, err := s.queries.AccessMissingUsageGrant(ctx, pgquery.AccessMissingUsageGrantParams{OrganizationID: s.organization, UserID: user})
	return missing, mapError(ctx, err)
}
