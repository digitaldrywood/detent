package hubserver

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeImplementingSession(run tracker.NativeRunData) int64 {
	return nativeRuntimeSession(run.Runtime)
}

func nativeRuntimeSession(runtime *tracker.NativeRuntimeObservation) int64 {
	if runtime == nil || runtime.Activity == nil {
		return -1
	}
	return runtime.Activity.SessionID
}

type changeSessionAuthority struct {
	session sql.NullInt64
	machine tracker.MachineID
	raw     string
}

func prepareChangeSession(ctx context.Context, query nativeQueryer, version tracker.ChangeVersion) (changeSessionAuthority, error) {
	authority := changeSessionAuthority{}
	if version.AttemptID == "" {
		authority.session = sql.NullInt64{Int64: -1, Valid: true}
		return authority, nil
	}
	var session sql.NullInt64
	if err := query.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),data_json),implementing_session_id FROM native_attempts WHERE id=? AND run_id=?`, version.AttemptID, version.RunID).Scan(&authority.raw, &session); err != nil {
		return authority, err
	}
	var source tracker.NativeRunData
	if err := decodeRunDetail(ctx, query, authority.raw, &source); err != nil {
		return authority, err
	}
	if source.PolicyID != version.PolicyID || source.Identity == nil || source.Identity.Role != "code" && source.Identity.Role != "rework" {
		session = sql.NullInt64{Int64: -1, Valid: true}
	} else if session.Valid && session.Int64 == 0 && source.Identity.Role != "rework" {
		session.Int64 = -1
	}
	authority.session, authority.machine = session, source.MachineID
	return authority, nil
}

func freezeChangeSession(ctx context.Context, tx *sql.Tx, version tracker.ChangeVersion, authority changeSessionAuthority) error {
	result, err := tx.ExecContext(ctx, `UPDATE change_versions SET implementing_session_id=?,implementing_machine_id=? WHERE id=? AND (?='' OR EXISTS(SELECT 1 FROM native_attempts WHERE id=? AND COALESCE(NULLIF(payload_object_key,''),data_json)=?))`, authority.session, authority.machine, version.ID, version.AttemptID, version.AttemptID, authority.raw)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return nativeExecutionConflict("Source attempt changed during publication")
	}
	return err
}
