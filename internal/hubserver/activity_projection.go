package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func activityRunRole(data tracker.NativeRunData) string {
	if data.Runtime != nil && data.Runtime.Identity.Role != "" {
		return data.Runtime.Identity.Role
	}
	if data.Identity != nil {
		return data.Identity.Role
	}
	return ""
}

func activityRunPhases(data tracker.NativeRunData) ([]tracker.NativePhase, int) {
	if data.Runtime == nil {
		return nil, 0
	}
	return data.Runtime.Phases, data.Runtime.PhasesDropped
}

func updateActivityAttemptProjection(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint) error {
	if err := updateReportAttemptProjection(ctx, tx, id, data); err != nil {
		return err
	}
	phases, dropped := activityRunPhases(data)
	var role, executionRole, phase, change, url string
	if data.Runtime != nil {
		phase = data.Runtime.Phase
		role = data.Runtime.Identity.Role
	}
	if data.Finalization != nil {
		change = data.Finalization.ChangeID
		if data.Finalization.Publication != nil {
			url = data.Finalization.Publication.External.URL
		}
	}
	if data.Identity != nil {
		executionRole = data.Identity.Role
	}
	var checkpointChange any
	if checkpoint == nil {
		var raw sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) FROM native_attempts WHERE id=? AND activity_checkpoint_change_id IS NULL`, id).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			checkpointChange = ""
			if raw.Valid {
				stored, err := decodeRunCheckpoint(ctx, tx, raw.String)
				if err != nil {
					return err
				}
				if stored != nil && stored.Change != nil {
					checkpointChange = stored.Change.ChangeID
				}
			}
		}
	}
	if checkpoint != nil {
		checkpointChange = ""
		if checkpoint.Change != nil {
			checkpointChange = checkpoint.Change.ChangeID
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE native_attempts SET activity_projection_version=1,role=?,execution_role=?,runner_id=?,session_id=?,runtime_phase=?,finalization_change_id=?,activity_phases_dropped=?,activity_checkpoint_change_id=coalesce(?,activity_checkpoint_change_id,''),activity_publication_url=? WHERE id=?`, role, executionRole, data.RunnerID, data.SessionID, phase, change, dropped, checkpointChange, url, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM activity_attempt_phases WHERE attempt_id=?`, id); err != nil {
		return err
	}
	for position, p := range phases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO activity_attempt_phases(attempt_id,position,name,started_at,finished_at) VALUES(?,?,?,?,?)`, id, position, p.Name, formatHubTime(p.StartedAt), projectionTime(p.FinishedAt)); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO activity_attempt_durations(attempt_id,organization_id,project_id,status,role,started_at,updated_at,seconds,partial)
 SELECT id,organization_id,project_id,status,role,activity_started_at,activity_updated_at,CASE WHEN status IN ('succeeded','failed') THEN ? ELSE 0 END,? FROM native_attempts WHERE id=?
 ON CONFLICT(attempt_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id,status=excluded.status,role=excluded.role,started_at=excluded.started_at,updated_at=excluded.updated_at,seconds=excluded.seconds,partial=excluded.partial`, closedAttemptSeconds(phases), len(phases) == 0 || dropped > 0, id)
	return err
}

func readActivityPhases(ctx context.Context, q nativeQueryer, id string) ([]tracker.NativePhase, error) {
	rows, err := q.QueryContext(ctx, `SELECT name,started_at,finished_at FROM activity_attempt_phases WHERE attempt_id=? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var phases []tracker.NativePhase
	for rows.Next() {
		var p tracker.NativePhase
		var start, finish string
		if err := rows.Scan(&p.Name, &start, &finish); err != nil {
			return nil, err
		}
		p.StartedAt, err = parseTimeValue(start)
		if err != nil {
			return nil, err
		}
		if finish != "" {
			p.FinishedAt, err = parseTimeValue(finish)
			if err != nil {
				return nil, err
			}
		}
		phases = append(phases, p)
	}
	return phases, rows.Err()
}

func (s *Service) convertActivityProjections(ctx context.Context) error {
	ctx = s.runDetailContext(ctx)
	families := []struct {
		name, selection string
		convert         func(context.Context, string) error
	}{
		{"activity-attempts", `SELECT id FROM native_attempts WHERE activity_projection_version IS NULL`, s.convertActivityAttempt},
		{"report-attempts", `SELECT id FROM native_attempts WHERE report_projection_version IS NULL`, s.convertActivityAttempt},
		{"activity-actors", `SELECT id FROM issues WHERE activity_actor_principal_id IS NULL`, s.convertActivityActor},
		{"activity-changes", `SELECT id FROM change_requests WHERE activity_current_version_id IS NULL`, s.convertActivityChange},
		{"activity-versions", `SELECT id FROM change_versions WHERE activity_external_url IS NULL`, s.convertActivityVersion},
	}
	start := int((s.activityConvertFamily.Add(1) - 1) % uint64(len(families)))
	families = append(families[start:], families[:start]...)
	remaining := storageConversionRows
	var failures error
	for _, family := range families {
		if remaining == 0 {
			break
		}
		candidates, err := s.storageProjectionPage(ctx, family.name, family.selection, "id", remaining)
		if err != nil {
			return errors.Join(failures, err)
		}
		for _, candidate := range candidates {
			remaining--
			s.projectionCursors.Store(family.name, candidate.cursor)
			if err := family.convert(ctx, candidate.id); err != nil {
				failures = errors.Join(failures, err)
				if ctx.Err() != nil {
					return failures
				}
			}
		}
	}
	return failures
}

func (s *Service) convertActivityAttempt(ctx context.Context, id string) error {
	var raw, status string
	var checkpoint sql.NullString
	err := s.database.reader.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),data_json),status,COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) FROM native_attempts WHERE id=? AND (activity_projection_version IS NULL OR report_projection_version IS NULL)`, id).Scan(&raw, &status, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var data tracker.NativeRunData
	if err := decodeActivityRun(ctx, s.database.reader, raw, &data); err != nil {
		return err
	}
	cp := &tracker.NativeCheckpoint{}
	if checkpoint.Valid {
		var err error
		cp, err = decodeRunCheckpoint(ctx, s.database.reader, checkpoint.String)
		if err != nil {
			return err
		}
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var unchanged bool
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),data_json)=? AND status=? AND COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) IS ? AND (activity_projection_version IS NULL OR report_projection_version IS NULL) FROM native_attempts WHERE id=?`, raw, status, checkpoint, id).Scan(&unchanged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !unchanged {
		return nil
	}
	if err := updateActivityAttemptProjection(ctx, tx, id, data, cp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) convertActivityActor(ctx context.Context, id string) error {
	var raw string
	err := s.database.reader.QueryRowContext(ctx, `SELECT actor_json FROM issues WHERE id=? AND activity_actor_principal_id IS NULL`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var actor tracker.Actor
	if err := decodeStorageJSON(ctx, []byte(raw), &actor); err != nil {
		return err
	}
	_, err = s.database.db.ExecContext(ctx, `UPDATE issues SET activity_actor_principal_id=? WHERE id=? AND actor_json=? AND activity_actor_principal_id IS NULL`, actor.PrincipalID, id, raw)
	return err
}

func (s *Service) convertActivityChange(ctx context.Context, id string) error {
	var raw string
	err := s.database.reader.QueryRowContext(ctx, `SELECT record_json FROM change_requests WHERE id=? AND activity_current_version_id IS NULL`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var c tracker.ChangeRequest
	if err := decodeStorageJSON(ctx, []byte(raw), &c); err != nil {
		return err
	}
	_, err = s.database.db.ExecContext(ctx, `UPDATE change_requests SET activity_created_at=julianday(?),activity_current_version_id=? WHERE id=? AND record_json=? AND activity_current_version_id IS NULL`, formatHubTime(c.CreatedAt), c.CurrentVersion, id, raw)
	return err
}

func (s *Service) convertActivityVersion(ctx context.Context, id string) error {
	var raw string
	err := s.database.reader.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE id=? AND activity_external_url IS NULL`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var v tracker.ChangeVersion
	if err := decodeStorageJSON(ctx, []byte(raw), &v); err != nil {
		return err
	}
	var url string
	if v.External != nil {
		url = v.External.URL
	}
	_, err = s.database.db.ExecContext(ctx, `UPDATE change_versions SET activity_external_url=? WHERE id=? AND COALESCE(NULLIF(payload_object_key,''),record_json)=? AND activity_external_url IS NULL`, url, id, raw)
	return err
}

func decodeActivityRun(ctx context.Context, q nativeQueryer, raw string, data *tracker.NativeRunData) error {
	var document map[string]json.RawMessage
	if err := decodeStorageJSON(ctx, []byte(raw), &document); err != nil {
		return err
	}
	if document == nil {
		return decodeRunDetail(ctx, q, raw, data)
	}
	var runtime map[string]json.RawMessage
	if value := document["runtime"]; len(value) > 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &runtime); err != nil {
			return err
		}
		var phases []tracker.NativePhase
		if value := runtime["phases"]; len(value) > 0 && json.Unmarshal(value, &phases) != nil {
			delete(runtime, "phases")
			encoded, err := json.Marshal(runtime)
			if err != nil {
				return err
			}
			document["runtime"] = encoded
			encoded, err = json.Marshal(document)
			if err != nil {
				return err
			}
			raw = string(encoded)
		}
	}
	return decodeRunDetail(ctx, q, raw, data)
}
