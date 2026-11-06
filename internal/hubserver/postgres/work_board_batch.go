package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) ReadBoardRequest(ctx context.Context, organization tracker.OrganizationID, credentialID string, run func(context.Context) error) (resultErr error) {
	if request, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		if request.repository != r || request.session.organization != organization {
			return corestore.ErrTenantMismatch
		}
		return run(ctx)
	}
	release, err := r.acquire(ctx, corestore.Serving)
	if err != nil {
		return err
	}
	defer release()
	s := &session{organization: organization, readOnly: true, projectGrants: map[projectAccessKey]accessstore.ProjectGrant{}, tokenGrants: map[projectAccessKey]accessstore.TokenGrant{}, projectRows: map[tracker.ProjectID][]pgquery.BoundProjectRecordRow{}, credentials: map[string]accessstore.Credential{}, members: map[string]accessstore.Member{}}
	batch := &pgx.Batch{}
	credential := accessstore.Credential{}
	batch.Queue(`SELECT organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind FROM api_tokens WHERE organization_id=$1 AND id=$2`, organization, credentialID).QueryRow(func(row pgx.Row) error {
		return row.Scan(&credential.OrganizationID, &credential.ID, &credential.Name, &credential.Hash, &credential.Fingerprint, &credential.Scope, &credential.NativeOnly, &credential.CreatedAt, &credential.UpdatedAt, &credential.RotatedAt, &credential.RevokedAt, &credential.ExpiresAt, &credential.LastUsedAt, &credential.UserID, &credential.MembershipID, &credential.KeyScope, &credential.ProjectAccess, &credential.Kind)
	})
	org := accessstore.Organization{}
	batch.Queue(`SELECT organization_id,name,slug,local,created_at FROM organizations WHERE organization_id=$1`, organization).QueryRow(func(row pgx.Row) error {
		return row.Scan(&org.OrganizationID, &org.Name, &org.Slug, &org.Local, &org.CreatedAt)
	})
	tx, results, err := r.runtime.pools.BeginBatch(ctx, pgtransport.Scope{Organization: organization}, pgtransport.Options{Isolation: pgx.RepeatableRead, ReadOnly: true}, batch)
	if err != nil {
		return mapError(ctx, err)
	}
	defer func() { resultErr = errors.Join(resultErr, mapError(ctx, tx.Rollback(ctx))) }()
	started := time.Now()
	err = results.Close()
	for _, query := range batch.QueuedQueries {
		observeStatement(ctx, started, query.SQL)
	}
	if err != nil {
		return mapError(ctx, err)
	}
	s.credentials[credential.ID] = credential
	s.organizationRecord = &org
	s.db = timedQueries{DBTX: tx}
	s.queries = pgquery.New(s.db)
	if err := run(context.WithValue(ctx, readRequestKey{}, &readRequest{repository: r, session: s})); err != nil {
		return err
	}
	started = time.Now()
	err = tx.Commit(ctx)
	observeStatement(ctx, started, "COMMIT")
	return mapError(ctx, err)
}
