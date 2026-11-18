package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
)

const setAttemptClaimFactsSQL = `UPDATE native_attempts SET claim_disposition=$6
WHERE organization_id=$1 AND id=$2 AND sequence=$3 AND payload_object_key=$4
AND checkpoint_object_key IS NOT DISTINCT FROM $5 AND status=$7
AND (NOT $8 OR claim_disposition IS NULL)`

func (s *session) SetAttemptClaimFacts(ctx context.Context, facts leasestore.AttemptClaimFacts) error {
	if s.readOnly || !facts.Valid() {
		return corestore.ErrInvalidPage
	}
	result, err := s.db.Exec(ctx, setAttemptClaimFactsSQL, s.organization, facts.AttemptID, facts.Sequence, facts.PayloadObjectKey, facts.CheckpointObjectKey, facts.Disposition, facts.Status, facts.UnresolvedOnly)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (f *runnerEventSession) SetAttemptClaimFacts(ctx context.Context, facts leasestore.AttemptClaimFacts) error {
	if !facts.Valid() || f.attempt.err != nil {
		return corestore.ErrInvalidPage
	}
	current := f.attempt.value
	if current.ID != facts.AttemptID || current.Sequence != facts.Sequence || current.Status != facts.Status || current.PayloadObjectKey != facts.PayloadObjectKey || !sameClaimCheckpoint(current.CheckpointObjectKey, facts.CheckpointObjectKey) || facts.UnresolvedOnly && current.ClaimDisposition != nil {
		return corestore.ErrRevisionConflict
	}
	f.pending.Queue(setAttemptClaimFactsSQL, f.session.organization, facts.AttemptID, facts.Sequence, facts.PayloadObjectKey, facts.CheckpointObjectKey, facts.Disposition, facts.Status, facts.UnresolvedOnly).Exec(func(result pgconn.CommandTag) error {
		if result.RowsAffected() != 1 {
			return corestore.ErrRevisionConflict
		}
		return nil
	})
	f.attempt.value.ClaimDisposition = new(facts.Disposition)
	if f.latest.err == nil && f.latest.value.ID == facts.AttemptID {
		f.latest.value.ClaimDisposition = new(facts.Disposition)
	}
	return nil
}

func (f *runnerEventSession) ArchiveObjectByKey(ctx context.Context, key string) (artifactstore.ArchiveObject, error) {
	return f.session.ArchiveObjectByKey(ctx, key)
}

func sameClaimCheckpoint(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

var _ leasestore.ClaimFactsWriter = (*session)(nil)
var _ leasestore.ClaimFactsWriter = (*runnerEventSession)(nil)
