package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeMutationPreparationKey struct{}

type changeMutationPreparation struct {
	change           tracker.ChangeRequest
	version          tracker.ChangeVersion
	publication      *preparedChangeVersion
	quality          *preparedQualityEscape
	review           tracker.ReviewChange
	reviewFields     []cloudContentField
	record           tracker.ChangeReview
	storedReview     tracker.ChangeReview
	reviewRaw        string
	reviewSlots      map[string][]attributionSlot
	sessionMachineID tracker.MachineID
	snapshot         changeMutationSnapshot
	sourceAttempt    string
	lease            tracker.LeaseID
	promote          bool
	evaluatedAt      time.Time
	promotionUntil   time.Time
	outputs          map[string]attachment.ArchiveReference
}

type changeMutationSnapshot struct {
	change, policy, rules, sourceRun, currentRun     string
	issueRevision, contentRevision, evidenceSequence int64
}

func preparedChangeMutation(ctx context.Context) *changeMutationPreparation {
	prepared, ok := ctx.Value(changeMutationPreparationKey{}).(*changeMutationPreparation)
	if !ok {
		return nil
	}
	return prepared
}

func readChangeMutationSnapshot(ctx context.Context, q nativeQueryer, scope nativeScope, item, change, sourceAttempt string, lease tracker.LeaseID) (changeMutationSnapshot, error) {
	var result changeMutationSnapshot
	err := q.QueryRowContext(ctx, `SELECT c.record_json,i.revision,
(SELECT content_settings_revision FROM projects WHERE organization_id=c.organization_id AND id=c.project_id),
COALESCE((SELECT policy_id FROM project_policies WHERE scope=?),''),
COALESCE((SELECT policy_json FROM change_review_policies WHERE organization_id=? AND project_id=?),''),
COALESCE((SELECT sequence FROM change_evidence WHERE change_id=c.id ORDER BY sequence DESC LIMIT 1),0),
COALESCE((SELECT COALESCE(NULLIF(payload_object_key,''),data_json) FROM native_attempts WHERE id=? AND organization_id=c.organization_id AND project_id=c.project_id AND work_item_id=c.work_item_id),''),
COALESCE((SELECT COALESCE(NULLIF(payload_object_key,''),data_json) FROM native_attempts WHERE lease_id=? AND organization_id=c.organization_id AND project_id=c.project_id AND work_item_id=c.work_item_id),'')
FROM change_requests c JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id
WHERE c.organization_id=? AND c.project_id=? AND c.id=? AND EXISTS(SELECT 1 FROM change_issue_links WHERE change_id=c.id AND work_item_id=?)`,
		string(scope.organization)+"/"+string(scope.project), scope.organization, scope.project, sourceAttempt, lease, scope.organization, scope.project, change, item).
		Scan(&result.change, &result.issueRevision, &result.contentRevision, &result.policy, &result.rules, &result.evidenceSequence, &result.sourceRun, &result.currentRun)
	return result, err
}

func recheckChangeMutation(ctx context.Context, tx *sql.Tx, scope nativeScope, options nativeCommandOptions) error {
	prepared := preparedChangeMutation(ctx)
	if prepared == nil {
		return nil
	}
	current, err := readChangeMutationSnapshot(ctx, tx, scope, options.Item, options.ChangeID, prepared.sourceAttempt, prepared.lease)
	if err != nil {
		return err
	}
	if current != prepared.snapshot {
		if prepared.quality != nil {
			return nativeConflict(prepared.change.Revision)
		}
		change, err := readChange(ctx, tx, scope, options.Item, options.ChangeID)
		if err != nil {
			return err
		}
		return nativeConflict(change.Revision)
	}
	if prepared.quality != nil {
		return recheckQualityEscape(ctx, tx, scope, prepared)
	}
	return nil
}

func prepareChangePromotion(ctx context.Context, q nativeQueryer, scope nativeScope, prepared *changeMutationPreparation, now time.Time) error {
	if prepared.publication == nil && prepared.review.Decision != "approved" {
		return nil
	}
	current, err := readChangeSummaryPolicy(ctx, q, scope)
	if err != nil {
		return err
	}
	detail := tracker.ChangeDetail{Change: prepared.change}
	var version tracker.ChangeVersion
	if prepared.publication != nil {
		version = prepared.publication.version
		detail.Change.CurrentVersion = version.ID
	} else {
		version = prepared.version
		detail.Reviews, err = readClaimReviews(ctx, q, detail.Change.ID, version.ID)
		if err != nil {
			return err
		}
		detail.Reviews = append(detail.Reviews, tracker.ChangeReview{VersionID: version.ID, Decision: prepared.review.Decision, Actor: scope.actor(), Validator: prepared.review.Validator, ImplementationReview: prepared.review.ImplementationReview, SecurityAudit: prepared.review.SecurityAudit})
		for _, expected := range version.Checks {
			checks, err := changeRows[tracker.ChangeCheck](ctx, q, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND version_id=? AND kind='check' AND source_key=?", detail.Change.ID, version.ID, expected.CheckRunID)
			if err != nil {
				return err
			}
			detail.Checks = append(detail.Checks, checks...)
			for _, result := range checks {
				expires := result.CompletedAt.Add(time.Duration(expected.MaxAgeSeconds) * time.Second)
				if prepared.promotionUntil.IsZero() || expires.Before(prepared.promotionUntil) {
					prepared.promotionUntil = expires
				}
			}
		}
	}
	detail.Versions = []tracker.ChangeVersion{version}
	prepared.promote = current.summarize(detail, now, false).Status == "reviewed"
	prepared.evaluatedAt = now
	return nil
}

func (s *Service) promotePreparedChange(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest) error {
	now, err := s.database.currentTime()
	if err != nil {
		return err
	}
	prepared := preparedChangeMutation(ctx)
	if prepared == nil {
		return promoteReviewedChange(ctx, tx, scope, change, now)
	}
	if !prepared.promote || now.Before(prepared.evaluatedAt) || !prepared.promotionUntil.IsZero() && !now.Before(prepared.promotionUntil) {
		return nil
	}
	return promoteReviewedIssue(ctx, tx, scope, change, now)
}
