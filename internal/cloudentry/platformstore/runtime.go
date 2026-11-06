package platformstore

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RuntimePublication struct{ Revision, ObjectKey string }

type RuntimeAdoption struct {
	Generation      int64
	Managed         bool
	State           string
	Publication     RuntimePublication
	AppliedRevision string
}

func (r *Repository) RuntimeTenantPage(ctx context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	if limit < 1 || limit > hubmaintenance.TenantLimit {
		return nil, ErrInvalid
	}
	var tenants []hubmaintenance.Tenant
	err := r.read(ctx, corestore.Background, func(tx pgquery.DBTX) error {
		rows, err := pgquery.New(tx).RuntimeTenantPage(ctx, pgquery.RuntimeTenantPageParams{AfterID: after, PageLimit: int32(limit)})
		for _, row := range rows {
			tenants = append(tenants, hubmaintenance.Tenant{Organization: string(row.ID), Generation: row.Generation})
		}
		return err
	})
	return tenants, err
}

func (r *Repository) RuntimeAdoption(ctx context.Context, organization tracker.OrganizationID, issuer string) (RuntimeAdoption, error) {
	var adoption RuntimeAdoption
	err := r.read(ctx, corestore.Background, func(tx pgquery.DBTX) error {
		row, err := pgquery.New(tx).RuntimeAdoptionTarget(ctx, pgquery.RuntimeAdoptionTargetParams{OrganizationID: string(organization), Issuer: issuer})
		adoption = RuntimeAdoption{Generation: row.Generation, Managed: row.Managed, State: row.State, Publication: RuntimePublication{Revision: row.RevisionID, ObjectKey: row.ObjectKey}, AppliedRevision: row.AppliedRevision}
		return err
	})
	return adoption, err
}

func (r *Repository) RuntimePublication(ctx context.Context, issuer string) (RuntimePublication, error) {
	var result RuntimePublication
	err := r.read(ctx, corestore.Serving, func(tx pgquery.DBTX) error {
		row, err := pgquery.New(tx).EntryRuntime(ctx, issuer)
		result = RuntimePublication{Revision: row.RevisionID, ObjectKey: row.ObjectKey}
		return err
	})
	return result, err
}

func (r *Repository) PublishRuntime(ctx context.Context, revision, issuer, key string, organizations []tracker.OrganizationID) error {
	if revision == "" || len(organizations) > 10000 || issuer != "" && key == "" {
		return ErrInvalid
	}
	targets := slices.Clone(organizations)
	slices.Sort(targets)
	if len(slices.Compact(targets)) != len(organizations) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		q := pgquery.New(tx)
		locked, err := q.LockRuntimePublicationTargets(ctx, pgquery.LockRuntimePublicationTargetsParams{Organizations: runtimeTargetStrings(targets), Revision: revision})
		if err != nil {
			return err
		}
		slices.Sort(locked)
		if !slices.Equal(locked, targets) {
			return corestore.ErrRevisionConflict
		}
		if err := q.PublishRuntime(ctx, revision); err != nil {
			return err
		}
		if issuer == "" {
			return nil
		}
		return q.SetEntryRuntime(ctx, pgquery.SetEntryRuntimeParams{Issuer: issuer, RevisionID: revision, ObjectKey: key})
	})
}

func runtimeTargetStrings(ids []tracker.OrganizationID) []string {
	result := make([]string, len(ids))
	for i, id := range ids {
		result[i] = string(id)
	}
	return result
}
