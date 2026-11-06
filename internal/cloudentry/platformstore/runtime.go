package platformstore

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RuntimePublication struct{ Revision, ObjectKey string }

func (r *Repository) RuntimePublication(ctx context.Context, issuer string) (RuntimePublication, error) {
	var result RuntimePublication
	err := r.transaction(ctx, corestore.Serving, true, func(tx pgx.Tx) error {
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
	return r.transaction(ctx, corestore.Serving, false, func(tx pgx.Tx) error {
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
