package hubserver

import (
	"context"
	"database/sql"
	"errors"

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

const (
	sessionHistoryStarted = 1 << iota
	sessionHistoryFinished
	sessionHistoryInvalid
)

func prepareChangeSession(ctx context.Context, query nativeQueryer, version tracker.ChangeVersion) (changeSessionAuthority, error) {
	authority := changeSessionAuthority{}
	if version.AttemptID == "" {
		authority.session = sql.NullInt64{Int64: -1, Valid: true}
		return authority, nil
	}
	var session sql.NullInt64
	if err := query.QueryRowContext(ctx, `SELECT data_json,implementing_session_id FROM native_attempts WHERE id=? AND run_id=?`, version.AttemptID, version.RunID).Scan(&authority.raw, &session); err != nil {
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
	result, err := tx.ExecContext(ctx, `UPDATE change_versions SET implementing_session_id=?,implementing_machine_id=? WHERE id=? AND (?='' OR EXISTS(SELECT 1 FROM native_attempts WHERE id=? AND data_json=?))`, authority.session, authority.machine, version.ID, version.AttemptID, version.AttemptID, authority.raw)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return nativeExecutionConflict("Source attempt changed during publication")
	}
	return err
}

func (s *Service) populateChangeSession(ctx context.Context) (bool, error) {
	var id, change, organization, project, item, machine string
	var after, candidate, state int64
	err := s.database.reader.QueryRowContext(ctx, `SELECT v.id,v.change_id,c.organization_id,c.project_id,c.work_item_id,v.session_history_sequence,v.session_history_candidate,v.session_history_state,v.session_history_machine_id FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE v.implementing_session_id IS NULL AND v.id>? ORDER BY v.id LIMIT 1`, s.changeSessionAfter).Scan(&id, &change, &organization, &project, &item, &after, &candidate, &state, &machine)
	if errors.Is(err, sql.ErrNoRows) {
		s.changeSessionAfter = ""
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, err := readChangeVersionSnapshot(ctx, s.database.reader, change, id)
	if err != nil {
		return true, err
	}
	var sequence int64
	var kind, eventRaw string
	err = s.database.reader.QueryRowContext(ctx, `SELECT sequence,type,data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? ORDER BY sequence LIMIT 1`, organization, project, item, after).Scan(&sequence, &kind, &eventRaw)
	if err == nil {
		var event tracker.CollaborationData
		if err := decodeRunHistory(ctx, s.database.reader, eventRaw, &event); err != nil {
			return true, err
		}
		if run := event.Run; run != nil && run.AttemptID == version.AttemptID {
			if machine == "" {
				machine = string(run.MachineID)
			}
			if machine == "" || machine != string(run.MachineID) {
				state |= sessionHistoryInvalid
			}
			if run.RunID != version.RunID || run.PolicyID != version.PolicyID || run.Identity == nil || run.Identity.Role != "code" && run.Identity.Role != "rework" {
				state |= sessionHistoryInvalid
			}
			if session := nativeImplementingSession(*run); session > 0 {
				if candidate > 0 && candidate != session {
					state |= sessionHistoryInvalid
				}
				candidate = session
			}
			if kind == "run.started" {
				state |= sessionHistoryStarted
			}
			if kind == "run.finished" {
				state |= sessionHistoryFinished
			}
		}
		_, err = s.database.db.ExecContext(ctx, `UPDATE change_versions SET session_history_sequence=?,session_history_candidate=?,session_history_state=?,session_history_machine_id=? WHERE id=? AND implementing_session_id IS NULL AND session_history_sequence=?`, sequence, candidate, state, machine, id, after)
		return true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	var source tracker.NativeRunData
	var sourceRaw, status string
	var recordedSession sql.NullInt64
	err = s.database.reader.QueryRowContext(ctx, `SELECT data_json,status,implementing_session_id FROM native_attempts WHERE id=? AND run_id=?`, version.AttemptID, version.RunID).Scan(&sourceRaw, &status, &recordedSession)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	if err == nil {
		if err := decodeRunDetail(ctx, s.database.reader, sourceRaw, &source); err != nil {
			return true, err
		}
	}
	if session := nativeImplementingSession(source); session > 0 {
		if candidate > 0 && candidate != session {
			state |= sessionHistoryInvalid
		}
		candidate = session
	}
	if recordedSession.Valid && recordedSession.Int64 > 0 {
		if candidate > 0 && candidate != recordedSession.Int64 {
			state |= sessionHistoryInvalid
		}
		candidate = recordedSession.Int64
	}
	if status == "running" && candidate == 0 {
		s.changeSessionAfter = id
		return true, nil
	}
	if source.Identity == nil || source.Identity.Role != "code" && source.Identity.Role != "rework" || source.PolicyID != version.PolicyID || machine != string(source.MachineID) || state&sessionHistoryInvalid != 0 || state&sessionHistoryStarted == 0 || candidate == 0 && (state&sessionHistoryFinished == 0 || status == "running" || source.Identity.Role != "rework" || nativeImplementingSession(source) != 0) {
		candidate = -1
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE change_versions SET implementing_session_id=?,implementing_machine_id=? WHERE id=? AND implementing_session_id IS NULL AND session_history_sequence=? AND (?=-1 OR EXISTS(SELECT 1 FROM native_attempts WHERE id=? AND data_json=?)) AND NOT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>?)`, candidate, machine, id, after, candidate, version.AttemptID, sourceRaw, organization, project, item, after)
	if err != nil {
		return true, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return true, err
	}
	if count == 1 && candidate >= 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE native_attempts SET implementing_session_id=? WHERE id=? AND data_json=? AND (implementing_session_id IS NULL OR implementing_session_id<=0)`, candidate, version.AttemptID, sourceRaw); err != nil {
			return true, err
		}
	}
	err = s.database.commit(ctx, tx)
	s.changeSessionAfter = id
	return true, err
}
