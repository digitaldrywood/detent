package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/policy"
)

func policyWorkflowRevision(descriptor policy.Descriptor) string {
	if descriptor.Workflow == nil {
		return ""
	}
	return descriptor.Workflow.Revision
}

func migratePolicyRevisionInputs(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE policy_revisions ADD COLUMN source_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE policy_revisions ADD COLUMN workflow_revision TEXT NOT NULL DEFAULT '';
CREATE INDEX policy_revisions_definition ON policy_revisions(scope, source_digest, approved_at DESC, policy_id);
CREATE INDEX policy_revisions_workflow ON policy_revisions(scope, source_digest, workflow_revision, approved_at DESC, policy_id);`); err != nil {
		return err
	}
	var scope, id string
	for {
		var nextScope, nextID, raw string
		err := tx.QueryRowContext(ctx, `SELECT scope, policy_id, metadata_json FROM policy_revisions
WHERE (scope, policy_id) > (?, ?) ORDER BY scope, policy_id LIMIT 1`, scope, id).Scan(&nextScope, &nextID, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var descriptor policy.Descriptor
		if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE policy_revisions SET source_digest=?, workflow_revision=? WHERE scope=? AND policy_id=?`, descriptor.SourceDigest, policyWorkflowRevision(descriptor), nextScope, nextID); err != nil {
			return err
		}
		scope, id = nextScope, nextID
	}
}
