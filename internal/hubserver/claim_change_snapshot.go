package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeClaimChangeFacts(ctx context.Context, r tenantstore.Reader, scope nativeScope, plans []nativeClaimPlan) error {
	ids := []string{}
	for _, plan := range plans {
		if (stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending) && plan.changeRecord.ID != "" {
			ids = append(ids, plan.changeRecord.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	reader, ok := r.(tenantstore.ClaimChangeRowsReader)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	rows, err := reader.ClaimChangeRows(ctx, scope.project, ids)
	if err != nil {
		return err
	}
	for i := range plans {
		if row, ok := rows[plans[i].changeRecord.ID]; ok {
			plans[i].changeFacts = &row
		}
	}
	return nil
}

func (s *Service) prepareNativeClaimLegacyAuditFacts(ctx context.Context, scope nativeScope, plans []nativeClaimPlan) error {
	runs := []int64{}
	seen := map[int64]bool{}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, plan := range plans {
		facts := plan.changeFacts
		if facts == nil {
			continue
		}
		if len(facts.Evidence) > corestore.MaxPageSize {
			return corestore.ErrInvalidPage
		}
		for _, row := range facts.Evidence {
			if row.Kind != "review" || row.VersionID == nil || *row.VersionID != plan.changeVersion.ID || row.ReviewKind != "" && row.ReviewSessionKnown {
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
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	var rows map[int64][]eventstore.AuditDisposition
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		reader, ok := r.(tenantstore.ClaimAuditDispositionRowsReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		rows, err = reader.ClaimAuditDispositionRows(ctx, scope.project, runs)
		return err
	})
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if plan.changeFacts == nil {
			continue
		}
		for id, values := range rows {
			plan.changeFacts.AuditDispositions[id] = values
		}
	}
	return nil
}
