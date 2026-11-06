package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) AccessCredentialHasMember(ctx context.Context, id string) (bool, error) {
	member, err := s.queries.AccessCredentialHasMember(ctx, pgquery.AccessCredentialHasMemberParams{OrganizationID: s.organization, PrincipalID: id})
	return member, mapError(ctx, err)
}
