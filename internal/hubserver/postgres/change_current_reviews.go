package postgres

import (
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) putCurrentChangeReview(ctx context.Context, row eventstore.Evidence) error {
	if row.Kind != "review" || row.ReviewKind == "" || !row.ReviewSessionKnown || row.Decision == "commented" {
		return nil
	}
	if row.VersionID == nil || row.ReviewKind == "human" && row.ActorPrincipalID == "" || row.Decision != "approved" && row.Decision != "changes_requested" {
		return eventstore.ErrInvalidRecord
	}
	put := func(kind, actor string) error {
		_, err := s.db.Exec(ctx, `INSERT INTO current_change_reviews(organization_id,change_id,version_id,review_kind,actor_key,decision,evidence_sequence)
VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,change_id,version_id,review_kind,actor_key)
DO UPDATE SET decision=excluded.decision,evidence_sequence=excluded.evidence_sequence
WHERE current_change_reviews.evidence_sequence<excluded.evidence_sequence`, s.organization, row.ChangeID, *row.VersionID, kind, actor, row.Decision, row.Sequence)
		return mapError(ctx, err)
	}
	switch row.ReviewKind {
	case "human":
		if err := put("human", row.ActorPrincipalID); err != nil {
			return err
		}
		if row.Decision == "approved" {
			return put("human_prior_approval", "")
		}
	case "validator", "implementation", "security_audit":
		if !row.ReviewLanding {
			if err := put(row.ReviewKind, ""); err != nil {
				return err
			}
		}
		if row.ReviewKind == "security_audit" && row.Decision == "changes_requested" {
			return put("security_audit_failure", "")
		}
	default:
		return eventstore.ErrInvalidRecord
	}
	return nil
}

func (r *Repository) BackfillCurrentChangeReviews(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int) (int, error) {
	n, _, err := r.BackfillCurrentChangeReviewsAfter(ctx, organization, content, limit, "")
	return n, err
}

func (r *Repository) BackfillCurrentChangeReviewsAfter(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int, after string) (count int, candidate string, resultErr error) {
	if organization == "" || content == nil || limit < 1 || limit > corestore.MaxPageSize {
		return 0, "", corestore.ErrInvalidPage
	}
	var source eventstore.Change
	defer func() { candidate = source.ID }()
	var rows []eventstore.Evidence
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		pending, err := s.queries.RecordPendingChangeReviewSummary(ctx, pgquery.RecordPendingChangeReviewSummaryParams{OrganizationID: organization, AfterID: after})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		source = eventstore.Change(pending)
		rows, err = s.Evidence(ctx, source.ID, source.ReviewSummarySequence, limit)
		return err
	})
	if err != nil || source.ID == "" {
		return 0, "", err
	}
	rows = slices.DeleteFunc(rows, func(row eventstore.Evidence) bool { return row.Sequence > source.EvidenceSequence })
	if len(rows) == 0 {
		return 0, "", eventstore.ErrInvalidRecord
	}
	classification := &ChangeRecoveryProjectionBatch{organization: organization}
	for i, row := range rows {
		if row.Kind != "review" || row.ReviewKind != "" && row.ReviewSessionKnown {
			continue
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return 0, "", err
		}
		review, err := eventstore.DecodeChangeReview(row, raw)
		if err != nil {
			return 0, "", err
		}
		projected, err := eventstore.ProjectChangeReview(row, review)
		if err != nil {
			return 0, "", err
		}
		if row.ReviewKind != "" && (row.ReviewKind != projected.ReviewKind || row.ReviewLanding != projected.ReviewLanding) || row.ReviewSessionKnown && (row.ReviewSessionID != projected.ReviewSessionID || row.ReviewSessionMachine != projected.ReviewSessionMachine) {
			return 0, "", eventstore.ErrInvalidRecord
		}
		if row.ReviewKind == "" || !row.ReviewSessionKnown {
			classification.reviews = append(classification.reviews, projected)
		}
		rows[i] = projected
		rows = rows[:i+1]
		break
	}
	if len(classification.reviews) > 0 {
		if _, err := r.ApplyChangeRecoveryProjections(ctx, classification); err != nil {
			return 0, "", err
		}
	}
	err = r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		current, err := s.LockChange(ctx, source.ID)
		if err != nil {
			return err
		}
		if current.ReviewSummarySequence != source.ReviewSummarySequence || current.ReviewEvidenceRevision != source.ReviewEvidenceRevision {
			return corestore.ErrRevisionConflict
		}
		for _, row := range rows {
			if err := s.putCurrentChangeReview(ctx, row); err != nil {
				return err
			}
		}
		_, err = s.db.Exec(ctx, `UPDATE change_requests SET review_summary_sequence=$3 WHERE organization_id=$1 AND id=$2`, organization, source.ID, rows[len(rows)-1].Sequence)
		return mapError(ctx, err)
	})
	if err != nil {
		return 0, "", err
	}
	return len(rows), "", nil
}

func (s *session) invalidateCurrentChangeReviews(ctx context.Context, row eventstore.Evidence, previousThrough int64) error {
	if row.Sequence > previousThrough {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE change_requests SET review_evidence_revision=review_evidence_revision+1,
review_summary_sequence=least(review_summary_sequence,$3-1)
WHERE organization_id=$1 AND id=$2`, s.organization, row.ChangeID, row.Sequence)
	return mapError(ctx, err)
}

func (s *session) ChangeReviewEvidence(ctx context.Context, change, version string, through int64, checks []string, actor string) ([]eventstore.Evidence, bool, error) {
	if actor == "" || version == "" || through < 0 || len(checks) > corestore.MaxPageSize {
		return nil, false, corestore.ErrInvalidPage
	}
	before, err := s.Change(ctx, change)
	if err != nil {
		return nil, false, err
	}
	if before.CurrentVersionID == nil || *before.CurrentVersionID != version || before.EvidenceSequence != through || before.ReviewSummarySequence != through {
		return nil, false, corestore.ErrRevisionConflict
	}
	sequences, err := s.queries.RecordCurrentChangeReviewSequences(ctx, pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: s.organization, ChangeID: change, VersionID: version, ReviewingActor: actor, Through: through})
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	rows, err := s.queries.RecordCurrentChangeEvidenceBySequences(ctx, pgquery.RecordCurrentChangeEvidenceBySequencesParams{OrganizationID: s.organization, ChangeID: change, Sequences: sequences})
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	if len(rows) != len(sequences) {
		return nil, false, eventstore.ErrInvalidRecord
	}
	result := make([]eventstore.Evidence, 0, len(rows)+len(checks))
	for _, row := range rows {
		if row.VersionID == nil || *row.VersionID != version || row.Sequence > through || row.Kind != "review" || row.OrganizationID != s.organization || row.ChangeID != change {
			return nil, false, eventstore.ErrInvalidRecord
		}
		if row.ReviewKind == "security_audit" {
			return nil, false, eventstore.ErrInvalidRecord
		}
		result = append(result, evidenceRecord(pgquery.ChangeEvidence(row)))
	}
	checkRows, err := s.queries.RecordCurrentChangeReviewChecks(ctx, pgquery.RecordCurrentChangeReviewChecksParams{OrganizationID: s.organization, ChangeID: change, VersionID: new(version), CheckIds: checks})
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	for _, row := range checkRows {
		if row.Sequence > through {
			return nil, false, corestore.ErrRevisionConflict
		}
		result = append(result, evidenceRecord(row))
	}
	prior, err := s.queries.RecordCurrentChangePriorApproval(ctx, pgquery.RecordCurrentChangePriorApprovalParams{OrganizationID: s.organization, ChangeID: change, VersionID: version})
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	after, err := s.Change(ctx, change)
	if err != nil {
		return nil, false, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, false, corestore.ErrRevisionConflict
	}
	slices.SortFunc(result, func(a, b eventstore.Evidence) int {
		if a.Sequence < b.Sequence {
			return -1
		}
		if a.Sequence > b.Sequence {
			return 1
		}
		return 0
	})
	result = slices.CompactFunc(result, func(a, b eventstore.Evidence) bool { return a.Sequence == b.Sequence })
	return result, prior, nil
}
