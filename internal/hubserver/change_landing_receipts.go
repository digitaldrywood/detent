package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type landingReceiptDecodeError struct{}

func (*landingReceiptDecodeError) Error() string {
	return "landing receipt missing or empty in attempt object"
}

func (*landingReceiptDecodeError) Unwrap() error { return artifact.ErrIntegrity }

func readVersionLanding(ctx context.Context, query nativeQueryer, version string) (tracker.NativeLandingReceipt, error) {
	var raw string
	var object bool
	err := query.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(r.payload_object_key,''),r.record_json),r.payload_object_key<>'' FROM change_landing_receipts r
JOIN native_attempts a ON a.id=r.attempt_id WHERE r.version_id=? ORDER BY a.fencing_token DESC LIMIT 1`, version).Scan(&raw, &object)
	if err != nil {
		return tracker.NativeLandingReceipt{}, err
	}
	return decodeLandingReceipt(ctx, raw, object)
}

func decodeLandingReceipt(ctx context.Context, raw string, object bool) (tracker.NativeLandingReceipt, error) {
	if object {
		var run tracker.NativeRunData
		if err := decodeStorageJSON(ctx, []byte(raw), &run); err != nil {
			return tracker.NativeLandingReceipt{}, err
		}
		if run.Runtime == nil || run.Runtime.Landing == nil || reflect.DeepEqual(*run.Runtime.Landing, tracker.NativeLandingReceipt{}) {
			return tracker.NativeLandingReceipt{}, &landingReceiptDecodeError{}
		}
		return *run.Runtime.Landing, nil
	}
	var receipt tracker.NativeLandingReceipt
	err := decodeStorageJSON(ctx, []byte(raw), &receipt)
	return receipt, err
}

func recordAttemptLanding(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, attempt string, receipt tracker.NativeLandingReceipt) error {
	var previous tracker.NativeLandingReceipt
	var storedRaw string
	var object bool
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json),payload_object_key<>'' FROM change_landing_receipts WHERE attempt_id=?", attempt).Scan(&storedRaw, &object)
	if err == nil {
		previous, err = decodeLandingReceipt(ctx, storedRaw, object)
		if err != nil {
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
	raw, err := marshalNative(receipt)
	if err != nil {
		return err
	}
	var document, status string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),data_json),status FROM native_attempts WHERE id=?", attempt).Scan(&document, &status); err != nil {
		return err
	}
	var data tracker.NativeRunData
	resolved, err := resolveStorageJSON(ctx, []byte(document))
	if err != nil {
		return err
	}
	document = string(resolved)
	if err := decodeStorageJSON(ctx, []byte(document), &data); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := decodeStorageJSON(ctx, []byte(document), &object); err != nil {
		return err
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	var runtime map[string]json.RawMessage
	if value := object["runtime"]; len(value) > 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &runtime); err != nil {
			return err
		}
	}
	if runtime == nil {
		runtime = map[string]json.RawMessage{}
	}
	runtime["landing"] = json.RawMessage(raw)
	encodedRuntime, err := json.Marshal(runtime)
	if err != nil {
		return err
	}
	object["runtime"] = encodedRuntime
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	var organization string
	if err := tx.QueryRowContext(ctx, "SELECT organization_id FROM native_attempts WHERE id=?", attempt).Scan(&organization); err != nil {
		return err
	}
	key, err := storedPayloadKey(ctx, organization, encoded)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET data_json='{}',payload_object_key=?,analytics_revision=analytics_revision+1 WHERE id=?", key, attempt); err != nil {
		return err
	}
	if data.Runtime == nil {
		data.Runtime = &tracker.NativeRuntimeObservation{}
	}
	data.Runtime.Landing = &receipt
	if err := updateAttemptClaimFacts(ctx, tx, attempt, data); err != nil {
		return err
	}
	if err := updateAttemptHotProjection(ctx, tx, attempt, status, data); err != nil {
		return err
	}
	if receipt.VersionID == "" {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_landing_receipts (attempt_id, version_id, record_json, payload_object_key, merge_sha) VALUES (?, ?, '{}', ?, ?)
ON CONFLICT(attempt_id) DO UPDATE SET version_id=excluded.version_id, record_json='{}', payload_object_key=excluded.payload_object_key, merge_sha=excluded.merge_sha`, attempt, receipt.VersionID, key, receipt.MergeSHA)
	if err != nil {
		return err
	}
	return nil
}

func recordLandingTransition(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, from string) error {
	var attempt string
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, COALESCE(NULLIF(payload_object_key,''),data_json) FROM native_attempts
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
	var data tracker.NativeRunData
	if err := decodeStorageJSON(ctx, []byte(raw.String), &data); err != nil {
		return err
	}
	if data.Runtime == nil || data.Runtime.Landing == nil {
		return nil
	}
	receipt := *data.Runtime.Landing
	if receipt.FromState != from || receipt.TargetState != "" {
		return nil
	}
	receipt.TargetState = issue.State
	return persistAttemptLanding(ctx, tx, attempt, receipt)
}
