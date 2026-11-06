package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
