package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) listChangeRecords(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error) {
	if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
		return nil, err
	}
	var records []eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		records, err = r.Changes(ctx, scope.Project, tracker.NativeWorkItemID(item), "", corestore.MaxPageSize)
		if err != nil || len(records) < corestore.MaxPageSize {
			return err
		}
		more, err := r.Changes(ctx, scope.Project, tracker.NativeWorkItemID(item), records[len(records)-1].ID, 1)
		if err != nil {
			return err
		}
		if len(more) != 0 {
			return corestore.ErrInvalidPage
		}
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if len(records) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	content := eventstore.NewContent(s.runDetailStore())
	result := make([]tracker.ChangeRequest, 0, len(records))
	for _, record := range records {
		change, err := content.Change(ctx, record)
		if err != nil {
			return nil, err
		}
		change, visible, err := s.filterChangeRecord(ctx, scope, change)
		if err != nil {
			return nil, err
		}
		if visible {
			result = append(result, change)
		}
	}
	return result, nil
}

func (s *Service) filterChangeRecord(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest) (tracker.ChangeRequest, bool, error) {
	if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	linked := []tracker.NativeWorkItemID{}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		cursor := tracker.NativeWorkItemID("")
		for {
			page, err := r.Links(ctx, change.ID, cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, link := range page {
				if link.ProjectID != scope.Project {
					return corestore.ErrTenantMismatch
				}
				linked = append(linked, link.WorkItemID)
				if len(linked) > 32 {
					return corestore.ErrInvalidPage
				}
			}
			if len(page) < corestore.MaxPageSize {
				return nil
			}
			cursor = page[len(page)-1].WorkItemID
		}
	})
	if err != nil {
		return tracker.ChangeRequest{}, false, hubapi.ProjectRecordError(err)
	}
	slices.Sort(linked)
	linked = slices.Compact(linked)
	if len(linked) > 32 {
		return tracker.ChangeRequest{}, false, corestore.ErrInvalidPage
	}
	if len(linked) == 0 || !slices.Contains(linked, change.WorkItemID) {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	change.LinkedIssues = linked
	visible := make([]tracker.NativeWorkItemID, 0, len(linked))
	for _, id := range linked {
		row, _, err := s.readWorkItemRecord(ctx, scope, string(id))
		if errors.Is(err, corestore.ErrNotFound) || hubapi.IsNativeNotFound(err) {
			continue
		}
		if err != nil {
			return tracker.ChangeRequest{}, false, err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			continue
		}
		visible = append(visible, id)
	}
	if !slices.Contains(visible, change.WorkItemID) {
		return tracker.ChangeRequest{}, false, nil
	}
	change.LinkedIssues = visible
	return change, true, nil
}

func (s *Service) readChangeRecord(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error) {
	if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
		return tracker.ChangeRequest{}, err
	}
	var record eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Change(ctx, id)
		return err
	})
	if err != nil {
		return tracker.ChangeRequest{}, hubapi.ProjectRecordError(err)
	}
	change, err := eventstore.NewContent(s.runDetailStore()).Change(ctx, record)
	if err != nil {
		return tracker.ChangeRequest{}, err
	}
	change, visible, err := s.filterChangeRecord(ctx, scope, change)
	if err != nil {
		return tracker.ChangeRequest{}, err
	}
	if !visible || !slices.Contains(change.LinkedIssues, tracker.NativeWorkItemID(item)) {
		return tracker.ChangeRequest{}, hubapi.NativeNotFound()
	}
	return change, nil
}

func (s *Service) changeRecordEvidenceMutation(c echo.Context, kind, versionID, sourceKey string, value any) error {
	if review, ok := value.(tracker.ReviewChange); ok && (review.Decision != "commented" || review.Validator != nil || review.ImplementationReview != nil || review.SecurityAudit != nil) {
		return s.changeReviewRecordMutation(c, review)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	response, err := s.changeRecordEvidenceCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), c.Param("change"), operation, kind, versionID, sourceKey, value)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) changeRecordEvidenceCommand(ctx context.Context, scope hubscope.NativeScope, item, changeID, operation, kind, versionID, sourceKey string, value any) (json.RawMessage, error) {
	ctx = hubcontent.IsolateContentContext(s.runDetailContext(ctx))
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
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
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
	if versionID != "" {
		if _, err := s.readChangeVersionRecord(ctx, scope.Organization, change.ID, versionID); err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
	}
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	evidence := eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: change.ID, Kind: kind, CreatedAt: hubcontent.PayloadTimestamp(ctx, now)}
	var raw []byte
	var quality *preparedTenantQualityEscape
	var contentFields []cloudContentField
	switch record := value.(type) {
	case tracker.DiscussChange:
		if strings.TrimSpace(record.Body) == "" || len(record.Body) > 64<<10 {
			return nil, hubapi.NativePublicInvalid("Discussion requires 1 byte to 64 KiB")
		}
		contentFields = []cloudContentField{{name: "body", value: record.Body}}
		if record.Escape != nil {
			quality, err = s.prepareQualityEscapeTenant(ctx, scope, change, record, now)
			if err != nil {
				return nil, hubapi.ProjectRecordError(err)
			}
		}
		if record.VersionID != "" {
			evidence.VersionID = new(record.VersionID)
		}
		if record.Provenance != nil {
			if err := validateNativeProvenance(scope, record.Provenance); err != nil {
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
		contentFields = changeReviewContentFields(record)
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
	} else if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := validateChangeRecordContent(ctx, r, scope, contentFields, nil, "", false)
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	evidence, err = eventstore.NewContent(s.runDetailStore()).PrepareEvidence(ctx, evidence, raw)
	if err != nil {
		return nil, err
	}
	var response []byte
	existingSource := false
	if sourceKey != "" {
		var old eventstore.Evidence
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			old, err = r.EvidenceBySource(ctx, change.ID, kind, sourceKey)
			return err
		})
		if err == nil {
			response, err = eventstore.NewContent(s.runDetailStore()).Evidence(ctx, old)
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
	receipt, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
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
		event, err = eventstore.NewContent(s.runDetailStore()).PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
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
				if err := recheckTenantQualityEscape(ctx, w, scope, change, *value.(tracker.DiscussChange).Escape, quality); err != nil {
					return err
				}
			}
			if createdEvidence {
				if err := bindChangeRecordContent(ctx, w, scope, string(change.WorkItemID), contentFields, nil, ""); err != nil {
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
			replay, found, readErr := s.changeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
			}
			if sourceKey != "" {
				var old eventstore.Evidence
				oldErr := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					old, err = r.EvidenceBySource(ctx, change.ID, kind, sourceKey)
					return err
				})
				if oldErr == nil {
					response, err = eventstore.NewContent(s.runDetailStore()).Evidence(ctx, old)
					if err != nil {
						return nil, err
					}
					receipt, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
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

func (s *Service) readChangeVersionRecord(ctx context.Context, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error) {
	var version eventstore.Version
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		version, err = r.Version(ctx, changeID, versionID)
		return err
	})
	return version, err
}

func (s *Service) changeCheckRecordMutation(c echo.Context, request tracker.SubmitChangeCheck) error {
	ctx := hubcontent.IsolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := hubscope.NativeRequestScope(c)
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	var approved eventstore.ReviewPolicy
	hostedCheck := s.config.Hosted != nil && scope.Credential.Hosted == nil && scope.Credential.Runner.RunnerID == ""
	if hostedCheck {
		approved, err = s.requireHostedChangeCheckPrincipal(ctx, scope)
		if err != nil {
			return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
		}
	}
	readReceipt := func() (json.RawMessage, bool, error) {
		if !hostedCheck {
			return s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
		}
		var receipt eventstore.Receipt
		err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if err := s.recheckHostedChangeCheckPrincipal(ctx, w, scope, approved); err != nil {
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
		value, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
		return value, true, err
	}

	replay, found, err := readReceipt()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if found {
		return c.JSONBlob(http.StatusOK, replay)
	}
	change, err := s.readChangeRecord(ctx, scope, c.Param("item"), c.Param("change"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	versionRecord, err := s.readChangeVersionRecord(ctx, scope.Organization, change.ID, c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	version, err := eventstore.NewContent(s.runDetailStore()).Version(ctx, versionRecord)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := changerequest.ValidateResult(version, request.ChangeCheckResult, scope.Credential.ID, scope.Credential.Scope == hubapi.APIScopeWorker); err != nil {
		if visualErr := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := validateChangeRecordContent(ctx, r, scope, nil, request.Evidence, "evidence", false)
			return err
		}); visualErr != nil {
			return s.nativeAPIError(c, hubapi.ProjectRecordError(visualErr))
		}
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	now := s.config.Clock().UTC()
	if now.IsZero() {
		return s.nativeAPIError(c, ErrInvalidClock)
	}
	if request.CompletedAt.After(now) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("CI completion time cannot be in the future"))
	}
	check := tracker.ChangeCheck{ChangeCheckResult: request.ChangeCheckResult, VersionID: version.ID, Actor: scope.Actor(), ReceivedAt: hubcontent.PayloadTimestamp(ctx, now)}
	raw, err := json.Marshal(check)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: change.ID, Kind: "check", VersionID: new(version.ID), SourceKey: new(check.CheckRunID)}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := raw
	createdEvidence := true
	var existing eventstore.Evidence
	lookupErr := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		existing, err = r.EvidenceBySource(ctx, change.ID, "check", check.CheckRunID)
		return err
	})
	if lookupErr == nil {
		oldRaw, err := content.Evidence(ctx, existing)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		var old tracker.ChangeCheck
		if json.Unmarshal(oldRaw, &old) != nil {
			return s.nativeAPIError(c, corestore.ErrInvalidCursor)
		}
		oldInput, err := json.Marshal(old.ChangeCheckResult)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		newInput, err := json.Marshal(check.ChangeCheckResult)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if string(oldInput) != string(newInput) || old.Actor.PrincipalID != scope.Credential.ID {
			return s.nativeAPIError(c, hubapi.NativeConflict(change.Revision))
		}
		response, createdEvidence = oldRaw, false
	} else if !errors.Is(lookupErr, corestore.ErrNotFound) {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(lookupErr))
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var historyEvent eventstore.Event
	if createdEvidence {
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change.check_submitted", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: check.ReceivedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID}}})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		historyEvent, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	var policyRows hubrecords.CurrentChangePolicyRows
	var workflowPlan *changeReviewWorkflow
	if scope.Credential.Scope != hubapi.APIScopeWorker && version.ID == change.CurrentVersion {
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
			return err
		})
		if err != nil {
			return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		approval, err := hydrateChangeSummaryPolicy(ctx, content, scope, policyRows)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		workflowPlan, err = s.prepareChangeCheckWorkflow(ctx, scope, change, version, check, approval, now)
		if err != nil {
			return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
		}
	}
	var transition *changeWorkflowTransition
	if workflowPlan != nil {
		transition = workflowPlan.transition
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			transitionReady, err := s.lockChangeWorkflowTransition(ctx, w, scope, transition)
			if err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if hostedCheck {
				if err := s.recheckHostedChangeCheckPrincipal(ctx, w, scope, approved); err != nil {
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
				workflowCurrent, err = s.recheckChangeReviewWorkflow(ctx, w, scope, workflowPlan, current)
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
				if err := bindChangeRecordContent(ctx, w, scope, string(change.WorkItemID), nil, request.Evidence, "evidence"); err != nil {
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
				if err := s.applyChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := readReceipt()
			if readErr != nil {
				return s.nativeAPIError(c, readErr)
			}
			if found {
				return c.JSONBlob(http.StatusOK, replay)
			}
			if createdEvidence {
				var old eventstore.Evidence
				oldErr := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					old, err = r.EvidenceBySource(ctx, change.ID, "check", check.CheckRunID)
					return err
				})
				if oldErr == nil {
					oldRaw, err := content.Evidence(ctx, old)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					var oldCheck tracker.ChangeCheck
					if json.Unmarshal(oldRaw, &oldCheck) != nil || oldCheck.Actor.PrincipalID != scope.Credential.ID {
						return s.nativeAPIError(c, hubapi.NativeConflict(change.Revision))
					}
					oldResult, err := json.Marshal(oldCheck.ChangeCheckResult)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					newResult, err := json.Marshal(check.ChangeCheckResult)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					if string(oldResult) != string(newResult) {
						return s.nativeAPIError(c, hubapi.NativeConflict(change.Revision))
					}
					response, createdEvidence = oldRaw, false
					receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					continue
				}
				if !errors.Is(oldErr, corestore.ErrNotFound) {
					return s.nativeAPIError(c, hubapi.ProjectRecordError(oldErr))
				}
			}
			continue
		}
		if err != nil {
			return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		break
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) createChangeRecord(c echo.Context, request tracker.CreateChange) error {
	ctx := hubcontent.IsolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := hubscope.NativeRequestScope(c)
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	raw, err := s.createChangeRecordCommand(ctx, scope, c.Param("item"), operation, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) createChangeRecordCommand(ctx context.Context, scope hubscope.NativeScope, itemID, operation string, request tracker.CreateChange) (json.RawMessage, error) {
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
	content := eventstore.NewContent(s.runDetailStore())
	authorizeLinked := func(w tenantstore.Writer) error {
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
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
			var item corestore.WorkItem
			if scope.Credential.Scope == hubapi.APIScopeWorker {
				item, err = w.WorkItem(ctx, scope.Project, linkedID)
			} else {
				item, err = w.LockWorkItem(ctx, scope.Project, linkedID)
			}
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
		err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
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
			if current.ResponseExpired || current.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= current.ResponseReplayUntilNs {
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
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			attempt, _, err := s.runnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(item), false)
			workerAttempt = &attempt
			return err
		}); err != nil {
			return nil, err
		}
	}
	if _, _, err := s.readWorkItemRecord(ctx, scope, string(item)); err != nil {
		return nil, err
	}
	for _, id := range linked {
		if _, _, err := s.readWorkItemRecord(ctx, scope, string(id)); err != nil {
			return nil, err
		}
	}
	contentFields := []cloudContentField{{name: "title", value: request.Title}, {name: "body", value: request.Body}}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := validateChangeRecordContent(ctx, r, scope, contentFields, nil, "", false)
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	now, err := s.database.currentTime()
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
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
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
				attempt, _, err := s.runnerSessionFence(ctx, w, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(item), true)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(attempt, *workerAttempt) {
					return corestore.ErrRevisionConflict
				}
			}
			if s.database.plansService().Plans() != nil {
				currentTime, err := s.database.currentTime()
				if err != nil {
					return err
				}
				if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", currentTime)); err != nil {
					return err
				}
			}
			if err := bindChangeRecordContent(ctx, w, scope, string(item), contentFields, nil, ""); err != nil {
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
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return receiptRaw, nil
}

func (s *Service) changeRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(ctx, scope.Credential.ID, operation, key)
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
	value, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return value, true, err
}

func (s *Service) authorizedChangeRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	if s.config.AccessRepository == nil {
		return nil, false, hubapi.NativeNotFound()
	}
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = w.Receipt(ctx, scope.Credential.ID, operation, key)
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
	value, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return value, true, err
}

func (s *Service) selectedChangeDetail(ctx context.Context, scope hubscope.NativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error) {
	change, err := s.readChangeRecord(ctx, scope, item, id)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	var versions []eventstore.Version
	var evidence []eventstore.Evidence
	var policyRows hubrecords.CurrentChangePolicyRows
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		versions, err = r.Versions(ctx, change.ID, 0, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(versions) == corestore.MaxPageSize {
			more, err := r.Versions(ctx, change.ID, versions[len(versions)-1].Number, 1)
			if err != nil {
				return err
			}
			if len(more) != 0 {
				return corestore.ErrInvalidPage
			}
		}
		evidence, err = r.Evidence(ctx, change.ID, 0, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(evidence) == corestore.MaxPageSize {
			more, err := r.Evidence(ctx, change.ID, evidence[len(evidence)-1].Sequence, 1)
			if err != nil {
				return err
			}
			if len(more) != 0 {
				return corestore.ErrInvalidPage
			}
		}
		policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}
	if len(versions) > corestore.MaxPageSize || len(evidence) > corestore.MaxPageSize {
		return tracker.ChangeDetail{}, corestore.ErrInvalidPage
	}
	content := eventstore.NewContent(s.runDetailStore())
	result := tracker.ChangeDetail{Change: change}
	seenSources := make(map[string]bool)
	for _, linked := range change.LinkedIssues {
		row, snapshot, err := s.readWorkItemRecord(ctx, scope, string(linked))
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		issue, err := s.hydrateWorkItemRecord(ctx, row, snapshot)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		for _, reference := range issue.ExternalReferences {
			if reference.Provider == "github" && reference.URL != "" && !seenSources[reference.URL] {
				result.SourceIssues = append(result.SourceIssues, reference)
				seenSources[reference.URL] = true
			}
		}
	}
	for _, row := range versions {
		version, err := s.hydrateChangeVersion(ctx, scope, tracker.NativeWorkItemID(item), row)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		if landed := change.CurrentLanding(); landed != nil && landed.Quality != nil && version.ChangeID == change.ID && landed.VersionID == version.ID && landed.HeadSHA == version.HeadSHA {
			version.LandingQuality = landed.Quality
		}
		result.Versions = append(result.Versions, version)
	}
	for _, row := range evidence {
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		switch row.Kind {
		case "review":
			var value tracker.ChangeReview
			if err := json.Unmarshal(raw, &value); err != nil {
				return tracker.ChangeDetail{}, err
			}
			result.Reviews = append(result.Reviews, value)
		case "check":
			var value tracker.ChangeCheck
			if err := json.Unmarshal(raw, &value); err != nil {
				return tracker.ChangeDetail{}, err
			}
			result.Checks = append(result.Checks, value)
		case "discussion":
			var value tracker.ChangeDiscussion
			if err := json.Unmarshal(raw, &value); err != nil {
				return tracker.ChangeDetail{}, err
			}
			result.Discussion = append(result.Discussion, value)
		}
	}
	var current *tracker.ChangeVersion
	for i := range result.Versions {
		if result.Versions[i].ID == result.Change.CurrentVersion {
			current = &result.Versions[i]
			break
		}
	}
	currentApproval, err := hydrateChangeSummaryPolicy(ctx, content, scope, policyRows)
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}
	policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(current, currentApproval.Policy, currentApproval.ReviewPolicy)
	result.Summary, err = s.summarizeChangeWithAuditDispositions(ctx, scope, result, policyID, reviewPolicyID, now, false)
	if err == nil {
		err = s.loadChangeExternalRecord(ctx, scope, tracker.NativeWorkItemID(item), &result)
	}
	return result, err
}

func (s *Service) nativeClaimCurrentChangeDetail(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan, now time.Time) (tracker.ChangeDetail, error) {
	if plan.ChangeFacts == nil || plan.ChangeFacts.VersionObservations == nil || plan.ChangeRecord.ID == "" || plan.ChangeRecord.CurrentVersionID == nil || *plan.ChangeRecord.CurrentVersionID != plan.ChangeVersion.ID || plan.ChangeVersion.ChangeID != plan.ChangeRecord.ID {
		return tracker.ChangeDetail{}, corestore.ErrRevisionConflict
	}
	return s.hydrateCurrentChangeDetailWithFacts(ctx, scope, plan.Candidate.WorkItemID, plan.ChangeRecord, &plan.ChangeVersion, plan.ChangePolicy, now, plan.ChangeFacts, plan.Approval, corestore.Serving)
}

func (s *Service) selectedCurrentChangeSummary(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, error) {
	if _, _, err := s.readWorkItemRecord(ctx, scope, string(item)); err != nil {
		return tracker.ChangeSummary{}, err
	}
	var change eventstore.Change
	var version *eventstore.Version
	var policyRows hubrecords.CurrentChangePolicyRows
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		change, err = r.Change(ctx, id)
		if err != nil {
			return err
		}
		if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project {
			return corestore.ErrNotFound
		}
		if change.CurrentVersionID != nil {
			row, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			version = &row
		}
		policyRows, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeSummary{}, hubapi.ProjectRecordError(err)
	}
	detail, err := s.hydrateCurrentChangeDetail(ctx, scope, item, change, version, policyRows, now)
	if err == nil {
		err = s.loadChangeExternalRecord(ctx, scope, item, &detail)
	}
	return detail.Summary, err
}

func (s *Service) hydrateCurrentChangeDetail(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time) (tracker.ChangeDetail, error) {
	return s.hydrateCurrentChangeDetailWithFacts(ctx, scope, item, changeRecord, versionRecord, policyRows, now, nil, nil, corestore.Serving)
}

func (s *Service) hydrateCurrentChangeDetailWithFacts(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *hubrecords.ChangeVersionApproval, workload corestore.Workload) (tracker.ChangeDetail, error) {
	return s.hydrateCurrentChangeDetailSelection(ctx, scope, item, changeRecord, versionRecord, policyRows, now, facts, approval, workload, false)
}

func (s *Service) hydrateCurrentChangeDetailSelection(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *hubrecords.ChangeVersionApproval, workload corestore.Workload, recovery bool) (tracker.ChangeDetail, error) {
	if changeRecord.CurrentVersionID == nil && versionRecord != nil || changeRecord.CurrentVersionID != nil && (versionRecord == nil || *changeRecord.CurrentVersionID != versionRecord.ID || versionRecord.ChangeID != changeRecord.ID) {
		return tracker.ChangeDetail{}, corestore.ErrRevisionConflict
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	change, err := content.Change(ctx, changeRecord)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	var visible bool
	if facts == nil {
		change, visible, err = s.filterChangeRecord(ctx, scope, change)
	} else {
		change, visible, err = filterBoardChangeRecord(scope, change, *facts)
	}
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	if !visible || !slices.Contains(change.LinkedIssues, item) || change.CurrentVersion != hubapi.ValueOrEmpty(changeRecord.CurrentVersionID) {
		return tracker.ChangeDetail{}, corestore.ErrNotFound
	}
	if versionRecord == nil {
		currentApproval, err := boardCurrentApproval(ctx, content, scope, policyRows, approval)
		if err != nil {
			return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
		}
		detail := tracker.ChangeDetail{Change: change}
		policyID, reviewID := changerequest.CurrentPolicyIdentities(nil, currentApproval.Policy, currentApproval.ReviewPolicy)
		detail.Summary = changerequest.SummarizeCurrentVersion(detail, policyID, reviewID, now, false)
		return detail, nil
	}
	var observations *tenantstore.ChangeVersionRows
	if facts != nil {
		observations = facts.VersionObservations
	}
	version, err := s.hydrateChangeVersionWithRows(ctx, scope, item, *versionRecord, observations)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	if landed := change.CurrentLanding(); landed != nil && landed.Quality != nil && version.ChangeID == change.ID && landed.VersionID == version.ID && landed.HeadSHA == version.HeadSHA {
		version.LandingQuality = landed.Quality
	}
	var evidence []eventstore.Evidence
	var priorApproval bool
	if facts != nil {
		evidence = facts.Evidence
	} else if recovery {
		repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
		if !ok {
			return tracker.ChangeDetail{}, corestore.ErrInvalidPage
		}
		checks := make([]string, 0, len(version.Checks))
		for _, check := range version.Checks {
			checks = append(checks, check.CheckRunID)
		}
		err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.ChangeRecoveryReader)
			if !ok {
				return corestore.ErrInvalidPage
			}
			var err error
			evidence, priorApproval, err = reader.RecoveryChangeEvidence(ctx, change.ID, version.ID, changeRecord.EvidenceSequence, checks)
			return err
		})
	} else if hubcontent.ExplanationRead(ctx) {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, workload, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.ChangeGateEvidenceReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			var err error
			evidence, err = reader.CurrentVersionGateEvidence(ctx, change.ID, version.ID, changeRecord.EvidenceSequence)
			return err
		})
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			evidence, err = r.Evidence(ctx, changeRecord.ID, 0, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			if len(evidence) == corestore.MaxPageSize {
				more, err := r.Evidence(ctx, changeRecord.ID, evidence[len(evidence)-1].Sequence, 1)
				if err != nil {
					return err
				}
				if len(more) != 0 {
					return corestore.ErrInvalidPage
				}
			}
			return nil
		})
	}
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}

	if !recovery && len(evidence) > corestore.MaxPageSize {
		return tracker.ChangeDetail{}, corestore.ErrInvalidPage
	}
	detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{changeVersionResponse(version)}}
	for _, row := range evidence {
		if row.OrganizationID != scope.Organization || row.ChangeID != changeRecord.ID {
			return tracker.ChangeDetail{}, corestore.ErrTenantMismatch
		}
		currentReview := row.Kind == "review" && row.VersionID != nil && *row.VersionID == version.ID
		priorReview := row.Kind == "review" && row.Decision == "approved" && (row.VersionID == nil || *row.VersionID != version.ID) && !priorApproval
		currentCheck := row.Kind == "check" && row.VersionID != nil && *row.VersionID == version.ID
		currentDiscussion := row.Kind == "discussion" && (row.VersionID == nil || *row.VersionID == "" || *row.VersionID == version.ID)
		if !currentReview && !priorReview && !currentCheck && !currentDiscussion {
			continue
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		switch row.Kind {
		case "review":
			var review tracker.ChangeReview
			if err := json.Unmarshal(raw, &review); err != nil {
				return tracker.ChangeDetail{}, err
			}
			if review.VersionID != hubapi.ValueOrEmpty(row.VersionID) || review.Decision != row.Decision || review.Actor.Kind != row.ActorKind || review.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(review.CreatedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			if currentReview {
				detail.Reviews = append(detail.Reviews, review)
			} else if priorReview && review.Decision == "approved" && review.SecurityAudit == nil && review.ImplementationReview == nil && review.Validator == nil {
				priorApproval = true
			}
		case "check":
			var check tracker.ChangeCheck
			if err := json.Unmarshal(raw, &check); err != nil {
				return tracker.ChangeDetail{}, err
			}
			if check.VersionID != hubapi.ValueOrEmpty(row.VersionID) || check.CheckRunID != hubapi.ValueOrEmpty(row.SourceKey) || check.Actor.Kind != row.ActorKind || check.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(check.ReceivedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			detail.Checks = append(detail.Checks, check)
		case "discussion":
			var discussion tracker.ChangeDiscussion
			if err := json.Unmarshal(raw, &discussion); err != nil {
				return tracker.ChangeDetail{}, err
			}
			if discussion.VersionID != hubapi.ValueOrEmpty(row.VersionID) || discussion.Actor.Kind != row.ActorKind || discussion.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(discussion.CreatedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			detail.Discussion = append(detail.Discussion, discussion)
		}
	}
	currentApproval, err := boardCurrentApproval(ctx, content, scope, policyRows, approval)
	if err != nil {
		return tracker.ChangeDetail{}, hubapi.ProjectRecordError(err)
	}
	policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(&detail.Versions[0], currentApproval.Policy, currentApproval.ReviewPolicy)
	detail.Summary, err = s.summarizeChangeWithAuditDispositionsFor(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval, workload, facts)
	if err == nil && hubcontent.ExplanationRead(ctx) && facts == nil && !recovery && (len(detail.Checks) > 64 || len(detail.Reviews) > 64) {
		detail.Summary.Status = "evidence_incomplete"
		if len(detail.Checks) > 64 {
			detail.Summary.Checks = "unknown"
		}
		if len(detail.Reviews) > 64 {
			detail.Summary.NativeReview, detail.Summary.ExternalReview = "unknown", "unknown"
		}
		detail.Summary.Messages = []string{"Current-version check or review history is truncated"}
	}
	if err == nil && recovery {
		detail.PublicationPushes, err = s.readBoundedChangePushes(ctx, scope, string(item), detail.Versions)
	}
	return detail, err
}

func sameTypedEvidenceTime(payload, typed time.Time) bool {
	return payload.UTC().Truncate(time.Microsecond).Equal(typed.UTC().Truncate(time.Microsecond))
}

func (s *Service) selectedLatestChangeSummary(ctx context.Context, scope hubscope.NativeScope, item string, now time.Time) (tracker.NativeChangeReference, tracker.ChangeSummary, bool, error) {
	var latest eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		latest, err = r.LatestChange(ctx, scope.Project, tracker.NativeWorkItemID(item))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, nil
	}
	if err != nil {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, hubapi.ProjectRecordError(err)
	}
	detail, err := s.selectedChangeDetail(ctx, scope, item, latest.ID, now)
	if err != nil {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, err
	}
	reference := tracker.NativeChangeReference{ChangeID: latest.ID}
	for _, version := range detail.Versions {
		if version.ID == detail.Change.CurrentVersion {
			reference.VersionID = version.ID
			reference.HeadSHA = version.HeadSHA
			break
		}
	}
	return reference, detail.Summary, true, nil
}

func (s *Service) lockChangeRecordOperation(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item, changeID, versionID string) (eventstore.Change, error) {
	if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
		return eventstore.Change{}, err
	}
	if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
		return eventstore.Change{}, err
	}
	project, err := w.Project(ctx, scope.Project)
	if err != nil {
		return eventstore.Change{}, err
	}
	if project.DeletedAt != nil {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	var issue corestore.WorkItem
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		issue, err = w.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
	} else {
		issue, err = w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
	}
	if err != nil {
		return eventstore.Change{}, err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(issue.ActorPrincipalID)}) {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	var change eventstore.Change
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		change, err = w.Change(ctx, changeID)
	} else {
		change, err = w.LockChange(ctx, changeID)
	}
	if err != nil {
		return eventstore.Change{}, err
	}
	if change.ProjectID != scope.Project {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	if change.WorkItemID != tracker.NativeWorkItemID(item) {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	if versionID != "" {
		if _, err := w.Version(ctx, changeID, versionID); err != nil {
			return eventstore.Change{}, err
		}
	}
	return change, nil
}

func (s *Service) changeOperationRecordReceipt(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation, key, hash string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, scope.Credential.ID, operation, key)
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
		raw, payloadErr = eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := s.lockChangeRecordOperation(ctx, w, scope, item, changeID, versionID); err != nil {
			return err
		}
		current, err := w.Receipt(ctx, scope.Credential.ID, operation, key)
		if err != nil {
			return err
		}
		if current.RequestHash != hash {
			return hubapi.NativeConflict(0)
		}
		if current.ResponseExpired || current.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= current.ResponseReplayUntilNs {
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

func (s *Service) loadChangeExternalRecord(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, detail *tracker.ChangeDetail) error {
	var selected *tracker.ChangeVersion
	for index := range detail.Versions {
		version := &detail.Versions[index]
		if version.ID == detail.Change.CurrentVersion && version.External != nil && version.External.Provider == "github" {
			selected = version
			break
		}
	}
	if selected == nil {
		return nil
	}
	number := externalPullRequestNumber(*selected.External)
	owner, repository, valid := splitRepositoryFullName(strings.TrimPrefix(strings.TrimSuffix(selected.Repository, ".git"), "https://github.com/"))
	if !valid || number <= 0 || selected.External.URL != "https://github.com/"+owner+"/"+repository+"/pull/"+selected.External.ID {
		return nil
	}
	name := strings.ToLower(owner + "/" + repository)
	var observed *projectstore.GitHubPullRequestObservation
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		itemRecord, err := r.WorkItem(ctx, scope.Project, item)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(itemRecord.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" || !project.Settings.RepositoryEnabled || !strings.EqualFold(project.Settings.CheckoutRepository, name) {
			return nil
		}
		change, err := r.Change(ctx, detail.Change.ID)
		if err != nil {
			return err
		}
		if change.ProjectID != scope.Project || change.Revision != int64(detail.Change.Revision) || hubapi.ValueOrEmpty(change.CurrentVersionID) != selected.ID {
			return corestore.ErrRevisionConflict
		}
		version, err := r.Version(ctx, change.ID, selected.ID)
		if err != nil {
			return err
		}
		if version.HeadSHA != selected.HeadSHA {
			return corestore.ErrRevisionConflict
		}
		if version.ExternalProvider != selected.External.Provider || version.ExternalID != selected.External.ID || version.ExternalURL != selected.External.URL {
			publication, err := r.PRPublication(ctx, selected.ID)
			if err != nil {
				return err
			}
			binding := tracker.NativePRPublication{ChangeID: publication.ChangeID, VersionID: publication.VersionID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: publication.PolicyID, SourceVersion: tracker.NativeChangeReference{ChangeID: publication.ChangeID, VersionID: publication.VersionID, HeadSHA: publication.HeadSHA}, SourceAttemptID: publication.SourceAttemptID, External: publication.External}
			if !binding.Matches(detail.Change.ID, *selected) || publication.External != *selected.External {
				return corestore.ErrRevisionConflict
			}
		}
		reader, ok := r.(projectstore.GitHubPullRequestRefreshReader)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		row, err := reader.GitHubPullRequestRefresh(ctx, projectstore.GitHubPullRequestRefreshKey{ProjectID: scope.Project, ChangeID: change.ID, Repository: name, Number: number})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.RequestedVersion != selected.ID || row.RequestedHeadSHA != selected.HeadSHA || row.ObservedRequestCount != row.RequestCount || row.Observation == nil || row.Observation.Repository != name || row.Observation.Number != number || row.Observation.ObservedAt.Before(row.RequestedAt) {
			return nil
		}
		observed = row.Observation
		return nil
	})
	if err != nil || observed == nil {
		return err
	}
	detail.External = &tracker.PullRequestSummary{Number: number, URL: selected.External.URL, State: observed.State, Draft: observed.Draft, HeadSHA: observed.HeadSHA, Checks: observed.Checks, Reviews: observed.Reviews, Merge: tracker.MergeSummary{State: "unknown"}}
	if observed.HeadSHA != selected.HeadSHA {
		detail.Summary.ExternalReview = "stale_head"
	} else if observed.Reviews.Decision != "" {
		detail.Summary.ExternalReview = "snapshot: " + observed.Reviews.Decision
	}
	return nil
}
