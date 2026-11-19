package claimapi

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadNativeClaimChangeFacts(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	ids := []string{}
	for _, plan := range plans {
		if (hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending) && plan.ChangeRecord.ID != "" {
			ids = append(ids, plan.ChangeRecord.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	reader, ok := r.(tenantstore.ClaimChangeRowsReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	rows, err := reader.ClaimChangeRows(ctx, scope.Project, ids)
	if err != nil {
		return err
	}
	for i := range plans {
		if row, ok := rows[plans[i].ChangeRecord.ID]; ok {
			row.VersionObservations = plans[i].Observations
			plans[i].ChangeFacts = &row
		}
	}

	return nil
}

func (d Deps) PrepareNativeClaimLegacyAuditFacts(ctx context.Context, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	runs := []int64{}
	seen := map[int64]bool{}
	content := eventstore.NewContent(d.DetailsFor(ctx))
	for _, plan := range plans {
		facts := plan.ChangeFacts
		if facts == nil {
			continue
		}
		if len(facts.Evidence) > corestore.MaxPageSize {
			return corestore.ErrInvalidPage
		}
		for _, row := range facts.Evidence {
			if row.Kind != "review" || row.VersionID == nil || *row.VersionID != plan.ChangeVersion.ID || row.ReviewKind != "" && row.ReviewSessionKnown {
				continue
			}
			raw, err := content.Evidence(ctx, row)
			if err != nil {
				return err
			}
			var review tracker.ChangeReview
			if err := json.Unmarshal(raw, &review); err != nil {
				return err
			}
			if review.SecurityAudit == nil {
				continue
			}
			id := review.SecurityAudit.Run.ID
			if _, covered := facts.AuditDispositions[id]; covered || seen[id] {
				continue
			}
			seen[id] = true
			runs = append(runs, id)
		}
	}
	if len(runs) == 0 {
		return nil
	}
	repository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	var rows map[int64][]eventstore.AuditDisposition
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, true); err != nil {
			return err
		}
		reader, ok := r.(tenantstore.ClaimAuditDispositionRowsReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		rows, err = reader.ClaimAuditDispositionRows(ctx, scope.Project, runs)
		return err
	})
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if plan.ChangeFacts == nil {
			continue
		}
		for id, values := range rows {
			plan.ChangeFacts.AuditDispositions[id] = values
		}
	}
	return nil
}
