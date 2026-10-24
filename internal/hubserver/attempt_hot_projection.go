package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func applyHealthAttemptData(a *healthRateAttempt, data tracker.NativeRunData) {
	if f := data.TerminalFailure; a.Status == "failed" && f != nil && f.Stage == "pre_provider" && f.Attribution == "instance" {
		a.InstanceSetupFailure = true
	}
	projected := tracker.NativeRunData{TerminalFailure: data.TerminalFailure, Finalization: data.Finalization}
	if data.Runtime != nil {
		projected.Runtime = &tracker.NativeRuntimeObservation{HeartbeatAt: data.Runtime.HeartbeatAt, Landing: data.Runtime.Landing}
	}
	text, _ := failureFromAttempt(projected, a.Status)
	a.Recovery = healthRecoveryReason(text)
	if text != "" && a.Recovery == "" {
		a.Signature = normalizeFailureSignature(text)
	}
	if r := data.Runtime; r != nil {
		a.Heartbeat = r.HeartbeatAt
		if l := r.Landing; l != nil {
			if l.Landed || l.RefusalKind != "" {
				a.LandingAt = l.ObservedAt
				a.Landing, a.Landed, a.Conflict = true, l.Landed, !l.Landed && l.RefusalKind == "conflict"
				if l.Landed && l.ChangeID != "" {
					a.LandingKey = l.ChangeID + "\x00" + l.VersionID + "\x00" + l.MergeSHA
				}
			}
		}
	}
}

func updateAttemptHotProjection(ctx context.Context, tx *sql.Tx, id, status string, data tracker.NativeRunData) error {
	a := healthRateAttempt{Status: status}
	applyHealthAttemptData(&a, data)
	heartbeat, landingAt := "", ""
	if !a.Heartbeat.IsZero() {
		heartbeat = formatHubTime(a.Heartbeat)
	}
	if data.Runtime != nil && data.Runtime.Landing != nil {
		landingAt = projectionTime(data.Runtime.Landing.ObservedAt)
	}
	var local int64
	if data.Runtime != nil {
		local = data.Runtime.LocalAttemptID
	}
	_, err := tx.ExecContext(ctx, `UPDATE native_attempts SET hot_projection_version=1,local_attempt_id=?,heartbeat_at=?,health_failure_signature=?,health_recovery_reason=?,health_instance_failure=?,health_landing=?,health_landed=?,health_conflict=?,health_landing_at=?,health_landing_key=?,health_updated_day=julianday(updated_at),health_landing_day=julianday(?) WHERE id=?`, local, heartbeat, a.Signature, a.Recovery, a.InstanceSetupFailure, a.Landing, a.Landed, a.Conflict, landingAt, a.LandingKey, landingAt, id)
	return err
}

func (s *Service) convertAttemptHotProjections(ctx context.Context) error {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id FROM native_attempts WHERE hot_projection_version=0 ORDER BY id LIMIT ?`, storageConversionRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.convertAttemptHotProjection(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) convertAttemptHotProjection(ctx context.Context, id string) error {
	var raw, status string
	err := s.database.reader.QueryRowContext(ctx, "SELECT data_json,status FROM native_attempts WHERE id=? AND hot_projection_version=0", id).Scan(&raw, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var data tracker.NativeRunData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var unchanged bool
	err = tx.QueryRowContext(ctx, "SELECT data_json=? AND status=? AND hot_projection_version=0 FROM native_attempts WHERE id=?", raw, status, id).Scan(&unchanged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !unchanged {
		return nil
	}
	if err := updateAttemptHotProjection(ctx, tx, id, status, data); err != nil {
		return err
	}
	return tx.Commit()
}
