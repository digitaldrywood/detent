package hubserver

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readNativeRecovery(ctx context.Context, scope nativeScope, item string) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	result.Issue, _, err = readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return result, err
	}
	change, found, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
	if err != nil {
		return result, err
	}
	var sourceAttempt string
	if found {
		detail, err := readChangeDetailView(ctx, tx, scope, item, change.ID, s.config.now(), true)
		if err != nil {
			return result, err
		}
		result.ChangeDetail = &detail
		if change.CurrentVersion != "" {
			if len(detail.Versions) != 1 {
				return result, fmt.Errorf("read change: current version %s is missing", change.CurrentVersion)
			}
			version := detail.Versions[0]
			result.Change = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			sourceAttempt = version.AttemptID
		}
	}
	ids, err := nativePageIDs(ctx, tx, `SELECT id FROM native_comments
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND (
 json_extract(actor_json, '$.kind') = 'human' OR json_extract(edited_by_json, '$.kind') = 'human' OR id = (
 SELECT id FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
 AND json_extract(actor_json, '$.kind') != 'human' ORDER BY sequence DESC LIMIT 1)) ORDER BY sequence`,
		scope.organization, scope.project, item, scope.organization, scope.project, item)
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		comment, err := readNativeComment(ctx, tx, scope, item, id)
		if err != nil {
			return result, err
		}
		result.Discussion = append(result.Discussion, comment)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.data_json, a.status, a.started_at, a.updated_at, a.checkpoint_json, a.artifact_ids_json, l.expires_at, l.released_at, l.renewed_at, a.work_item_revision, a.dispatch_generation
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND (a.id = ? OR a.id = (
 SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? ORDER BY fencing_token DESC LIMIT 1)
 OR a.id = (SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
 AND json_extract(data_json, '$.runtime.landing') IS NOT NULL ORDER BY fencing_token DESC LIMIT 1)
 OR a.id = (SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ?
 AND checkpoint_json IS NOT NULL ORDER BY fencing_token DESC LIMIT 1))
ORDER BY a.fencing_token LIMIT 4`, scope.organization, scope.project, item, sourceAttempt,
		scope.organization, scope.project, item, scope.organization, scope.project, item, scope.organization, scope.project, item)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		attempt, err := scanNativeAttempt(rows, s.config.now())
		if err != nil {
			rows.Close()
			return result, err
		}
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		result.Attempts = append(result.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
