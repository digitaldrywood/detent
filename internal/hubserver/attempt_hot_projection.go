package hubserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func applyHealthAttemptData(a *healthRateAttempt, data tracker.NativeRunData) {
	a.Session = data.SessionID
	if data.TerminalFailure != nil {
		a.Stage = data.TerminalFailure.Stage
	}
	if f := data.Finalization; f != nil && f.Settled && f.Changed {
		a.SourceVersion = f.VersionID
	}
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
	var settled, changed bool
	var versionID, finalizationID, publicationURL, checkpointID string
	if f := data.Finalization; f != nil {
		settled, changed, versionID = f.Settled, f.Changed, f.VersionID
		finalizationID = f.ChangeID
		if f.Publication != nil {
			publicationURL = f.Publication.External.URL
		}
	}
	if data.Handoff != nil && data.Handoff.Change != nil {
		checkpointID = data.Handoff.Change.ChangeID
	}
	var dispositionReason, dispositionStatus string
	var dispositionHumanAction bool
	if data.Disposition != nil {
		dispositionReason, dispositionStatus, dispositionHumanAction = data.Disposition.ReasonCode, data.Disposition.Status, data.Disposition.HumanAction
	}
	var local int64
	if data.Runtime != nil {
		local = data.Runtime.LocalAttemptID
	}
	_, err := tx.ExecContext(ctx, `UPDATE native_attempts SET finalization_change_id=?,finalization_external_url=?,checkpoint_change_id=CASE WHEN ? THEN ? WHEN checkpoint_json IS NULL THEN '' ELSE checkpoint_change_id END,runner_id=?,activity_identity_known=CASE WHEN ? OR checkpoint_json IS NULL OR checkpoint_change_id IS NOT NULL THEN 1 ELSE NULL END,policy_id=?,has_landing_receipt=?,hot_projection_version=1,session_id=?,local_attempt_id=?,heartbeat_at=?,health_failure_signature=?,health_recovery_reason=?,health_instance_failure=?,health_landing=?,health_landed=?,health_conflict=?,health_landing_at=?,health_landing_key=?,failure_stage=?,finalization_settled=?,finalization_changed=?,finalization_version_id=?,health_disposition_reason=?,disposition_status=?,disposition_human_action=?,health_updated_day=julianday(updated_at),health_landing_day=julianday(?) WHERE id=?`, finalizationID, publicationURL, data.Handoff != nil, checkpointID, data.RunnerID, data.Handoff != nil, data.PolicyID, data.Runtime != nil && data.Runtime.Landing != nil, a.Session, local, heartbeat, a.Signature, a.Recovery, a.InstanceSetupFailure, a.Landing, a.Landed, a.Conflict, landingAt, a.LandingKey, a.Stage, settled, changed, versionID, dispositionReason, dispositionStatus, dispositionHumanAction, landingAt, id)
	return err
}

func (s *Service) convertAttemptHotProjections(ctx context.Context) error {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT id FROM native_attempts WHERE hot_projection_version=0 OR health_disposition_reason IS NULL ORDER BY id LIMIT ?`, storageConversionRows)
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
	err := s.database.reader.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),data_json),status FROM native_attempts WHERE id=? AND (hot_projection_version=0 OR health_disposition_reason IS NULL)", id).Scan(&raw, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var data tracker.NativeRunData
	if err := decodeStorageJSON(ctx, []byte(raw), &data); err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var unchanged bool
	err = tx.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),data_json)=? AND status=? AND (hot_projection_version=0 OR health_disposition_reason IS NULL) FROM native_attempts WHERE id=?", raw, status, id).Scan(&unchanged)
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
