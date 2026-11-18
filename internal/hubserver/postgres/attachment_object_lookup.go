package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/rowlookup"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) lookups() rowlookup.Reader {
	return rowlookup.Reader{Organization: s.organization, ForShare: !s.readOnly, Scan: func(ctx context.Context, query string, args []any, dest ...any) error {
		return s.db.QueryRow(ctx, query, args...).Scan(dest...)
	}}
}

var _ tenantstore.LookupRepository = (*Repository)(nil)

type rowLookups interface {
	AccessOrganization(context.Context) (accessstore.Organization, error)
	AttachmentObjectExists(context.Context, string, string) (bool, error)
}

func (r *Repository) LookupOrganization(ctx context.Context, org tracker.OrganizationID) (accessstore.Organization, error) {
	return readLookup(ctx, r, org, func(reader rowLookups) (accessstore.Organization, error) { return reader.AccessOrganization(ctx) })
}

func (r *Repository) LookupAttachment(ctx context.Context, org tracker.OrganizationID, id, key string) (bool, error) {
	return readLookup(ctx, r, org, func(reader rowLookups) (bool, error) { return reader.AttachmentObjectExists(ctx, id, key) })
}

func readLookup[T any](ctx context.Context, r *Repository, org tracker.OrganizationID, read func(rowLookups) (T, error)) (T, error) {
	var zero T
	if request, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		if request.repository != r || request.session.organization != org {
			return zero, corestore.ErrTenantMismatch
		}
		return read(request.session)
	}
	finish := transactionSpan(ctx, "pg_admission")
	release, err := r.acquire(ctx, corestore.Serving)
	finish()
	if err != nil {
		return zero, err
	}
	defer release()
	return read(rowlookup.Reader{Organization: org, Scan: func(ctx context.Context, query string, args []any, dest ...any) error {
		started := time.Now()
		err := r.runtime.pools.ReadRow(ctx, pgtransport.Scope{Organization: org}, query, args, dest...)
		observeStatement(ctx, started, query)
		return err
	}})
}

func (s *session) AttachmentObjectExists(ctx context.Context, id, key string) (bool, error) {
	return s.lookups().AttachmentObjectExists(ctx, id, key)
}
