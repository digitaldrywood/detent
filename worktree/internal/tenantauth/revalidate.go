package tenantauth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
)

func (a *Authority) Revalidate(ctx context.Context, tx *pgtransport.Transaction, scope Scope) (Scope, error) {
	if tx == nil || tx.Scope().Organization != scope.Organization() || scope.ValidAt(a.now()) != nil {
		return Scope{}, ErrDenied
	}
	var organization string
	if err := tx.QueryRow(ctx, `SELECT organization_id FROM public.organizations WHERE organization_id=$1 FOR SHARE`, string(scope.Organization())).Scan(&organization); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Scope{}, ErrDenied
		}
		return Scope{}, unavailable(err)
	}
	current, err := a.ResolveIn(ctx, tx, scope.proof)
	if err != nil {
		return Scope{}, err
	}
	p := current.Principal()
	batch := &pgx.Batch{}
	batch.Queue(`SELECT id FROM public.api_tokens WHERE organization_id=$1 AND id=ANY($2::text[]) ORDER BY id FOR SHARE`, organization, []string{p.CredentialID, p.PrincipalID})
	batch.Queue(`SELECT user_id FROM public.hosted_members WHERE organization_id=$1 AND user_id=$2 FOR SHARE`, organization, p.UserID)
	batch.Queue(`SELECT organization_id FROM public.hosted_tenant WHERE organization_id=$1 FOR SHARE`, organization)
	batch.Queue(`SELECT runner_id FROM public.runner_identities WHERE organization_id=$1 AND token_id=$2 FOR SHARE`, organization, p.CredentialID)
	results := tx.SendBatch(ctx, batch)
	if err := results.Close(); err != nil {
		return Scope{}, unavailable(err)
	}
	current, err = a.ResolveIn(ctx, tx, scope.proof)
	if err != nil {
		return Scope{}, err
	}
	if current.Generation() != scope.Generation() || current.Principal() != scope.Principal() || current.RunnerID() != scope.RunnerID() {
		return Scope{}, ErrDenied
	}
	return current, nil
}

func (a *Authority) ResolveIn(ctx context.Context, tx *pgtransport.Transaction, input RequestIdentity) (Scope, error) {
	if tx == nil || input.Organization == "" || input.CredentialHash == "" || tx.Scope().Organization != input.Organization {
		return Scope{}, ErrDenied
	}
	return scanAuthority(tx.QueryRow(ctx, authoritySQL, string(input.Organization), input.CredentialHash, runnerstore.MaxRunnerProjectList+1), input, a.now())
}
