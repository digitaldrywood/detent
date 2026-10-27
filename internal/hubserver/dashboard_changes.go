package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeBoardRetentionCandidateQuery = `SELECT organization_id,project_id FROM native_board_sequences
WHERE retained_count>2048 AND (organization_id,project_id)>(?,?) ORDER BY organization_id,project_id LIMIT 1`

type dashboardChange struct {
	Sequence       int64  `json:"sequence"`
	Topic          string `json:"topic"`
	EntityID       string `json:"entity_id"`
	EntityRevision int64  `json:"entity_revision"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	RelatedID      string `json:"related_id,omitempty"`
}

type dashboardChangePage struct {
	NextSequence int64             `json:"next_sequence"`
	Sequence     int64             `json:"sequence"`
	Floor        int64             `json:"floor"`
	Gap          bool              `json:"gap"`
	Changes      []dashboardChange `json:"changes"`
}

func readDashboardWatermark(ctx context.Context, q nativeQueryer, scope nativeScope) (int64, int64, error) {
	var head, floor int64
	err := q.QueryRowContext(ctx, "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&head, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read dashboard watermark: %w", err)
	}
	return head, floor, nil
}

func readDashboardChanges(ctx context.Context, q nativeQueryer, scope nativeScope, since int64, limit int) (dashboardChangePage, error) {
	page := dashboardChangePage{NextSequence: since, Changes: []dashboardChange{}}
	var err error
	page.Sequence, page.Floor, err = readDashboardWatermark(ctx, q, scope)
	if err != nil {
		return page, err
	}
	if since < page.Floor || since > page.Sequence {
		page.Gap = true
		return page, nil
	}
	limit = max(1, min(limit, nativeBoardReplayLimit))
	rows, err := q.QueryContext(ctx, `SELECT sequence,topic,entity_id,entity_revision,kind,status,related_id FROM native_board_deltas
WHERE organization_id=? AND project_id=? AND sequence>? ORDER BY sequence LIMIT ?`, scope.organization, scope.project, since, limit)
	if err != nil {
		return page, fmt.Errorf("read dashboard changes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var change dashboardChange
		if err := rows.Scan(&change.Sequence, &change.Topic, &change.EntityID, &change.EntityRevision, &change.Kind, &change.Status, &change.RelatedID); err != nil {
			return page, fmt.Errorf("scan dashboard change: %w", err)
		}
		page.Changes = append(page.Changes, change)
		page.NextSequence = change.Sequence
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("iterate dashboard changes: %w", err)
	}
	return page, nil
}

func nativeBoardChangeKind(current *tracker.NativeBoardCard) string {
	if current == nil {
		return "delete"
	}
	return "upsert"
}

func trimDashboardChanges(ctx context.Context, tx *sql.Tx, scope nativeScope) error {
	var cutoff int64
	err := tx.QueryRowContext(ctx, `SELECT sequence FROM native_board_deltas WHERE organization_id=? AND project_id=?
ORDER BY sequence DESC LIMIT 1 OFFSET ?`, scope.organization, scope.project, nativeBoardReplayLimit).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read dashboard retention cutoff: %w", err)
	}
	var floor int64
	err = tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM (SELECT sequence FROM native_board_deltas
WHERE organization_id=? AND project_id=? AND sequence<=? ORDER BY sequence LIMIT ?)`, scope.organization, scope.project, cutoff, nativeBoardTrimLimit).Scan(&floor)
	if err != nil {
		return fmt.Errorf("read dashboard trim floor: %w", err)
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM native_board_deltas WHERE organization_id=? AND project_id=? AND sequence<=?", scope.organization, scope.project, floor)
	if err != nil {
		return fmt.Errorf("trim dashboard changes: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count trimmed dashboard changes: %w", err)
	}
	_, err = tx.ExecContext(ctx, "UPDATE native_board_sequences SET replay_floor=MAX(replay_floor,?),retained_count=retained_count-? WHERE organization_id=? AND project_id=?", floor, removed, scope.organization, scope.project)
	if err != nil {
		return fmt.Errorf("advance dashboard replay floor: %w", err)
	}
	return nil
}

func (s *Service) trimNextDashboardProject(ctx context.Context) error {
	var scope nativeScope
	err := s.database.reader.QueryRowContext(ctx, nativeBoardRetentionCandidateQuery, s.boardRetentionScope.organization, s.boardRetentionScope.project).Scan(&scope.organization, &scope.project)
	if errors.Is(err, sql.ErrNoRows) {
		s.boardRetentionScope = nativeScope{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read dashboard retention project: %w", err)
	}
	s.boardRetentionScope = scope
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dashboard retention: %w", err)
	}
	defer tx.Rollback()
	if err := trimDashboardChanges(ctx, tx, scope); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dashboard retention: %w", err)
	}
	return nil
}

func appendDashboardHealthFinding(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,status)
SELECT f.organization_id,hp.project_id,'health',f.id,f.dashboard_revision,CASE WHEN f.resolved_at IS NULL THEN f.severity ELSE 'resolved' END
FROM health_findings f JOIN health_finding_projects hp ON hp.finding_id=f.id WHERE f.id=?`, id)
	if err != nil {
		return fmt.Errorf("append dashboard health finding: %w", err)
	}
	return nil
}
