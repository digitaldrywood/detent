package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/projectreads"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type readRequestKey struct{}
type readRequest struct {
	repository *Repository
	session    *session
}

func (r *Repository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, run func(context.Context) error) error {
	if request, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		if request.repository != r || request.session.organization != organization {
			return corestore.ErrTenantMismatch
		}
		return run(ctx)
	}
	return r.transactionIsolation(ctx, organization, corestore.Serving, true, pgx.RepeatableRead, func(s *session) error {
		s.projectGrants = make(map[projectAccessKey]accessstore.ProjectGrant)
		s.tokenGrants = make(map[projectAccessKey]accessstore.TokenGrant)
		s.projectReads.BeginSnapshot()
		s.credentials = make(map[string]accessstore.Credential)
		s.members = make(map[string]accessstore.Member)
		return run(context.WithValue(ctx, readRequestKey{}, &readRequest{repository: r, session: s}))
	})
}

func (r *Repository) WithProjectWorkflows(ctx context.Context) context.Context {
	if r == nil {
		return ctx
	}
	return projectreads.WithScope(ctx, r.pool)
}
