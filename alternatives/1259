package hubserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func migrateClaimFacts(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `ALTER TABLE change_requests ADD COLUMN claim_version_id TEXT;
ALTER TABLE change_requests ADD COLUMN claim_head_sha TEXT;
ALTER TABLE native_attempts ADD COLUMN checkpoint_preserves_source INTEGER;
ALTER TABLE change_evidence ADD COLUMN claim_review_kind TEXT;
ALTER TABLE change_evidence ADD COLUMN claim_review_actor TEXT;
ALTER TABLE change_evidence ADD COLUMN claim_review_decision TEXT;
ALTER TABLE change_evidence ADD COLUMN claim_review_current INTEGER;
CREATE INDEX change_evidence_claim_unknown ON change_evidence(change_id,version_id,sequence) WHERE kind='review' AND claim_review_current IS NULL;
CREATE UNIQUE INDEX change_evidence_claim_reviewer ON change_evidence(change_id,version_id,claim_review_kind,claim_review_actor) WHERE claim_review_current=1;
CREATE INDEX change_evidence_claim_review ON change_evidence(change_id,version_id,claim_review_kind,claim_review_current,claim_review_decision,sequence DESC) WHERE kind='review';
CREATE INDEX change_evidence_claim_rejection ON change_evidence(change_id,version_id,claim_review_kind,claim_review_decision,sequence DESC) WHERE kind='review';
CREATE INDEX native_attempts_source_checkpoint ON native_attempts(organization_id,project_id,work_item_id,fencing_token DESC) WHERE checkpoint_preserves_source=1 OR checkpoint_preserves_source IS NULL;
CREATE INDEX change_evidence_claim_version ON change_evidence(change_id,version_id,sequence DESC);
CREATE INDEX native_attempts_claim_publication ON native_attempts(finalization_version_id,fencing_token DESC);
CREATE INDEX change_requests_claim_unknown ON change_requests(id) WHERE claim_version_id IS NULL OR claim_head_sha IS NULL;
CREATE INDEX native_attempts_claim_unknown ON native_attempts(id) WHERE checkpoint_preserves_source IS NULL;`)
	return err
}

const claimFactsRowsPerTick = 1000

func (d *database) populateClaimFacts(ctx context.Context) error {
	for range claimFactsRowsPerTick {
		changed, err := d.populateClaimFact(ctx)
		if err != nil || !changed {
			return err
		}
	}
	return nil
}

func (d *database) populateClaimFact(ctx context.Context) (bool, error) {
	write, err := prepareLegacyAdmissionFact(ctx, d.reader)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if write == nil {
		return false, nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed := true
	err = write(tx)
	if err != nil {
		return false, err
	}
	return changed, d.commit(ctx, tx)
}

func claimChangeIdentity(ctx context.Context, q nativeQueryer, id, raw string) (string, string, error) {
	var change struct {
		CurrentVersion string `json:"current_version_id"`
	}
	if err := decodeStorageJSON(ctx, []byte(raw), &change); err != nil {
		return "", "", err
	}
	var version struct {
		HeadSHA string `json:"head_sha"`
	}
	if change.CurrentVersion != "" {
		err := q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND id=?`, id, change.CurrentVersion).Scan(&raw)
		if err != nil {
			return "", "", err
		}
		if err := decodeStorageJSON(ctx, []byte(raw), &version); err != nil {
			return "", "", err
		}
	}
	return change.CurrentVersion, version.HeadSHA, nil
}

func projectClaimReview(ctx context.Context, tx *sql.Tx, sequence int64, change, version, raw string) error {
	var review tracker.ChangeReview
	if err := decodeStorageJSON(ctx, []byte(raw), &review); err != nil {
		return err
	}
	kind, actor, decision := "human", review.Actor.PrincipalID, review.Decision
	switch {
	case review.SecurityAudit != nil:
		kind, actor = "audit", ""
		if review.SecurityAudit.Landing {
			kind = "landing_audit"
		}
	case review.ImplementationReview != nil:
		kind, actor = "implementation", ""
	case review.Validator != nil:
		kind, actor = "validator", ""
	}
	current := kind != "human" || decision != "commented"
	var previous int64
	if current {
		err := tx.QueryRowContext(ctx, `SELECT sequence FROM change_evidence WHERE change_id=? AND version_id=? AND claim_review_kind=? AND claim_review_actor=? AND claim_review_current=1`, change, version, kind, actor).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current = previous < sequence
	}
	if current && previous != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE change_evidence SET claim_review_current=0 WHERE sequence=?`, previous); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE change_evidence SET claim_review_kind=?,claim_review_actor=?,claim_review_decision=?,claim_review_current=? WHERE sequence=?`, kind, actor, decision, current, sequence); err != nil {
		return err
	}
	return nil
}
