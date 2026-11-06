package postgres

import (
	"context"
	"database/sql"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type readRequestKey struct{}
type readRequest struct {
	repository *Repository
	session    *session
	mu         sync.Mutex
}

func (r *Repository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, run func(context.Context) error) error {
	if request, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		if request.repository != r || request.session.organization != organization {
			return corestore.ErrTenantMismatch
		}
		return run(ctx)
	}
	return r.transactionIsolation(ctx, organization, corestore.Serving, true, sql.LevelRepeatableRead, func(s *session) error {
		s.projectGrants = make(map[projectAccessKey]accessstore.ProjectGrant)
		s.tokenGrants = make(map[projectAccessKey]accessstore.TokenGrant)
		s.projectRows = make(map[tracker.ProjectID][]pgquery.BoundProjectRecordRow)
		s.credentials = make(map[string]accessstore.Credential)
		s.members = make(map[string]accessstore.Member)
		return run(context.WithValue(ctx, readRequestKey{}, &readRequest{repository: r, session: s}))
	})
}
