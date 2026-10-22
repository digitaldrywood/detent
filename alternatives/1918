package hubserver

import (
	"context"
	"database/sql"
	"time"
)

// nativeDispatchState is the pair a claim decision is made against: the
// version of the item an attempt answers, and the generation of the last
// explicit request for a new attempt on it.
type nativeDispatchState struct {
	revision   int64
	generation int64
}

// readNativeDispatchState reads the item's current revision and dispatch
// generation. It is deliberately a direct read of the two columns rather than
// a whole issue read: it runs inside the run.started transaction, which has
// already resolved and fenced the item.
func readNativeDispatchState(ctx context.Context, tx *sql.Tx, scope nativeScope, workItemID string) (nativeDispatchState, error) {
	var state nativeDispatchState
	err := tx.QueryRowContext(
		ctx,
		"SELECT revision, dispatch_generation FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?",
		scope.organization, scope.project, workItemID,
	).Scan(&state.revision, &state.generation)
	return state, err
}

// requestNativeDispatch records that the item wants a new attempt even though
// nothing about the item itself changed.
//
// A conversation continuation is the case that forces this to be explicit. A
// `continue` command bumps no revision, moves no lane and edits no content
// (decisions.md section 10), so nothing a claim query can read distinguishes
// "this item already had its answer" from "the user asked for another turn".
// Inferring it was tried and it broke the continuation the product promises
// (operations.md section 7), so the request is written down instead: the
// generation moves forward, an attempt records the generation it started
// under, and the claim query offers the item again exactly while the request
// is newer than the last succeeded attempt.
func requestNativeDispatch(ctx context.Context, tx *sql.Tx, scope nativeScope, workItemID string, now time.Time) error {
	_, err := tx.ExecContext(
		ctx,
		`UPDATE issues SET dispatch_generation = dispatch_generation + 1, dispatch_requested_at = ?
WHERE organization_id = ? AND project_id = ? AND native_id = ?`,
		formatHubTime(now), scope.organization, scope.project, workItemID,
	)
	return err
}
