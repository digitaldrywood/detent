package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) ReadRunnerEventCommand(ctx context.Context, org tracker.OrganizationID, credentialID, runnerID string, command eventstore.RunCommand, read func(context.Context, tenantstore.Reader) error) (resultErr error) {
	return r.runnerEventCommand(ctx, org, credentialID, runnerID, command, true, read)
}

func (r *Repository) WriteRunnerEventCommand(ctx context.Context, org tracker.OrganizationID, credentialID, runnerID string, command eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
	return r.runnerEventCommand(ctx, org, credentialID, runnerID, command, false, func(ctx context.Context, reader tenantstore.Reader) error {
		f, ok := reader.(*runnerEventSession)
		if !ok {
			return corestore.ErrInvalidPage
		}
		if err := write(ctx, f); err != nil {
			return err
		}
		return f.flushRunEvents(ctx)
	})
}

func (r *Repository) runnerEventCommand(ctx context.Context, org tracker.OrganizationID, credentialID, runnerID string, command eventstore.RunCommand, readOnly bool, use func(context.Context, tenantstore.Reader) error) (resultErr error) {
	if _, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		return corestore.ErrTenantMismatch
	}
	release, err := r.acquire(ctx, corestore.Serving)
	if err != nil {
		return err
	}
	defer release()
	s := &session{organization: org, readOnly: readOnly, leaseClock: r.leaseClock, credentials: map[string]accessstore.Credential{}, tokenGrants: map[projectAccessKey]accessstore.TokenGrant{}}
	f, facts := newRunnerEventFacts(s, s, command)
	b := new(pgx.Batch)
	lock := ""
	if !readOnly {
		lock = " FOR UPDATE"
	}
	var identity runnerstore.Identity
	b.Queue(runnerIdentitySelect+`token_id=$2`+lock, org, credentialID).QueryRow(func(row pgx.Row) error { var err error; identity, err = scanRunnerIdentity(ctx, row); return err })
	var credential accessstore.Credential
	b.Queue(`SELECT organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind FROM api_tokens WHERE organization_id=$1 AND id=$2`+lock, org, credentialID).QueryRow(func(row pgx.Row) error {
		err := row.Scan(&credential.OrganizationID, &credential.ID, &credential.Name, &credential.Hash, &credential.Fingerprint, &credential.Scope, &credential.NativeOnly, &credential.CreatedAt, &credential.UpdatedAt, &credential.RotatedAt, &credential.RevokedAt, &credential.ExpiresAt, &credential.LastUsedAt, &credential.UserID, &credential.MembershipID, &credential.KeyScope, &credential.ProjectAccess, &credential.Kind)
		if err == nil {
			s.credentials[credentialID] = credential
		}
		return err
	})
	b.Queue(`SELECT organization_id,token_id,project_id FROM token_grants WHERE organization_id=$1 AND token_id=$2 AND project_id=$3`+lock, org, credentialID, command.Project).QueryRow(func(row pgx.Row) error {
		var grant accessstore.TokenGrant
		err := row.Scan(&grant.OrganizationID, &grant.TokenID, &grant.ProjectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			s.tokenGrants[projectAccessKey{principal: credentialID, project: command.Project}] = grant
		}
		return err
	})
	b.QueuedQueries = append(b.QueuedQueries, facts.QueuedQueries...)
	started := time.Now()
	tx, results, err := r.runtime.pools.BeginBatch(ctx, pgtransport.Scope{Organization: org}, runnerEventOptions(readOnly), b)
	if err != nil {
		return mapError(ctx, err)
	}
	defer func() { resultErr = errors.Join(resultErr, mapError(ctx, tx.Rollback(ctx))) }()
	batchErr := results.Close()
	for _, q := range b.QueuedQueries {
		observeStatement(ctx, started, q.SQL)
	}
	if err := batchErr; err != nil {
		return mapError(ctx, err)
	}
	if identity.RunnerID != runnerID {
		return accessstore.ErrDenied
	}
	s.db = timedQueries{DBTX: tx}
	s.queries = pgquery.New(s.db)
	ctx = context.WithValue(ctx, runnerHeartbeatIdentityKey{}, runnerHeartbeatIdentity{session: s, identity: &identity})
	f.command.WorkItem = f.item.value.NativeID
	if f.command.AttemptID == "" {
		f.command.AttemptID = f.attempt.value.ID
	}
	if err := use(ctx, f); err != nil {
		return err
	}
	return mapError(ctx, tx.Commit(ctx))
}

func runnerEventOptions(readOnly bool) pgtransport.Options {
	if readOnly {
		return pgtransport.Options{ReadOnly: true, Isolation: pgx.RepeatableRead}
	}
	return pgtransport.Options{}
}

func (r *Repository) ReadRunnerAuthority(ctx context.Context, input tenantauth.RequestIdentity, now func() time.Time) (tenantauth.Scope, int, error) {
	var capacity int
	batch := new(pgx.Batch)
	batch.Queue(`SELECT coalesce((SELECT least(r.capacity_limit,m.capacity) FROM runner_identities r JOIN api_tokens t ON t.organization_id=r.organization_id AND t.id=r.token_id JOIN machines m ON m.organization_id=r.organization_id AND m.id=r.machine_id WHERE r.organization_id=$1 AND t.token_hash=$2),0)`, input.Organization, input.CredentialHash).QueryRow(func(row pgx.Row) error { return row.Scan(&capacity) })
	scope, err := tenantauth.New(r.runtime.pools, now).ReadTenantAuthorityBatch(ctx, input, batch)
	return scope, capacity, err
}
