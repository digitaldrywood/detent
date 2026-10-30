package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeBoardRank(ctx context.Context, q nativeQueryer, project, state string) (int, error) {
	var rank sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT (SELECT board_rank FROM workflow_states WHERE project_id=p.id AND source_name=?) FROM projects p WHERE p.id=?`, state, project).Scan(&rank); err != nil {
		return 0, err
	}
	if rank.Valid {
		return int(rank.Int64), nil
	}
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT states_json FROM projects WHERE id=?", project).Scan(&raw); err != nil {
		return 0, err
	}
	var states []tracker.NativeState
	if err := json.Unmarshal([]byte(raw), &states); err != nil {
		return 0, err
	}
	for i, s := range states {
		if s.Name == state {
			return i, nil
		}
	}
	return 2147483647, nil
}

func (s *Service) convertNativeBoardRanks(ctx context.Context) error {
	positions, err := s.storageProjectionPage(ctx, "board-ranks", `SELECT project_id FROM workflow_states WHERE board_rank IS NULL`, "project_id", storageConversionLimit(ctx))
	if err != nil {
		return err
	}
	type candidate struct{ id, raw string }
	var candidates []candidate
	for _, position := range positions {
		c := candidate{id: position.id}
		if err := s.database.reader.QueryRowContext(ctx, "SELECT states_json FROM projects WHERE id=?", c.id).Scan(&c.raw); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	for _, c := range candidates {
		if err := chargeStorageConversion(ctx, len(c.raw)); err != nil {
			return err
		}
		if err := s.convertNativeBoardRank(ctx, c.id, c.raw); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) convertNativeBoardRank(ctx context.Context, project, raw string) error {
	var states []tracker.NativeState
	if err := json.Unmarshal([]byte(raw), &states); err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var unchanged bool
	err = tx.QueryRowContext(ctx, "SELECT states_json=? FROM projects WHERE id=?", raw, project).Scan(&unchanged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !unchanged {
		return nil
	}
	for rank, state := range states {
		if _, err := tx.ExecContext(ctx, "UPDATE workflow_states SET board_rank=? WHERE project_id=? AND source_name=? AND board_rank IS NULL", rank, project, state.Name); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE workflow_states SET board_rank=2147483647 WHERE project_id=? AND board_rank IS NULL", project); err != nil {
		return err
	}
	return tx.Commit()
}
