package changeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *RecordMutations) ChangeRecordEvidenceMutation(c echo.Context, kind, versionID, sourceKey string, value any) error {
	if review, ok := value.(tracker.ReviewChange); ok && (review.Decision != "commented" || review.Validator != nil || review.ImplementationReview != nil || review.SecurityAudit != nil) {
		return s.deps.ChangeReviewRecordMutation(c, review)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	response, err := s.ChangeRecordEvidenceCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), c.Param("change"), operation, kind, versionID, sourceKey, value)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *RecordMutations) ChangeRecordEvidenceCommand(ctx context.Context, scope hubscope.NativeScope, item, changeID, operation, kind, versionID, sourceKey string, value any) (json.RawMessage, error) {
	ctx = hubcontent.IsolateContentContext(s.deps.RunDetailContext(ctx))
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	var command tracker.Mutation
	switch record := value.(type) {
	case tracker.DiscussChange:
		command = record.Mutation
	case tracker.ReviewChange:
		command = record.Mutation
	default:
		return nil, hubapi.NativePublicInvalid("An idempotency key is required")
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(value)
	if err != nil {
		return nil, err
	}
	replay, found, err := s.deps.ChangeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	change, err := s.deps.ReadChangeRecord(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	if versionID != "" {
		if _, err := s.deps.ReadChangeVersionRecord(ctx, scope.Organization, change.ID, versionID); err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
	}
	now, err := s.deps.CurrentTime()
	if err != nil {
		return nil, err
	}
	evidence := eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: change.ID, Kind: kind, CreatedAt: hubcontent.PayloadTimestamp(ctx, now)}
	var raw []byte
	var quality *changerules.PreparedQualityEscape
	var qualityEscape tracker.QualityEscape
	var contentFields []changefiles.ContentField
	switch record := value.(type) {
	case tracker.DiscussChange:
		if strings.TrimSpace(record.Body) == "" || len(record.Body) > 64<<10 {
			return nil, hubapi.NativePublicInvalid("Discussion requires 1 byte to 64 KiB")
		}
		contentFields = []changefiles.ContentField{{Name: "body", Value: record.Body}}
		if record.Escape != nil {
			qualityEscape = *record.Escape
			quality, err = s.deps.PrepareQualityEscapeTenant(ctx, scope, change, record, now)
			if err != nil {
				return nil, hubapi.ProjectRecordError(err)
			}
		}
		if record.VersionID != "" {
			evidence.VersionID = new(record.VersionID)
		}
		if record.Provenance != nil {
			if err := s.deps.ValidateNativeProvenance(scope, record.Provenance); err != nil {
				return nil, err
			}
			sourceKey = record.Provenance.Provider + ":" + record.Provenance.ExternalID
		}
		discussion := tracker.ChangeDiscussion{ID: hubcontent.PayloadNativeID(ctx, "cmt"), VersionID: record.VersionID, Body: record.Body, Actor: scope.Actor(), Provenance: record.Provenance, CreatedAt: evidence.CreatedAt, Escape: record.Escape}
		raw, err = json.Marshal(discussion)
	case tracker.ReviewChange:
		if record.Decision != "commented" || record.ExpectedRevision != 0 && record.ExpectedRevision != change.Revision || record.ExpectedVersionID != "" && record.ExpectedVersionID != versionID || len(record.Body) > 64<<10 {
			return nil, hubapi.NativeWorkItemStorageUnavailable()
		}
		if record.Validator != nil || record.ImplementationReview != nil || record.SecurityAudit != nil || record.Bundle != nil {
			return nil, hubapi.NativeWorkItemStorageUnavailable()
		}
		evidence.VersionID = new(versionID)
		contentFields = changefiles.ReviewContentFields(record)
		review := tracker.ChangeReview{ID: hubcontent.PayloadNativeID(ctx, "review"), VersionID: versionID, Decision: record.Decision, Body: record.Body, Actor: scope.Actor(), CreatedAt: evidence.CreatedAt}
		raw, err = json.Marshal(review)
	default:
		return nil, hubapi.NativePublicInvalid("Unsupported change evidence")
	}
	if err != nil {
		return nil, err
	}
	if sourceKey != "" {
		evidence.SourceKey = new(sourceKey)
	} else if err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := changefiles.ValidateRecordContent(ctx, r, scope, contentFields, nil, "", false, s.deps.AttachmentReferences)
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	evidence, err = eventstore.NewContent(s.deps.RunDetailStore()).PrepareEvidence(ctx, evidence, raw)
	if err != nil {
		return nil, err
	}
	var response []byte
	existingSource := false
	if sourceKey != "" {
		var old eventstore.Evidence
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			old, err = r.EvidenceBySource(ctx, change.ID, kind, sourceKey)
			return err
		})
		if err == nil {
			response, err = eventstore.NewContent(s.deps.RunDetailStore()).Evidence(ctx, old)
			existingSource = true
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return nil, hubapi.ProjectRecordError(err)
		}
	}
	if response == nil {
		response = raw
	}
	createdEvidence := !existingSource
	receipt, err := eventstore.NewContent(s.deps.RunDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	var event eventstore.Event
	if createdEvidence {
		eventValue := tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change." + kind, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: evidence.CreatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: versionID}}}
		eventRaw, err := json.Marshal(eventValue)
		if err != nil {
			return nil, err
		}
		event, err = eventstore.NewContent(s.deps.RunDetailStore()).PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
	}
	for {
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.deps.AuthorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, command.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return hubapi.NativeConflict(change.Revision)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			current, err := w.Change(ctx, change.ID)
			if err != nil {
				return err
			}
			if current.Revision != int64(change.Revision) || current.CurrentVersionID != nil && *current.CurrentVersionID != change.CurrentVersion {
				return corestore.ErrRevisionConflict
			}
			if quality != nil {
				if err := changerules.RecheckTenantQualityEscape(ctx, w, scope, change, qualityEscape, quality); err != nil {
					return err
				}
			}
			if createdEvidence {
				if err := changefiles.BindRecordContent(ctx, w, scope, string(change.WorkItemID), contentFields, nil, "", s.deps.AttachmentReferences); err != nil {
					return err
				}
				if _, err := w.AppendEvidence(ctx, evidence); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.deps.ChangeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
			}
			if sourceKey != "" {
				var old eventstore.Evidence
				oldErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					old, err = r.EvidenceBySource(ctx, change.ID, kind, sourceKey)
					return err
				})
				if oldErr == nil {
					response, err = eventstore.NewContent(s.deps.RunDetailStore()).Evidence(ctx, old)
					if err != nil {
						return nil, err
					}
					receipt, err = eventstore.NewContent(s.deps.RunDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
					if err != nil {
						return nil, err
					}
					createdEvidence = false
					continue
				}
				if !errors.Is(oldErr, corestore.ErrNotFound) {
					return nil, hubapi.ProjectRecordError(oldErr)
				}
			}
			continue
		}
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		break
	}
	return response, nil
}

func (s *RecordMutations) ChangeCheckRecordMutation(c echo.Context, request tracker.SubmitChangeCheck) error {
	ctx := hubcontent.IsolateContentContext(s.deps.RunDetailContext(c.Request().Context()))
	scope := hubscope.NativeRequestScope(c)
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	var approved eventstore.ReviewPolicy
	hostedCheck := s.deps.Hosted && scope.Credential.Hosted == nil && scope.Credential.Runner.RunnerID == ""
	if hostedCheck {
		approved, err = s.deps.RequireHostedChangeCheckPrincipal(ctx, scope)
		if err != nil {
			return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
		}
	}
	readReceipt := func() (json.RawMessage, bool, error) {
		if !hostedCheck {
			return s.deps.ChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
		}
		var receipt eventstore.Receipt
		err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if err := s.deps.RecheckHostedChangeCheckPrincipal(ctx, w, scope, approved); err != nil {
				return err
			}
			var err error
			receipt, err = w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, hubapi.ProjectRecordError(err)
		}
		if receipt.RequestHash != hash {
			return nil, false, hubapi.NativeConflict(0)
		}
		value, err := eventstore.NewContent(s.deps.RunDetailStore()).Receipt(ctx, receipt)
		return value, true, err
	}

	replay, found, err := readReceipt()
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if found {
		return c.JSONBlob(http.StatusOK, replay)
	}
	change, err := s.deps.ReadChangeRecord(ctx, scope, c.Param("item"), c.Param("change"))
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	versionRecord, err := s.deps.ReadChangeVersionRecord(ctx, scope.Organization, change.ID, c.Param("version"))
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	version, err := eventstore.NewContent(s.deps.RunDetailStore()).Version(ctx, versionRecord)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if err := changerequest.ValidateResult(version, request.ChangeCheckResult, scope.Credential.ID, scope.Credential.Scope == hubapi.APIScopeWorker); err != nil {
		if visualErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := changefiles.ValidateRecordContent(ctx, r, scope, nil, request.Evidence, "evidence", false, s.deps.AttachmentReferences)
			return err
		}); visualErr != nil {
			return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(visualErr))
		}
		return s.deps.NativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	now := s.deps.Clock().UTC()
	if now.IsZero() {
		return s.deps.NativeAPIError(c, s.deps.InvalidClock)
	}
	if request.CompletedAt.After(now) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("CI completion time cannot be in the future"))
	}
	check := tracker.ChangeCheck{ChangeCheckResult: request.ChangeCheckResult, VersionID: version.ID, Actor: scope.Actor(), ReceivedAt: hubcontent.PayloadTimestamp(ctx, now)}
	raw, err := json.Marshal(check)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: change.ID, Kind: "check", VersionID: new(version.ID), SourceKey: new(check.CheckRunID)}, raw)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	response := raw
	createdEvidence := true
	var existing eventstore.Evidence
	lookupErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		existing, err = r.EvidenceBySource(ctx, change.ID, "check", check.CheckRunID)
		return err
	})
	if lookupErr == nil {
		oldRaw, err := content.Evidence(ctx, existing)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		var old tracker.ChangeCheck
		if json.Unmarshal(oldRaw, &old) != nil {
			return s.deps.NativeAPIError(c, corestore.ErrInvalidCursor)
		}
		oldInput, err := json.Marshal(old.ChangeCheckResult)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		newInput, err := json.Marshal(check.ChangeCheckResult)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		if string(oldInput) != string(newInput) || old.Actor.PrincipalID != scope.Credential.ID {
			return s.deps.NativeAPIError(c, hubapi.NativeConflict(change.Revision))
		}
		response, createdEvidence = oldRaw, false
	} else if !errors.Is(lookupErr, corestore.ErrNotFound) {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(lookupErr))
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var historyEvent eventstore.Event
	if createdEvidence {
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change.check_submitted", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: check.ReceivedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID}}})
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		historyEvent, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
	}
	var policyRows hubrecords.CurrentChangePolicyRows
	var workflowPlan *changelanding.ReviewWorkflow
	if scope.Credential.Scope != hubapi.APIScopeWorker && version.ID == change.CurrentVersion {
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
			return err
		})
		if err != nil {
			return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		approval, err := s.deps.HydrateChangeSummaryPolicy(ctx, content, scope, policyRows)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		workflowPlan, err = s.deps.PrepareChangeCheckWorkflow(ctx, scope, change, version, check, approval, now)
		if err != nil {
			return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
		}
	}
	var transition *changelanding.WorkflowTransition
	if workflowPlan != nil {
		transition = workflowPlan.Transition
	}
	for {
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.deps.AuthorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			transitionReady, err := s.deps.LockChangeWorkflowTransition(ctx, w, scope, transition)
			if err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if hostedCheck {
				if err := s.deps.RecheckHostedChangeCheckPrincipal(ctx, w, scope, approved); err != nil {
					return err
				}
			}
			if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return hubapi.NativeConflict(change.Revision)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			workflowCurrent := false
			if workflowPlan != nil {
				current, err := w.LockChange(ctx, change.ID)
				if err != nil {
					return err
				}
				workflowCurrent, err = s.deps.RecheckChangeReviewWorkflow(ctx, w, scope, workflowPlan, current)
				if err != nil {
					return err
				}
				currentVersion, err := w.Version(ctx, change.ID, version.ID)
				if err != nil {
					return err
				}
				rows, err := hubrecords.ReadChangeSummaryPolicyRows(ctx, w, scope)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(rows, policyRows) || !reflect.DeepEqual(currentVersion, versionRecord) {
					return corestore.ErrRevisionConflict
				}
			}
			if createdEvidence {
				if err := changefiles.BindRecordContent(ctx, w, scope, string(change.WorkItemID), nil, request.Evidence, "evidence", s.deps.AttachmentReferences); err != nil {
					return err
				}
				if _, err := w.AppendEvidence(ctx, evidence); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, historyEvent); err != nil {
					return err
				}
			}
			if transitionReady && workflowCurrent {
				if err := s.deps.ApplyChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := readReceipt()
			if readErr != nil {
				return s.deps.NativeAPIError(c, readErr)
			}
			if found {
				return c.JSONBlob(http.StatusOK, replay)
			}
			if createdEvidence {
				var old eventstore.Evidence
				oldErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					old, err = r.EvidenceBySource(ctx, change.ID, "check", check.CheckRunID)
					return err
				})
				if oldErr == nil {
					oldRaw, err := content.Evidence(ctx, old)
					if err != nil {
						return s.deps.NativeAPIError(c, err)
					}
					var oldCheck tracker.ChangeCheck
					if json.Unmarshal(oldRaw, &oldCheck) != nil || oldCheck.Actor.PrincipalID != scope.Credential.ID {
						return s.deps.NativeAPIError(c, hubapi.NativeConflict(change.Revision))
					}
					oldResult, err := json.Marshal(oldCheck.ChangeCheckResult)
					if err != nil {
						return s.deps.NativeAPIError(c, err)
					}
					newResult, err := json.Marshal(check.ChangeCheckResult)
					if err != nil {
						return s.deps.NativeAPIError(c, err)
					}
					if string(oldResult) != string(newResult) {
						return s.deps.NativeAPIError(c, hubapi.NativeConflict(change.Revision))
					}
					response, createdEvidence = oldRaw, false
					receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
					if err != nil {
						return s.deps.NativeAPIError(c, err)
					}
					continue
				}
				if !errors.Is(oldErr, corestore.ErrNotFound) {
					return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(oldErr))
				}
			}
			continue
		}
		if err != nil {
			return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		break
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *RecordMutations) CreateChangeRecord(c echo.Context, request tracker.CreateChange) error {
	ctx := hubcontent.IsolateContentContext(s.deps.RunDetailContext(c.Request().Context()))
	scope := hubscope.NativeRequestScope(c)
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	raw, err := s.CreateChangeRecordCommand(ctx, scope, c.Param("item"), operation, request)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *RecordMutations) CreateChangeRecordCommand(ctx context.Context, scope hubscope.NativeScope, itemID, operation string, request tracker.CreateChange) (json.RawMessage, error) {
	item := tracker.NativeWorkItemID(itemID)
	if strings.TrimSpace(request.Title) == "" || len(request.Title) > 512 || len(request.Body) > 64<<10 || len(request.LinkedIssues) > 32 {
		return nil, hubapi.NativePublicInvalid("Change requires a title up to 512 bytes, discussion up to 64 KiB, and at most 32 linked issues")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	linked := append([]tracker.NativeWorkItemID{item}, request.LinkedIssues...)
	slices.Sort(linked)
	linked = slices.Compact(linked)
	if len(linked) > 32 {
		return nil, hubapi.NativePublicInvalid("A Change may link at most 32 work items")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	authorizeLinked := func(w tenantstore.Writer) error {
		if err := s.deps.AuthorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		project, err := w.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		for _, linkedID := range linked {
			item, err := w.LockWorkItem(ctx, scope.Project, linkedID)
			if err != nil {
				return err
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
				return corestore.ErrNotFound
			}
		}
		return nil
	}
	replayRecord := func() (json.RawMessage, bool, error) {
		var receipt eventstore.Receipt
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			receipt, err = r.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, hubapi.ProjectRecordError(err)
		}
		var raw json.RawMessage
		var payloadErr error
		if receipt.RequestHash == hash {
			raw, payloadErr = content.Receipt(ctx, receipt)
		}
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := authorizeLinked(w); err != nil {
				return err
			}
			current, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
			if err != nil {
				return err
			}
			if current.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			if current.ResponseExpired || current.ResponseReplayUntilNs > 0 && s.deps.Clock().UnixNano() >= current.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			if !reflect.DeepEqual(current, receipt) {
				return hubapi.NativeConflict(0)
			}
			return payloadErr
		})
		if err != nil {
			return nil, false, hubapi.ProjectRecordError(err)
		}
		return raw, true, nil
	}
	replay, found, err := replayRecord()
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	var workerAttempt *leasestore.Attempt
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		if len(request.LinkedIssues) != 0 {
			return nil, hubapi.NativePublicInvalid("Worker changes may only link their leased work item")
		}
		if err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			attempt, _, err := s.deps.RunnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(item), false)
			workerAttempt = &attempt
			return err
		}); err != nil {
			return nil, err
		}
	}
	if _, _, err := s.deps.ReadWorkItemRecord(ctx, scope, string(item)); err != nil {
		return nil, err
	}
	for _, id := range linked {
		if _, _, err := s.deps.ReadWorkItemRecord(ctx, scope, string(id)); err != nil {
			return nil, err
		}
	}
	contentFields := []changefiles.ContentField{{Name: "title", Value: request.Title}, {Name: "body", Value: request.Body}}
	if err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := changefiles.ValidateRecordContent(ctx, r, scope, contentFields, nil, "", false, s.deps.AttachmentReferences)
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	now, err := s.deps.CurrentTime()
	if err != nil {
		return nil, err
	}
	change := tracker.ChangeRequest{ID: hubcontent.PayloadNativeID(ctx, "change"), OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, LinkedIssues: linked, Title: request.Title, Body: request.Body, Revision: 1, CreatedAt: hubcontent.PayloadTimestamp(ctx, now), UpdatedAt: hubcontent.PayloadTimestamp(ctx, now)}
	raw, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	changeRecord, err := content.PrepareChange(ctx, scope.Organization, raw)
	if err != nil {
		return nil, err
	}
	receiptRaw, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, receiptRaw)
	if err != nil {
		return nil, err
	}
	events := make([]eventstore.Event, 0, len(linked))
	for _, linkedID := range linked {
		eventValue := tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: linkedID, Type: "change.created", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: change.CreatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID}}}
		eventRaw, err := json.Marshal(eventValue)
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	for {
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if workerAttempt != nil {
				if _, err := w.RunnerForLease(ctx, string(request.LeaseID)); err != nil {
					return err
				}
			}
			if err := authorizeLinked(w); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return hubapi.NativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if workerAttempt != nil {
				attempt, _, err := s.deps.RunnerSessionFence(ctx, w, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(item), true)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(attempt, *workerAttempt) {
					return corestore.ErrRevisionConflict
				}
			}
			if s.deps.HasPlans() {
				currentTime, err := s.deps.CurrentTime()
				if err != nil {
					return err
				}
				if err := s.deps.RequireCollaboration(ctx, w, currentTime); err != nil {
					return err
				}
			}
			if err := changefiles.BindRecordContent(ctx, w, scope, string(item), contentFields, nil, "", s.deps.AttachmentReferences); err != nil {
				return err
			}
			if _, err := w.CreateChange(ctx, changeRecord); err != nil {
				return err
			}
			for _, linkedID := range linked {
				if err := w.LinkIssue(ctx, eventstore.Link{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: linkedID, ChangeID: change.ID}); err != nil {
					return err
				}
			}
			for _, event := range events {
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := replayRecord()
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
			}
			continue
		}
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		break
	}
	select {
	case s.deps.BoardRefresh <- struct{}{}:
	default:
	}
	return receiptRaw, nil
}
