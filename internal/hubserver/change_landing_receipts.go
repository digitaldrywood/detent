package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readVersionLanding(ctx context.Context, query nativeQueryer, version string) (tracker.NativeLandingReceipt, error) {
	var raw string
	err := query.QueryRowContext(ctx, `SELECT r.record_json FROM change_landing_receipts r
JOIN native_attempts a ON a.id=r.attempt_id WHERE r.version_id=? ORDER BY a.fencing_token DESC LIMIT 1`, version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = query.QueryRowContext(ctx, "SELECT record_json FROM landing_barrier_receipts WHERE version_id=?", version).Scan(&raw)
	}
	if err != nil {
		return tracker.NativeLandingReceipt{}, err
	}
	var receipt tracker.NativeLandingReceipt
	err = json.Unmarshal([]byte(raw), &receipt)
	if err != nil {
		return receipt, err
	}
	return withLandingBarrier(ctx, query, receipt)
}

func recordAttemptLanding(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, attempt string, receipt tracker.NativeLandingReceipt) error {
	var previous tracker.NativeLandingReceipt
	var storedRaw string
	err := tx.QueryRowContext(ctx, "SELECT record_json FROM change_landing_receipts WHERE attempt_id=?", attempt).Scan(&storedRaw)
	if err == nil {
		if err := json.Unmarshal([]byte(storedRaw), &previous); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	left, right := previous, receipt
	left.ObservedAt, right.ObservedAt = time.Time{}, time.Time{}
	left.FromState, left.TargetState = "", ""
	right.FromState, right.TargetState = "", ""
	if storedRaw != "" && reflect.DeepEqual(left, right) {
		receipt = previous
	} else {
		issue, _, err := readNativeIssue(ctx, tx, scope, string(item))
		if err != nil {
			return err
		}
		receipt.FromState = issue.State
		if receipt.Landed {
			transition, err := latestNativeRuntimeEvent(ctx, tx, scope, string(item), "workflow.transitioned", "")
			if err != nil {
				return err
			}
			if transition.Actor == scope.actor() && transition.Data.ToState == issue.State {
				receipt.FromState, receipt.TargetState = transition.Data.FromState, transition.Data.ToState
			}
		}
	}
	return persistAttemptLanding(ctx, tx, attempt, receipt)
}

func persistAttemptLanding(ctx context.Context, tx *sql.Tx, attempt string, receipt tracker.NativeLandingReceipt) error {
	receipt, err := withLandingBarrier(ctx, tx, receipt)
	if err != nil {
		return err
	}
	raw, err := marshalNative(receipt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET data_json=json_set(data_json, '$.runtime.landing', json(?)) WHERE id=?", raw, attempt); err != nil {
		return err
	}
	if receipt.VersionID == "" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_landing_receipts (attempt_id, version_id, record_json) VALUES (?, ?, ?)
ON CONFLICT(attempt_id) DO UPDATE SET version_id=excluded.version_id, record_json=excluded.record_json`, attempt, receipt.VersionID, raw)
	return err
}

func recordLandingTransition(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, from string) error {
	var attempt string
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, json_extract(data_json, '$.runtime.landing') FROM native_attempts
WHERE organization_id=? AND project_id=? AND work_item_id=?
ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&attempt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !raw.Valid {
		return nil
	}
	var receipt tracker.NativeLandingReceipt
	if err := json.Unmarshal([]byte(raw.String), &receipt); err != nil {
		return err
	}
	if receipt.FromState != from || receipt.TargetState != "" {
		return nil
	}
	receipt.TargetState = issue.State
	return persistAttemptLanding(ctx, tx, attempt, receipt)
}

func withLandingBarrier(ctx context.Context, query nativeQueryer, receipt tracker.NativeLandingReceipt) (tracker.NativeLandingReceipt, error) {
	if !receipt.Landed || receipt.VersionID == "" {
		return receipt, nil
	}
	var raw sql.NullString
	err := query.QueryRowContext(ctx, "SELECT json_extract(record_json, '$.barrier') FROM landing_barrier_receipts WHERE version_id=?", receipt.VersionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, nil
	}
	if err != nil {
		return receipt, err
	}
	if raw.Valid {
		err = json.Unmarshal([]byte(raw.String), &receipt.Barrier)
	}
	return receipt, err
}
