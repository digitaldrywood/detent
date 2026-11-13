package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) changeReviewRecordMutation(c echo.Context, request tracker.ReviewChange) error {
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	raw, err := s.reviewChangeRecordCommand(ctx, nativeRequestScope(c), c.Param("item"), c.Param("change"), c.Param("version"), operation, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) reviewChangeRecordCommand(ctx context.Context, scope nativeScope, item, changeID, versionID, operation string, request tracker.ReviewChange) (json.RawMessage, error) {
	if request.Decision != "approved" && request.Decision != "changes_requested" && request.Decision != "commented" {
		return nil, hubapi.NativePublicInvalid("review decision is invalid")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || len(request.Body) > 64<<10 {
		refusal := hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes and a review body up to 64 KiB are required")
		if len(request.Body) > 64<<10 && scope.credential.Scope == hubapi.APIScopeWorker && request.Validator != nil && request.ImplementationReview == nil && request.SecurityAudit == nil {
			return nil, nativeRejection(refusal, "acceptance_validation", "body")
		}
		return nil, refusal
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	submittedContent := changeReviewContentFields(request)
	for _, result := range []**gate.ValidatorResult{&request.Validator, &request.ImplementationReview} {
		if *result != nil {
			normalized := **result
			normalized.Findings = slices.Clone(normalized.Findings)
			normalized.Commands = slices.Clone(normalized.Commands)
			gate.NormalizeAttribution(&normalized)
			*result = &normalized
		}
	}
	if request.SecurityAudit != nil {
		normalized := *request.SecurityAudit
		normalized.Run.Findings = slices.Clone(normalized.Run.Findings)
		securityaudit.NormalizeAttribution(&normalized.Run)
		request.SecurityAudit = &normalized
	}
prepareReview:
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	replay, found, err := s.changeOperationRecordReceipt(ctx, scope, item, changeID, versionID, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	change, err := s.readChangeRecord(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	if change.WorkItemID != tracker.NativeWorkItemID(item) {
		return nil, hubapi.NativeNotFound()
	}
	if request.ExpectedRevision != 0 && request.ExpectedRevision != change.Revision || request.ExpectedVersionID != "" && request.ExpectedVersionID != versionID || request.Decision == "approved" && change.CurrentVersion != versionID || change.CurrentLanding() != nil {
		return nil, hubapi.NativeConflict(change.Revision)
	}
	versionRow, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, versionID)
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	version, err := eventstore.NewContent(s.runDetailStore()).Version(ctx, versionRow)
	if err != nil {
		return nil, err
	}
	prepared, sessionMachine, err := s.prepareValidatorReviewRecord(ctx, scope, change, version, versionRow, request)
	if err != nil {
		return nil, fmt.Errorf("prepare validator review: %w", err)
	}

	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if request.Bundle != nil {
		if err := s.validateReviewBundleRecord(ctx, scope, change, version, *request.Bundle, now); err != nil {
			return nil, err
		}
	}
	review := tracker.ChangeReview{SessionMachineID: sessionMachine, ImplementationReview: request.ImplementationReview, Validator: request.Validator, SecurityAudit: request.SecurityAudit, ID: payloadNativeID(ctx, "review"), VersionID: versionID, Decision: request.Decision, Body: request.Body, Actor: scope.actor(), CreatedAt: payloadTimestamp(ctx, now)}
	var independent *preparedIndependentRepairs
	if request.Decision == "changes_requested" && review.VersionID == change.CurrentVersion {
		independent, err = s.prepareIndependentRepairs(ctx, scope, change.WorkItemID, review.IndependentFailures(), change.ID+"/"+review.ID, now)
		if err != nil {
			return nil, err
		}
		if independent != nil {
			review.RepairDependencies, review.RepairRefusal = independent.dependencies, independent.refusal
		}
	}
	raw, err := json.Marshal(review)
	if err != nil {
		return nil, err
	}
	var preparedContent *reviewDetailStore
	var detailStore attachment.DetailStore
	if owner := s.runDetailStore(); owner != nil {
		preparedContent = &reviewDetailStore{DetailStore: owner, service: s, prepared: make(map[string]attachment.ArchiveReference)}
		detailStore = preparedContent
	}
	content := eventstore.NewContent(detailStore)
	var approvedPolicyRows currentChangePolicyRows
	var approval ChangeVersionApproval
	if request.Decision == "approved" || prepared != nil || request.Decision == "changes_requested" && versionID == change.CurrentVersion {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			approvedPolicyRows, err = readCurrentChangePolicyRows(ctx, r, scope)
			return err
		}); err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		approval, err = hydrateCurrentChangeVersionApproval(ctx, content, scope, approvedPolicyRows, "")
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(&version, approval.Policy, approval.ReviewPolicy)
		if policyID != version.Policy.ID || reviewPolicyID != version.ReviewPolicy.ID {
			return nil, hubapi.PolicyMismatch("Current policy changed; the reviewed version must be republished")
		}
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := validateChangeRecordContent(ctx, r, scope, submittedContent, nil, "", false)
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	workflowPlan, err := s.prepareChangeReviewWorkflow(ctx, scope, change, version, review, approval, now)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, hubapi.NativeConflict(change.Revision)
	}
	if err != nil {
		return nil, err
	}
	evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: scope.organization, ChangeID: change.ID, VersionID: new(versionID), Kind: "review", CreatedAt: review.CreatedAt}, raw)
	if err != nil {
		return nil, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change.review", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: review.CreatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: versionID}}})
	if err != nil {
		return nil, err
	}
	event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
	if err != nil {
		return nil, err
	}
	type preparedInstructions struct {
		item    corestore.WorkItem
		comment corestore.Comment
		version eventstore.CollaborationVersion
		event   eventstore.Event
	}
	var instructions *preparedInstructions
	if request.Decision == "changes_requested" && versionID == change.CurrentVersion {
		itemRow, itemVersion, err := s.readWorkItemRecord(ctx, scope, item)
		if err != nil {
			return nil, err
		}
		issue, err := s.hydrateWorkItemRecord(ctx, itemRow, itemVersion)
		if err != nil {
			return nil, err
		}
		if !issue.Terminal && !issue.Archived {
			body := reviewInstructions(change, review)
			bodyRaw, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			bodyRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, bodyRaw)
			if err != nil {
				return nil, err
			}
			comment := tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemRow.NativeID, ID: payloadNativeID(ctx, "cmt"), Revision: 1, Sequence: itemRow.EventSequence + 2, Body: body, Actor: scope.actor(), CreatedAt: review.CreatedAt, UpdatedAt: review.CreatedAt}
			commentRaw, err := json.Marshal(comment)
			if err != nil {
				return nil, err
			}
			commentVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemRow.NativeID, RecordID: comment.ID, Revision: 1}, commentRaw)
			if err != nil {
				return nil, err
			}
			commentEventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: itemRow.NativeID, Type: "comment.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: review.CreatedAt, Data: tracker.CollaborationData{CommentID: comment.ID}})
			if err != nil {
				return nil, err
			}
			commentEvent, err := content.PrepareEvent(ctx, scope.organization, commentEventRaw)
			if err != nil {
				return nil, err
			}
			kind, principal := scope.actor().Kind, scope.actor().PrincipalID
			instructions = &preparedInstructions{item: itemRow, comment: corestore.Comment{OrganizationID: scope.organization, ID: comment.ID, ProjectID: scope.project, WorkItemID: itemRow.NativeID, Revision: 1, Sequence: itemRow.EventSequence + 2, BodyObjectKey: bodyRef.PayloadObjectKey, ActorKind: kind, ActorPrincipalID: principal, CreatedAt: review.CreatedAt, UpdatedAt: review.CreatedAt}, version: commentVersion, event: commentEvent}
		}
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, raw)
	if err != nil {
		return nil, err
	}
	var current eventstore.Change
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		current, err = r.Change(ctx, change.ID)
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if prepared != nil {
				if _, err := w.RunnerForLease(ctx, string(request.LeaseID)); err != nil {
					return fmt.Errorf("lock review runner: %w", err)
				}
			}
			var transition *changeWorkflowTransition
			if workflowPlan != nil {
				transition = workflowPlan.transition
			}
			if independent != nil {
				refusal, err := s.checkIndependentRepairs(ctx, w, scope, independent)
				if err != nil {
					return err
				}
				if refusal != independent.refusal {
					return corestore.ErrSerializationFailure
				}
			}
			transitionReady, err := s.lockChangeWorkflowTransition(ctx, w, scope, transition)
			if err != nil {
				return err
			}
			if _, err := s.lockChangeRecordOperation(ctx, w, scope, item, changeID, versionID); err != nil {
				return err
			}
			var lockedItem corestore.WorkItem
			if instructions != nil {
				lockedItem, err = w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
				if err != nil {
					return err
				}
				if lockedItem.Revision != instructions.item.Revision || lockedItem.EventSequence != instructions.item.EventSequence {
					return corestore.ErrRevisionConflict
				}
			}
			if err := bindChangeRecordContent(ctx, w, scope, item, changeReviewContentFields(request), nil, ""); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return hubapi.NativeConflict(change.Revision)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			currentChange, err := w.LockChange(ctx, change.ID)
			if err != nil {
				return err
			}
			if currentChange.Revision != current.Revision || currentChange.EvidenceSequence != current.EvidenceSequence || currentChange.CurrentVersionID == nil || *currentChange.CurrentVersionID != change.CurrentVersion {
				return corestore.ErrRevisionConflict
			}
			currentVersion, err := w.Version(ctx, change.ID, versionID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(currentVersion, versionRow) {
				return corestore.ErrRevisionConflict
			}
			if request.Decision == "approved" || prepared != nil || request.Decision == "changes_requested" && versionID == change.CurrentVersion {
				rows, err := readCurrentChangePolicyRows(ctx, w, scope)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(rows, approvedPolicyRows) {
					return hubapi.PolicyMismatch("Current policy changed; the reviewed version must be republished")
				}
			}
			if err := s.lockValidatorReviewRecord(ctx, w, scope, change, request, prepared); err != nil {
				return fmt.Errorf("lock validator review: %w", err)
			}
			if request.Bundle != nil {
				ref, err := w.ArtifactReference(ctx, scope.project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
				if err != nil {
					return err
				}
				clock, ok := w.(runnersession.Reader)
				if !ok {
					return hubapi.NativeRunEventStorageUnavailable()
				}
				at, err := clock.RunnerSessionTime(ctx)
				if err != nil {
					return err
				}
				if ref.SHA256 != request.Bundle.SHA256 || !changerequest.ReviewBundleMatches(version, ref) || !at.Before(ref.ExpiresAt) || ref.Availability != "available" {
					return hubapi.NativePublicInvalid("review bundle is unavailable")
				}
			}
			workflowCurrent, err := s.recheckChangeReviewWorkflow(ctx, w, scope, workflowPlan, currentChange)
			if err != nil {
				return fmt.Errorf("recheck review workflow: %w", err)
			}
			if preparedContent != nil {
				keys := make([]string, 0, len(preparedContent.prepared))
				for key := range preparedContent.prepared {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				for _, key := range keys {
					if err := s.registerChangeContentRecord(ctx, w, string(scope.organization), preparedContent.prepared[key], true); err != nil {
						return err
					}
				}
			}
			if _, err := w.AppendEvidence(ctx, evidence); err != nil {
				return fmt.Errorf("append review evidence: %w", err)
			}
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return fmt.Errorf("append review event: %w", err)
			}
			if instructions != nil {
				if _, err := w.CreateComment(ctx, instructions.comment); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, instructions.version); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, instructions.event); err != nil {
					return err
				}
			}
			if err := s.applyIndependentRepairs(ctx, w, scope, independent); err != nil {
				return err
			}
			if transitionReady && workflowCurrent {
				if err := s.applyChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return fmt.Errorf("apply review workflow: %w", err)
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.changeOperationRecordReceipt(ctx, scope, item, changeID, versionID, operation, request.IdempotencyKey, hash)
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
			}
			if independent != nil {
				goto prepareReview
			}
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nil, hubapi.NativeConflict(change.Revision)
		}
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		break
	}
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return raw, nil
}

type reviewDetailStore struct {
	attachment.DetailStore
	service  *Service
	prepared map[string]attachment.ArchiveReference
}

func (store *reviewDetailStore) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if store.DetailStore == nil {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	expected, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if previous, found := store.prepared[expected.Key]; found {
		if previous != expected {
			return attachment.ArchiveReference{}, artifact.ErrIntegrity
		}
		return previous, nil
	}
	if err := store.service.checkChangeContentAllowance(ctx, organization, store.prepared, expected); err != nil {
		return attachment.ArchiveReference{}, err
	}
	actual, err := (runEventDetailStore{DetailStore: store.DetailStore}).StoreDetail(ctx, organization, raw)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if actual != expected {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	store.prepared[expected.Key] = actual
	return actual, nil
}
