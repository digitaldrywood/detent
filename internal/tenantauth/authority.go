package tenantauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	ReadTenantAuthority(context.Context, RequestIdentity) (Scope, error)
}

type Authority struct {
	pools *pgtransport.Pools
	now   func() time.Time
}

func New(pools *pgtransport.Pools, now func() time.Time) *Authority {
	if now == nil {
		now = time.Now
	}
	return &Authority{pools: pools, now: now}
}

func (a *Authority) Resolve(ctx context.Context, input RequestIdentity) (Scope, error) {
	return a.ReadTenantAuthority(ctx, input)
}

func (a *Authority) ReadTenantAuthority(ctx context.Context, input RequestIdentity) (result Scope, resultErr error) {
	if input.Organization == "" || input.CredentialHash == "" {
		return Scope{}, ErrDenied
	}
	if a.pools == nil {
		return Scope{}, ErrUnavailable
	}
	batch := &pgx.Batch{}
	batch.Queue(authoritySQL, string(input.Organization), input.CredentialHash, runnerstore.MaxRunnerProjectList+1)
	tx, results, err := a.pools.BeginBatch(ctx, pgtransport.Scope{Organization: input.Organization}, pgtransport.Options{ReadOnly: true}, batch)
	if err != nil {
		return Scope{}, unavailable(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			result = Scope{}
			resultErr = errors.Join(resultErr, unavailable(err))
		}
	}()
	scope, readErr := scanAuthority(results.QueryRow(), input, a.now())
	closeErr := results.Close()
	if closeErr != nil {
		closeErr = unavailable(closeErr)
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return Scope{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Scope{}, unavailable(err)
	}
	return scope, nil
}

func unavailable(err error) error {
	return fmt.Errorf("read tenant authority: %w", errors.Join(ErrUnavailable, err))
}

func scanAuthority(row pgx.Row, input RequestIdentity, now time.Time) (Scope, error) {
	scope := Scope{organization: input.Organization, proof: input}
	p := &scope.principal
	var revoked, runnerRemoved *time.Time
	var active, principalValid, tenantFound bool
	var runnerExpires *time.Time
	var projects []string
	err := row.Scan(&p.CredentialID, &p.Name, &p.Scope, &p.NativeOnly, &scope.created, &revoked, &scope.expires,
		&p.UserID, &p.MembershipID, &p.KeyScope, &p.ProjectAccess, &p.Kind,
		&p.PrincipalID, &p.Role, &active, &principalValid, &tenantFound, &p.ProviderID, &scope.generation,
		&scope.runner.RunnerID, &scope.runner.MachineID, &scope.runner.Scope, &scope.runner.Operations, &runnerExpires, &runnerRemoved, &projects)
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, ErrDenied
	}
	if err != nil {
		return Scope{}, unavailable(err)
	}
	if revoked != nil || p.KeyScope != "" && !apikey.ValidScope(apikey.Scope(p.KeyScope)) {
		return Scope{}, ErrDenied
	}
	if scope.RunnerID() != "" {
		if p.Scope != "worker" || p.Kind != "service" || runnerRemoved != nil || runnerExpires == nil || !scope.runner.Valid() || !runnerauth.ValidOperations(scope.runner.Operations) {
			return Scope{}, ErrDenied
		}
		if len(projects) > runnerstore.MaxRunnerProjectList {
			return Scope{}, runnerstore.ErrLimitExceeded
		}
		scope.runner.OrganizationID = input.Organization
		scope.runner.ExpiresAt = *runnerExpires
		for _, project := range projects {
			scope.runner.ProjectIDs = append(scope.runner.ProjectIDs, tracker.ProjectID(project))
		}
		scope.renewal = input.RenewalRunner == scope.RunnerID() && input.RenewalOrganization == input.Organization
	} else if !tenantFound || p.UserID != "" && (!active || !principalValid) {
		return Scope{}, ErrDenied
	}
	if err := scope.ValidAt(now); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

const authoritySQL = `SELECT t.id,t.name,t.scope,t.native_only,t.created_at,t.revoked_at,t.expires_at,
t.hosted_user_id,t.hosted_membership_id,t.operator_key_scope,t.operator_project_access,t.key_kind,
COALESCE(m.principal_id,''),COALESCE(m.role,''),COALESCE(m.active AND m.membership_id=t.hosted_membership_id,false),
COALESCE(pr.id IS NOT NULL AND pr.revoked_at IS NULL,false),h.organization_id IS NOT NULL,COALESCE(h.provider_id,''),COALESCE(h.allocation_generation,0),
COALESCE(r.runner_id,''),COALESCE(r.machine_id,''),COALESCE(r.scope,''),COALESCE(r.operations,ARRAY[]::text[]),r.expires_at,r.removed_at,
ARRAY(SELECT p.id FROM public.projects p WHERE p.organization_id=t.organization_id AND p.deleted_at IS NULL AND r.runner_id IS NOT NULL
AND (r.scope='organization' OR EXISTS(SELECT 1 FROM public.token_grants g WHERE g.organization_id=t.organization_id AND g.token_id=t.id AND g.project_id=p.id)) ORDER BY p.id LIMIT $3)
FROM public.api_tokens t
LEFT JOIN public.hosted_members m ON m.organization_id=t.organization_id AND m.user_id=t.hosted_user_id
LEFT JOIN public.api_tokens pr ON pr.organization_id=m.organization_id AND pr.id=m.principal_id
LEFT JOIN public.hosted_tenant h ON h.organization_id=t.organization_id
LEFT JOIN public.runner_identities r ON r.organization_id=t.organization_id AND r.token_id=t.id
WHERE t.organization_id=$1 AND t.token_hash=$2`
