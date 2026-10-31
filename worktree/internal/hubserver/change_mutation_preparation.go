package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
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

func (s *Service) prepareChangeMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, requestHash string) (context.Context, error) {
	switch request := input.(type) {
	case tracker.PublishChangeVersion, tracker.ReviewChange:
	case tracker.DiscussChange:
		if request.Escape == nil {
			return ctx, nil
		}
	default:
		return ctx, nil
	}
	if options.ChangeID == "" {
		return ctx, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	tx, err := s.database.reader.BeginTx(readCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ctx, err
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(readCtx, tx, &scope); err != nil {
		return ctx, err
	}
	if _, found, err := nativeCommandReceipt(readCtx, tx, scope, options.OperationID, command.IdempotencyKey, requestHash); err != nil || found {
		return ctx, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return ctx, err
	}
	if err := requireRunnerAuthority(readCtx, tx, scope, now); err != nil {
		return ctx, err
	}
	if scope.credential.Scope == apiScopeWorker && options.RequireLease {
		if err := requireNativeMutationLease(readCtx, tx, scope, options.Item, command, now); err != nil {
			return ctx, err
		}
	}
	if err := s.database.requireHostedFeature(readCtx, tx, options.Feature, now); err != nil {
		return ctx, err
	}
	prepared := &changeMutationPreparation{lease: command.LeaseID}
	switch request := input.(type) {
	case tracker.DiscussChange:
		prepared.change, err = readChange(readCtx, tx, scope, options.Item, options.ChangeID)
		if err != nil {
			return ctx, err
		}
		if request.VersionID != "" {
			if _, err := readChangeVersion(readCtx, tx, prepared.change.ID, request.VersionID); err != nil {
				return ctx, err
			}
		}
		prepared.quality = &preparedQualityEscape{version: request.VersionID, occurrence: request.Escape.OccurrenceID}
		if err := validateQualityEscape(readCtx, tx, scope, prepared.change, request, now, prepared.quality); err != nil {
			return ctx, err
		}
		prepared.quality.snapshot, err = readQualityEscapeSnapshot(readCtx, tx, scope, options.ChangeID, prepared.quality)
		if err != nil {
			return ctx, err
		}
	case tracker.PublishChangeVersion:
		prepared.publication, err = s.prepareChangeVersion(readCtx, tx, scope, options.Item, options.ChangeID, request, now)
		if err != nil {
			return ctx, err
		}
		prepared.change = prepared.publication.change
		prepared.sourceAttempt = request.AttemptID
	case tracker.ReviewChange:
		prepared.review = request
		prepared.reviewFields = changeReviewContentFields(request)
		prepared.change, prepared.sessionMachineID, err = validateChangeReview(readCtx, tx, scope, options.Item, options.ChangeID, options.VersionID, prepared.review, now)
		if err != nil {
			return ctx, err
		}
		prepared.version, err = readChangeVersion(readCtx, tx, options.ChangeID, options.VersionID)
		if err != nil {
			return ctx, err
		}
		prepared.sourceAttempt = prepared.version.AttemptID
		prepared.record = tracker.ChangeReview{SecurityAudit: request.SecurityAudit, SessionMachineID: prepared.sessionMachineID, ImplementationReview: request.ImplementationReview, Validator: request.Validator, ID: payloadNativeID(ctx, "review"), VersionID: options.VersionID, Decision: request.Decision, Body: request.Body, Actor: scope.actor(), CreatedAt: payloadTimestamp(ctx, now)}
		prepared.reviewSlots = reviewAttributionSlots(&prepared.record)
		raw, err := json.Marshal(prepared.record)
		if err != nil {
			return ctx, err
		}
		if err := json.Unmarshal(raw, &prepared.storedReview); err != nil {
			return ctx, err
		}
	}
	if err := prepareChangePromotion(readCtx, tx, scope, prepared, now); err != nil {
		return ctx, err
	}
	prepared.snapshot, err = readChangeMutationSnapshot(readCtx, tx, scope, options.Item, options.ChangeID, prepared.sourceAttempt, prepared.lease)
	if err != nil {
		return ctx, err
	}
	if err := tx.Rollback(); err != nil {
		return ctx, err
	}
	prepared.outputs = make(map[string]attachment.ArchiveReference)
	commands := map[string][]gate.CommandResult{}
	if prepared.publication != nil && prepared.publication.version.Validation != nil {
		commands["validation"] = []gate.CommandResult{*prepared.publication.version.Validation}
	}
	for role, result := range map[string]*gate.ValidatorResult{"validator": prepared.review.Validator, "implementation_review": prepared.review.ImplementationReview} {
		if result != nil {
			commands[role] = result.Commands
		}
	}
	if _, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		for role, list := range commands {
			for position, result := range list {
				if result.Output == "" {
					continue
				}
				ref, err := prepareChangeContent(ctx, string(scope.organization), []byte(result.Output))
				if err != nil {
					return ctx, err
				}
				prepared.outputs[fmt.Sprintf("%s.%d", role, position)] = ref
			}
		}
	}
	if prepared.publication == nil && prepared.quality == nil {
		if _, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
			for _, result := range []*gate.ValidatorResult{prepared.storedReview.Validator, prepared.storedReview.ImplementationReview} {
				if result != nil {
					for i := range result.Commands {
						result.Commands[i].Output = ""
					}
				}
			}
		}
		prepared.reviewRaw, err = marshalNative(prepared.storedReview)
		if err != nil {
			return ctx, err
		}
	}
	response := any(prepared.record)
	if prepared.publication != nil {
		if _, err := prepareChangeContent(ctx, string(scope.organization), []byte(prepared.publication.raw)); err != nil {
			return ctx, err
		}
		response = prepared.publication.version
	}
	responseRaw, err := json.Marshal(response)
	if err != nil {
		return ctx, err
	}
	if _, err := prepareChangeContent(ctx, string(scope.organization), responseRaw); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, changeMutationPreparationKey{}, prepared), nil
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
