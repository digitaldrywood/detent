package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const archiveFactsRowsPerTick = 200

func writeArchiveChangeFacts(ctx context.Context, tx *sql.Tx, change tracker.ChangeRequest, source string) error {
	landed := tracker.ChangeLanding{}
	var at, merge any
	if change.Landed != nil {
		landed = *change.Landed
		at, merge = landed.LandedAt.Format(time.RFC3339Nano), landed.MergeSHA
	}
	query := `UPDATE change_requests SET archive_landed_version_id=?,archive_landed_at=?,archive_landed_merge_sha=?,archive_actor_kind=?,archive_actor_principal_id=?,archive_actor_via_key=?,archive_actor_display_name=?,archive_actor_agent=? WHERE id=?`
	args := []any{landed.VersionID, at, merge, landed.Actor.Kind, landed.Actor.PrincipalID, landed.Actor.ViaKey, landed.Actor.DisplayName, landed.Actor.Agent, change.ID}
	if source != "" {
		query += " AND archive_landed_version_id IS NULL AND record_json=?"
		args = append(args, source)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func archiveEventValues(raw string) ([]any, error) {
	var data struct {
		Operation string  `json:"operation"`
		From      *string `json:"from_state"`
		To        *string `json:"to_state"`
		Run       *struct {
			Attempt      *string `json:"attempt_id"`
			Outcome      *string `json:"outcome"`
			Finalization *struct {
				Publication *struct {
					External *struct {
						HeadSHA *string `json:"head_sha"`
					} `json:"external"`
				} `json:"publication"`
			} `json:"finalization"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, err
	}
	var attempt, outcome, commit *string
	if data.Run != nil {
		attempt, outcome = data.Run.Attempt, data.Run.Outcome
		if data.Run.Finalization != nil && data.Run.Finalization.Publication != nil && data.Run.Finalization.Publication.External != nil {
			commit = data.Run.Finalization.Publication.External.HeadSHA
		}
	}
	return []any{data.Operation, data.From, data.To, attempt, outcome, commit}, nil
}

func writeArchiveEventFacts(ctx context.Context, tx *sql.Tx, id string, values []any, source string) error {
	args := append(values, id)
	query := `UPDATE collaboration_events SET archive_operation=?,archive_from_state=?,archive_to_state=?,archive_attempt_id=?,archive_outcome=?,archive_landed_commit=? WHERE id=?`
	if source != "" {
		query += " AND archive_operation IS NULL AND data_json=?"
		args = append(args, source)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func writeConversationExecutionFacts(ctx context.Context, tx *sql.Tx, id string, execution conversation.Execution, source string) error {
	query := `UPDATE conversations SET execution_status=?,execution_lease_id=? WHERE id=?`
	args := []any{execution.Status, execution.Owner.LeaseID, id}
	if source != "" {
		query += " AND (execution_status IS NULL OR execution_lease_id IS NULL) AND execution_json=?"
		args = append(args, source)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func (d *database) populateArchiveFacts(ctx context.Context) error {
	for range archiveFactsRowsPerTick {
		changed, err := d.populateArchiveFact(ctx)
		if err != nil || !changed {
			return err
		}
	}
	return nil
}

func (d *database) populateArchiveFact(ctx context.Context) (bool, error) {
	var id, raw string
	var write func(context.Context, *sql.Tx) error
	err := d.reader.QueryRowContext(ctx, `SELECT id,record_json FROM change_requests WHERE archive_landed_version_id IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
	if err == nil {
		var change tracker.ChangeRequest
		if err := json.Unmarshal([]byte(raw), &change); err != nil {
			return false, err
		}
		change.ID = id
		write = func(ctx context.Context, tx *sql.Tx) error { return writeArchiveChangeFacts(ctx, tx, change, raw) }
	} else if errors.Is(err, sql.ErrNoRows) {
		err = d.reader.QueryRowContext(ctx, `SELECT id,execution_json FROM conversations WHERE execution_status IS NULL OR execution_lease_id IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
		if err == nil {
			var execution conversation.Execution
			if err := json.Unmarshal([]byte(raw), &execution); err != nil {
				return false, err
			}
			write = func(ctx context.Context, tx *sql.Tx) error {
				return writeConversationExecutionFacts(ctx, tx, id, execution, raw)
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			err = d.reader.QueryRowContext(ctx, `SELECT id,data_json FROM collaboration_events WHERE archive_operation IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			values, err := archiveEventValues(raw)
			if err != nil {
				return false, err
			}
			write = func(ctx context.Context, tx *sql.Tx) error { return writeArchiveEventFacts(ctx, tx, id, values, raw) }
		}
	}
	if err != nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := write(ctx, tx); err != nil {
		return false, err
	}
	return true, d.commit(ctx, tx)
}

func archiveNullValue(value sql.NullString) any {
	if value.Valid {
		return value.String
	}
	return nil
}

func readArchiveTimeline(ctx context.Context, q nativeQueryer, scope nativeScope, item string) ([]json.RawMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT sequence,type,actor_json,recorded_at,archive_from_state,archive_to_state,archive_attempt_id,archive_outcome,archive_landed_commit,CASE WHEN archive_operation IS NULL THEN data_json END FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type IN ('workflow.transitioned','run.started','run.finished') ORDER BY sequence`, scope.organization, scope.project, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var timeline []json.RawMessage
	for rows.Next() {
		var sequence int64
		var kind, actor, at string
		var from, to, attempt, outcome, commit, legacy sql.NullString
		if err := rows.Scan(&sequence, &kind, &actor, &at, &from, &to, &attempt, &outcome, &commit, &legacy); err != nil {
			return nil, err
		}
		values := []any{nil, archiveNullValue(from), archiveNullValue(to), archiveNullValue(attempt), archiveNullValue(outcome), archiveNullValue(commit)}
		if legacy.Valid {
			values, err = archiveEventValues(legacy.String)
			if err != nil {
				return nil, err
			}
		}
		raw, err := json.Marshal(map[string]any{"sequence": sequence, "type": kind, "actor": json.RawMessage(actor), "recorded_at": at, "from_state": values[1], "to_state": values[2], "attempt_id": values[3], "outcome": values[4], "landed_commit": values[5]})
		if err != nil {
			return nil, err
		}
		timeline = append(timeline, raw)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT c.id,c.archive_landed_at,c.archive_landed_merge_sha,c.archive_actor_kind,c.archive_actor_principal_id,c.archive_actor_via_key,c.archive_actor_display_name,c.archive_actor_agent,CASE WHEN c.archive_landed_version_id IS NULL THEN c.record_json END FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE l.organization_id=? AND l.project_id=? AND l.work_item_id=? AND (c.archive_landed_merge_sha IS NOT NULL OR c.archive_landed_version_id IS NULL) ORDER BY c.id`, scope.organization, scope.project, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at, merge, kind, principal, via, display, agent, legacy sql.NullString
		if err := rows.Scan(&id, &at, &merge, &kind, &principal, &via, &display, &agent, &legacy); err != nil {
			return nil, err
		}
		actor := tracker.Actor{Kind: kind.String, PrincipalID: principal.String, ViaKey: via.String, DisplayName: display.String, Agent: agent.String}
		if legacy.Valid {
			var change tracker.ChangeRequest
			if err := json.Unmarshal([]byte(legacy.String), &change); err != nil {
				return nil, err
			}
			if change.Landed == nil {
				continue
			}
			at, merge = sql.NullString{String: change.Landed.LandedAt.Format(time.RFC3339Nano), Valid: true}, sql.NullString{String: change.Landed.MergeSHA, Valid: true}
			actor = change.Landed.Actor
		}
		raw, err := json.Marshal(map[string]any{"type": "change.landed", "recorded_at": archiveNullValue(at), "actor": actor, "landed_commit": archiveNullValue(merge), "change_id": id})
		if err != nil {
			return nil, err
		}
		timeline = append(timeline, raw)
	}
	return timeline, errors.Join(rows.Err(), rows.Close())
}
