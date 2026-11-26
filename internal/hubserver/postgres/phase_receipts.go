package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RecordPhaseReceipt(ctx context.Context, project tracker.ProjectID, issueID int64, input tracker.NativePhaseReceipt) (tracker.NativePhaseReceipt, error) {
	existing, err := s.queries.NativePhaseReceiptByKey(ctx, pgquery.NativePhaseReceiptByKeyParams{
		OrganizationID: s.organization, ProjectID: project, IssueID: issueID,
		FencingToken: int64(input.FencingToken), IdempotencyKey: input.IdempotencyKey,
	})
	if err == nil {
		receipt := phaseReceipt(existing)
		if receipt != input {
			return tracker.NativePhaseReceipt{}, mutation.ErrConflict
		}
		return receipt, nil
	}
	if err = mapError(ctx, err); !errors.Is(err, corestore.ErrNotFound) {
		return tracker.NativePhaseReceipt{}, err
	}
	row, err := s.queries.InsertNativePhaseReceipt(ctx, pgquery.InsertNativePhaseReceiptParams{
		OrganizationID: s.organization, ProjectID: project, IssueID: issueID,
		IdempotencyKey: input.IdempotencyKey, LeaseID: string(input.LeaseID), FencingToken: int64(input.FencingToken),
		AttemptID: input.AttemptID, Phase: input.Phase, Branch: input.Branch, HeadSHA: input.HeadSHA,
		TreeSha: input.TreeSHA, PolicyID: input.PolicyID, InputDigest: input.InputDigest,
	})
	return phaseReceipt(row), mapError(ctx, err)
}

func (s *session) AttemptHasSourceReceipt(ctx context.Context, project tracker.ProjectID, issueID int64, attempt, head, policyID string) (bool, error) {
	found, err := s.queries.NativeAttemptHasSourceReceipt(ctx, pgquery.NativeAttemptHasSourceReceiptParams{
		OrganizationID: s.organization, ProjectID: project, IssueID: issueID,
		AttemptID: attempt, HeadSHA: head, PolicyID: policyID,
	})
	return found, mapError(ctx, err)
}

func (s *session) RecentPhaseReceipts(ctx context.Context, project tracker.ProjectID, issueID int64) ([]tracker.NativePhaseReceipt, error) {
	rows, err := s.queries.RecentNativePhaseReceipts(ctx, pgquery.RecentNativePhaseReceiptsParams{
		OrganizationID: s.organization, ProjectID: project, IssueID: issueID,
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	receipts := make([]tracker.NativePhaseReceipt, 0, len(rows))
	for _, row := range rows {
		receipts = append(receipts, phaseReceipt(row))
	}
	return receipts, nil
}

func (s *session) PhaseContinuation(ctx context.Context, project tracker.ProjectID, issueID int64, policyID string, phases, inputDigests []string) (tracker.NativePhaseContinuation, error) {
	rows, err := s.queries.NativePhaseContinuation(ctx, pgquery.NativePhaseContinuationParams{
		OrganizationID: s.organization, ProjectID: project, IssueID: issueID,
		PolicyID: policyID, Phases: phases, InputDigests: inputDigests,
	})
	if err != nil {
		return tracker.NativePhaseContinuation{}, mapError(ctx, err)
	}
	continuation := tracker.NativePhaseContinuation{Receipts: make([]tracker.NativePhaseReceipt, 0, len(phases))}
	for _, row := range rows {
		if row.Ordinal != 0 && row.Ordinal != int64(len(continuation.Receipts)+1) {
			break
		}
		receipt := phaseReceipt(pgquery.NativePhaseReceipt{
			IdempotencyKey: row.IdempotencyKey, LeaseID: row.LeaseID, FencingToken: row.FencingToken,
			AttemptID: row.AttemptID, Phase: row.Phase, Branch: row.Branch, HeadSHA: row.HeadSHA,
			TreeSha: row.TreeSha, PolicyID: row.PolicyID, InputDigest: row.InputDigest,
		})
		if row.Ordinal == 0 {
			continuation.LastPushed = &receipt
		} else {
			continuation.Receipts = append(continuation.Receipts, receipt)
		}
	}
	if len(continuation.Receipts) < len(phases) {
		continuation.NextPhase = phases[len(continuation.Receipts)]
	}
	return continuation, nil
}

func phaseReceipt(row pgquery.NativePhaseReceipt) tracker.NativePhaseReceipt {
	return tracker.NativePhaseReceipt{
		Mutation: tracker.Mutation{IdempotencyKey: row.IdempotencyKey, LeaseID: tracker.LeaseID(row.LeaseID), FencingToken: tracker.FencingToken(row.FencingToken)},
		Phase:    row.Phase, AttemptID: row.AttemptID, Branch: row.Branch, HeadSHA: row.HeadSHA,
		TreeSHA: row.TreeSha, PolicyID: row.PolicyID, InputDigest: row.InputDigest,
	}
}
