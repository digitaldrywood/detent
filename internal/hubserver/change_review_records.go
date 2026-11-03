package hubserver

import (
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
	scope := nativeRequestScope(c)
	if request.Decision != "approved" && request.Decision != "changes_requested" && request.Decision != "commented" {
		return s.nativeAPIError(c, nativePublicInvalid("review decision is invalid"))
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || len(request.Body) > 64<<10 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes and a review body up to 64 KiB are required"))
	}
	item, changeID, versionID := c.Param("item"), c.Param("change"), c.Param("version")
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if found {
		return c.JSONBlob(http.StatusOK, replay)
	}
	change, err := s.readChangeRecord(ctx, scope, item, changeID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.ExpectedRevision != 0 && request.ExpectedRevision != change.Revision || request.ExpectedVersionID != "" && request.ExpectedVersionID != versionID || request.Decision == "approved" && change.CurrentVersion != versionID || change.CurrentLanding() != nil {
		return s.nativeAPIError(c, nativeConflict(change.Revision))
	}
	versionRow, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, versionID)
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	version, err := eventstore.NewContent(s.runDetailStore()).Version(ctx, versionRow)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	prepared, sessionMachine, err := s.prepareValidatorReviewRecord(ctx, scope, change, version, versionRow, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}

	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Bundle != nil {
		if err := s.validateReviewBundleRecord(ctx, scope, change, version, *request.Bundle, now); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	review := tracker.ChangeReview{SessionMachineID: sessionMachine, ImplementationReview: request.ImplementationReview, Validator: request.Validator, SecurityAudit: request.SecurityAudit, ID: payloadNativeID(ctx, "review"), VersionID: versionID, Decision: request.Decision, Body: request.Body, Actor: scope.actor(), CreatedAt: payloadTimestamp(ctx, now)}
	raw, err := json.Marshal(review)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	var approvedPolicyRows currentChangePolicyRows
	if request.Decision == "approved" || prepared != nil {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			approvedPolicyRows, err = readCurrentChangePolicyRows(ctx, r, scope)
			return err
		}); err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		approval, err := hydrateCurrentChangeVersionApproval(ctx, content, scope, approvedPolicyRows, "")
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(&version, approval.Policy, approval.ReviewPolicy)
		if policyID != version.Policy.ID || reviewPolicyID != version.ReviewPolicy.ID {
			return s.nativeAPIError(c, policyMismatch("Current policy changed; the reviewed version must be republished"))
		}
	}
	evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: scope.organization, ChangeID: change.ID, VersionID: new(versionID), Kind: "review", CreatedAt: review.CreatedAt}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change.review", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: review.CreatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: versionID}}})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
	if err != nil {
		return s.nativeAPIError(c, err)
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
			return s.nativeAPIError(c, err)
		}
		issue, err := s.hydrateWorkItemRecord(ctx, itemRow, itemVersion)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if !issue.Terminal && !issue.Archived {
			body := reviewInstructions(change, review)
			bodyRaw, err := json.Marshal(body)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			bodyKey, err := s.prepareWorkItemField(ctx, scope.organization, bodyRaw)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			comment := tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemRow.NativeID, ID: payloadNativeID(ctx, "cmt"), Revision: 1, Body: body, Actor: scope.actor(), CreatedAt: review.CreatedAt, UpdatedAt: review.CreatedAt}
			commentRaw, err := json.Marshal(comment)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			commentVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: itemRow.NativeID, RecordID: comment.ID, Revision: 1}, commentRaw)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			commentEventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: itemRow.NativeID, Type: "comment.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: review.CreatedAt, Data: tracker.CollaborationData{CommentID: comment.ID}})
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			commentEvent, err := content.PrepareEvent(ctx, scope.organization, commentEventRaw)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			kind, principal := scope.actor().Kind, scope.actor().PrincipalID
			instructions = &preparedInstructions{item: itemRow, comment: corestore.Comment{OrganizationID: scope.organization, ID: comment.ID, ProjectID: scope.project, WorkItemID: itemRow.NativeID, Revision: 1, Sequence: itemRow.EventSequence + 2, BodyObjectKey: bodyKey, ActorKind: kind, ActorPrincipalID: principal, CreatedAt: review.CreatedAt, UpdatedAt: review.CreatedAt}, version: commentVersion, event: commentEvent}
		}
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var current eventstore.Change
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		current, err = r.Change(ctx, change.ID)
		return err
	}); err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
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
			if request.Decision == "approved" || prepared != nil {
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
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			if found {
				return c.JSONBlob(http.StatusOK, replay)
			}
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return s.nativeAPIError(c, nativeConflict(change.Revision))
		}
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		break
	}
	return c.JSONBlob(http.StatusOK, raw)
}
