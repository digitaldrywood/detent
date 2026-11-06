package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
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
		return nil, nativePublicInvalid("review decision is invalid")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || len(request.Body) > 64<<10 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes and a review body up to 64 KiB are required")
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
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
		return nil, nativeNotFound()
	}
	if request.ExpectedRevision != 0 && request.ExpectedRevision != change.Revision || request.ExpectedVersionID != "" && request.ExpectedVersionID != versionID || request.Decision == "approved" && change.CurrentVersion != versionID || change.CurrentLanding() != nil {
		return nil, nativeConflict(change.Revision)
	}
	versionRow, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, versionID)
	if err != nil {
		return nil, projectRecordError(err)
	}
	version, err := eventstore.NewContent(s.runDetailStore()).Version(ctx, versionRow)
	if err != nil {
		return nil, err
	}
	prepared, sessionMachine, err := s.prepareValidatorReviewRecord(ctx, scope, change, version, versionRow, request)
	if err != nil {
		return nil, err
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
	if request.Decision == "changes_requested" && len(review.IndependentFailures()) != 0 {
		return nil, workItemStorageUnavailable()
	}
	raw, err := json.Marshal(review)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	var approvedPolicyRows currentChangePolicyRows
	var approval ChangeVersionApproval
	if request.Decision == "approved" || prepared != nil || request.Decision == "changes_requested" && versionID == change.CurrentVersion {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			approvedPolicyRows, err = readCurrentChangePolicyRows(ctx, r, scope)
			return err
		}); err != nil {
			return nil, projectRecordError(err)
		}
		approval, err = hydrateCurrentChangeVersionApproval(ctx, content, scope, approvedPolicyRows, "")
		if err != nil {
			return nil, projectRecordError(err)
		}
		policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(&version, approval.Policy, approval.ReviewPolicy)
		if policyID != version.Policy.ID || reviewPolicyID != version.ReviewPolicy.ID {
			return nil, policyMismatch("Current policy changed; the reviewed version must be republished")
		}
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := validateChangeRecordContent(ctx, r, scope, changeReviewContentFields(request), nil, false)
		return err
	}); err != nil {
		return nil, projectRecordError(err)
	}
	workflowPlan, err := s.prepareChangeReviewWorkflow(ctx, scope, change, version, review, approval, now)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, nativeConflict(change.Revision)
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
			bodyKey, err := s.prepareWorkItemField(ctx, scope.organization, bodyRaw)
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
			instructions = &preparedInstructions{item: itemRow, comment: corestore.Comment{OrganizationID: scope.organization, ID: comment.ID, ProjectID: scope.project, WorkItemID: itemRow.NativeID, Revision: 1, Sequence: itemRow.EventSequence + 2, BodyObjectKey: bodyKey, ActorKind: kind, ActorPrincipalID: principal, CreatedAt: review.CreatedAt, UpdatedAt: review.CreatedAt}, version: commentVersion, event: commentEvent}
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
		return nil, projectRecordError(err)
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			var transition *changeWorkflowTransition
			if workflowPlan != nil {
				transition = workflowPlan.transition
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
			if err := bindChangeRecordContent(ctx, w, scope, item, changeReviewContentFields(request), nil); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return nativeConflict(change.Revision)
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
					return policyMismatch("Current policy changed; the reviewed version must be republished")
				}
			}
			if err := s.lockValidatorReviewRecord(ctx, w, scope, change, request, prepared); err != nil {
				return err
			}
			if request.Bundle != nil {
				ref, err := w.ArtifactReference(ctx, scope.project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
				if err != nil {
					return err
				}
				clock, ok := w.(runnersession.Reader)
				if !ok {
					return nativeRunEventStorageUnavailable()
				}
				at, err := clock.RunnerSessionTime(ctx)
				if err != nil {
					return err
				}
				if ref.SHA256 != request.Bundle.SHA256 || !changerequest.ReviewBundleMatches(version, ref) || !at.Before(ref.ExpiresAt) || ref.Availability != "available" {
					return nativePublicInvalid("review bundle is unavailable")
				}
			}
			workflowCurrent, err := s.recheckChangeReviewWorkflow(ctx, w, scope, workflowPlan, currentChange)
			if err != nil {
				return err
			}
			if _, err := w.AppendEvidence(ctx, evidence); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return err
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
			if transitionReady && workflowCurrent {
				if err := s.applyChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return err
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
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nil, nativeConflict(change.Revision)
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		break
	}
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return raw, nil
}
