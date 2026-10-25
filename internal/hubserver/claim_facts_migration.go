package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func migrateClaimFacts(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE change_requests ADD COLUMN claim_version_id TEXT NOT NULL DEFAULT '';
ALTER TABLE change_requests ADD COLUMN claim_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_preserves_source INTEGER NOT NULL DEFAULT 0;
CREATE INDEX native_attempts_source_checkpoint ON native_attempts(organization_id,project_id,work_item_id,fencing_token DESC) WHERE checkpoint_preserves_source=1;
CREATE INDEX change_evidence_claim_version ON change_evidence(change_id,version_id,sequence DESC);
CREATE INDEX native_attempts_claim_publication ON native_attempts(finalization_version_id,fencing_token DESC);`); err != nil {
		return err
	}
	var after string
	for {
		var id, raw string
		err := tx.QueryRowContext(ctx, `SELECT id,record_json FROM change_requests WHERE id>? ORDER BY id LIMIT 1`, after).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		var change struct {
			CurrentVersion string `json:"current_version_id"`
		}
		if err := json.Unmarshal([]byte(raw), &change); err != nil {
			return err
		}
		var version struct {
			HeadSHA string `json:"head_sha"`
		}
		if change.CurrentVersion != "" {
			err := tx.QueryRowContext(ctx, `SELECT record_json FROM change_versions WHERE change_id=? AND id=?`, id, change.CurrentVersion).Scan(&raw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if err := json.Unmarshal([]byte(raw), &version); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET claim_version_id=?,claim_head_sha=? WHERE id=?`, change.CurrentVersion, version.HeadSHA, id); err != nil {
			return err
		}
		after = id
	}
	after = ""
	for {
		var id, raw string
		err := tx.QueryRowContext(ctx, `SELECT id,checkpoint_json FROM native_attempts WHERE id>? AND checkpoint_json IS NOT NULL ORDER BY id LIMIT 1`, after).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var checkpoint *tracker.NativeCheckpoint
		if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE native_attempts SET checkpoint_preserves_source=? WHERE id=?`, checkpoint.PreservesSource(), id); err != nil {
			return err
		}
		after = id
	}
}
