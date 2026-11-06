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
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func selectedChangeRecordRoute(path string) bool {
	return path == nativeBase+"/change-review-policy" || strings.HasPrefix(path, changeBase)
}

func (s *Service) listChangeRecords(ctx context.Context, scope nativeScope, item string) ([]tracker.ChangeRequest, error) {
	if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
		return nil, err
	}
	var records []eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		records, err = r.Changes(ctx, scope.project, tracker.NativeWorkItemID(item), "", corestore.MaxPageSize)
		if err != nil || len(records) < corestore.MaxPageSize {
			return err
		}
		more, err := r.Changes(ctx, scope.project, tracker.NativeWorkItemID(item), records[len(records)-1].ID, 1)
		if err != nil {
			return err
		}
		if len(more) != 0 {
			return corestore.ErrInvalidPage
		}
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
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

func (s *Service) filterChangeRecord(ctx context.Context, scope nativeScope, change tracker.ChangeRequest) (tracker.ChangeRequest, bool, error) {
	if change.OrganizationID != scope.organization || change.ProjectID != scope.project {
		return tracker.ChangeRequest{}, false, corestore.ErrNotFound
	}
	linked := []tracker.NativeWorkItemID{}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		cursor := tracker.NativeWorkItemID("")
		for {
			page, err := r.Links(ctx, change.ID, cursor, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, link := range page {
				if link.ProjectID != scope.project {
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
		return tracker.ChangeRequest{}, false, projectRecordError(err)
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
		if errors.Is(err, corestore.ErrNotFound) || isNativeNotFound(err) {
			continue
		}
		if err != nil {
			return tracker.ChangeRequest{}, false, err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
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

func (s *Service) readChangeRecord(ctx context.Context, scope nativeScope, item, id string) (tracker.ChangeRequest, error) {
	if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
		return tracker.ChangeRequest{}, err
	}
	var record eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Change(ctx, id)
		return err
	})
	if err != nil {
		return tracker.ChangeRequest{}, projectRecordError(err)
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
		return tracker.ChangeRequest{}, nativeNotFound()
	}
	return change, nil
}

func (s *Service) changeRecordEvidenceMutation(c echo.Context, kind, versionID, sourceKey string, value any) error {
	if review, ok := value.(tracker.ReviewChange); ok && (review.Decision != "commented" || review.Validator != nil || review.ImplementationReview != nil || review.SecurityAudit != nil) {
		return s.changeReviewRecordMutation(c, review)
	}
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	item, changeID := c.Param("item"), c.Param("change")
	if scope.credential.Scope == apiScopeWorker {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	var command tracker.Mutation
	switch record := value.(type) {
	case tracker.DiscussChange:
		command = record.Mutation
	case tracker.ReviewChange:
		command = record.Mutation
	default:
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key is required"))
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	hash, err := nativeCommandHash(value)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
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
	if versionID != "" {
		if _, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, versionID); err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	evidence := eventstore.Evidence{OrganizationID: scope.organization, ChangeID: change.ID, Kind: kind, CreatedAt: payloadTimestamp(ctx, now)}
	var raw []byte
	switch record := value.(type) {
	case tracker.DiscussChange:
		if strings.TrimSpace(record.Body) == "" || len(record.Body) > 64<<10 || record.Escape != nil {
			return s.nativeAPIError(c, nativePublicInvalid("Discussion requires 1 byte to 64 KiB and cannot include a quality escape"))
		}
		if record.VersionID != "" {
			evidence.VersionID = new(record.VersionID)
		}
		if record.Provenance != nil {
			if err := validateNativeProvenance(scope, record.Provenance); err != nil {
				return s.nativeAPIError(c, err)
			}
			sourceKey = record.Provenance.Provider + ":" + record.Provenance.ExternalID
		}
		discussion := tracker.ChangeDiscussion{ID: payloadNativeID(ctx, "cmt"), VersionID: record.VersionID, Body: record.Body, Actor: scope.actor(), Provenance: record.Provenance, CreatedAt: evidence.CreatedAt}
		raw, err = json.Marshal(discussion)
	case tracker.ReviewChange:
		if record.Decision != "commented" || record.ExpectedRevision != 0 && record.ExpectedRevision != change.Revision || record.ExpectedVersionID != "" && record.ExpectedVersionID != versionID || len(record.Body) > 64<<10 {
			return s.nativeAPIError(c, workItemStorageUnavailable())
		}
		if record.Validator != nil || record.ImplementationReview != nil || record.SecurityAudit != nil || record.Bundle != nil {
			return s.nativeAPIError(c, workItemStorageUnavailable())
		}
		evidence.VersionID = new(versionID)
		review := tracker.ChangeReview{ID: payloadNativeID(ctx, "review"), VersionID: versionID, Decision: record.Decision, Body: record.Body, Actor: scope.actor(), CreatedAt: evidence.CreatedAt}
		raw, err = json.Marshal(review)
	default:
		return s.nativeAPIError(c, nativePublicInvalid("Unsupported change evidence"))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if sourceKey != "" {
		evidence.SourceKey = new(sourceKey)
	}
	evidence, err = eventstore.NewContent(s.runDetailStore()).PrepareEvidence(ctx, evidence, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var response []byte
	existingSource := false
	if sourceKey != "" {
		var old eventstore.Evidence
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			old, err = r.EvidenceBySource(ctx, change.ID, kind, sourceKey)
			return err
		})
		if err == nil {
			response, err = eventstore.NewContent(s.runDetailStore()).Evidence(ctx, old)
			existingSource = true
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return s.nativeAPIError(c, projectRecordError(err))
		}
	}
	if response == nil {
		response = raw
	}
	createdEvidence := !existingSource
	receipt, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var event eventstore.Event
	if createdEvidence {
		eventValue := tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change." + kind, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: evidence.CreatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: versionID}}}
		eventRaw, err := json.Marshal(eventValue)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		event, err = eventstore.NewContent(s.runDetailStore()).PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return nativeConflict(change.Revision)
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
			if createdEvidence {
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
				return s.nativeAPIError(c, readErr)
			}
			if found {
				return c.JSONBlob(http.StatusOK, replay)
			}
			if sourceKey != "" {
				var old eventstore.Evidence
				oldErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					old, err = r.EvidenceBySource(ctx, change.ID, kind, sourceKey)
					return err
				})
				if oldErr == nil {
					response, err = eventstore.NewContent(s.runDetailStore()).Evidence(ctx, old)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					receipt, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					createdEvidence = false
					continue
				}
				if !errors.Is(oldErr, corestore.ErrNotFound) {
					return s.nativeAPIError(c, projectRecordError(oldErr))
				}
			}
			continue
		}
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		break
	}
	return c.JSONBlob(http.StatusOK, response)
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
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	if scope.credential.Scope == apiScopeWorker {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	var approved eventstore.ReviewPolicy
	hostedCheck := s.config.Hosted != nil && scope.credential.Hosted == nil && scope.credential.Runner.RunnerID == ""
	if hostedCheck {
		approved, err = s.requireHostedChangeCheckPrincipal(ctx, scope)
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
	}
	readReceipt := func() (json.RawMessage, bool, error) {
		if !hostedCheck {
			return s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
		}
		var receipt eventstore.Receipt
		err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if err := s.recheckHostedChangeCheckPrincipal(ctx, w, scope, approved); err != nil {
				return err
			}
			var err error
			receipt, err = w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, projectRecordError(err)
		}
		if receipt.RequestHash != hash {
			return nil, false, nativeConflict(0)
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
	versionRecord, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	version, err := eventstore.NewContent(s.runDetailStore()).Version(ctx, versionRecord)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := changerequest.ValidateResult(version, request.ChangeCheckResult, scope.credential.ID, scope.credential.Scope == apiScopeWorker); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	now := s.config.now().UTC()
	if now.IsZero() {
		return s.nativeAPIError(c, ErrInvalidClock)
	}
	if request.CompletedAt.After(now) {
		return s.nativeAPIError(c, nativePublicInvalid("CI completion time cannot be in the future"))
	}
	check := tracker.ChangeCheck{ChangeCheckResult: request.ChangeCheckResult, VersionID: version.ID, Actor: scope.actor(), ReceivedAt: payloadTimestamp(ctx, now)}
	raw, err := json.Marshal(check)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: scope.organization, ChangeID: change.ID, Kind: "check", VersionID: new(version.ID), SourceKey: new(check.CheckRunID)}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := raw
	createdEvidence := true
	var existing eventstore.Evidence
	lookupErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		if string(oldInput) != string(newInput) || old.Actor.PrincipalID != scope.credential.ID {
			return s.nativeAPIError(c, nativeConflict(change.Revision))
		}
		response, createdEvidence = oldRaw, false
	} else if !errors.Is(lookupErr, corestore.ErrNotFound) {
		return s.nativeAPIError(c, projectRecordError(lookupErr))
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var historyEvent eventstore.Event
	if createdEvidence {
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "change.check_submitted", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: check.ReceivedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID}}})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		historyEvent, err = content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if hostedCheck {
				if err := s.recheckHostedChangeCheckPrincipal(ctx, w, scope, approved); err != nil {
					return err
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
			if createdEvidence {
				if _, err := w.AppendEvidence(ctx, evidence); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, historyEvent); err != nil {
					return err
				}
			}
			_, err := w.PutReceipt(ctx, receipt)
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
				oldErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
					if json.Unmarshal(oldRaw, &oldCheck) != nil || oldCheck.Actor.PrincipalID != scope.credential.ID {
						return s.nativeAPIError(c, nativeConflict(change.Revision))
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
						return s.nativeAPIError(c, nativeConflict(change.Revision))
					}
					response, createdEvidence = oldRaw, false
					receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
					if err != nil {
						return s.nativeAPIError(c, err)
					}
					continue
				}
				if !errors.Is(oldErr, corestore.ErrNotFound) {
					return s.nativeAPIError(c, projectRecordError(oldErr))
				}
			}
			continue
		}
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		break
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) createChangeRecord(c echo.Context, request tracker.CreateChange) error {
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	raw, err := s.createChangeRecordCommand(ctx, scope, c.Param("item"), operation, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) createChangeRecordCommand(ctx context.Context, scope nativeScope, itemID, operation string, request tracker.CreateChange) (json.RawMessage, error) {
	item := tracker.NativeWorkItemID(itemID)
	if strings.TrimSpace(request.Title) == "" || len(request.Title) > 512 || len(request.Body) > 64<<10 || len(request.LinkedIssues) > 32 {
		return nil, nativePublicInvalid("Change requires a title up to 512 bytes, discussion up to 64 KiB, and at most 32 linked issues")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	linked := append([]tracker.NativeWorkItemID{item}, request.LinkedIssues...)
	slices.Sort(linked)
	linked = slices.Compact(linked)
	if len(linked) > 32 {
		return nil, nativePublicInvalid("A Change may link at most 32 work items")
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	authorizeLinked := func(w tenantstore.Writer) error {
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		project, err := w.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		for _, linkedID := range linked {
			var item corestore.WorkItem
			if scope.credential.Scope == apiScopeWorker {
				item, err = w.WorkItem(ctx, scope.project, linkedID)
			} else {
				item, err = w.LockWorkItem(ctx, scope.project, linkedID)
			}
			if err != nil {
				return err
			}
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
				return corestore.ErrNotFound
			}
		}
		return nil
	}
	replayRecord := func() (json.RawMessage, bool, error) {
		var receipt eventstore.Receipt
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			receipt, err = r.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			return err
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, projectRecordError(err)
		}
		var raw json.RawMessage
		var payloadErr error
		if receipt.RequestHash == hash {
			raw, payloadErr = content.Receipt(ctx, receipt)
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := authorizeLinked(w); err != nil {
				return err
			}
			current, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			if err != nil {
				return err
			}
			if current.RequestHash != hash {
				return nativeConflict(0)
			}
			if current.ResponseExpired || current.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= current.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			if !reflect.DeepEqual(current, receipt) {
				return nativeConflict(0)
			}
			return payloadErr
		})
		if err != nil {
			return nil, false, projectRecordError(err)
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
	if scope.credential.Scope == apiScopeWorker {
		if len(request.LinkedIssues) != 0 {
			return nil, nativePublicInvalid("Worker changes may only link their leased work item")
		}
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	change := tracker.ChangeRequest{ID: payloadNativeID(ctx, "change"), OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, LinkedIssues: linked, Title: request.Title, Body: request.Body, Revision: 1, CreatedAt: payloadTimestamp(ctx, now), UpdatedAt: payloadTimestamp(ctx, now)}
	raw, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	changeRecord, err := content.PrepareChange(ctx, scope.organization, raw)
	if err != nil {
		return nil, err
	}
	receiptRaw, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, receiptRaw)
	if err != nil {
		return nil, err
	}
	events := make([]eventstore.Event, 0, len(linked))
	for _, linkedID := range linked {
		eventValue := tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: linkedID, Type: "change.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: change.CreatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID}}}
		eventRaw, err := json.Marshal(eventValue)
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := authorizeLinked(w); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return nativeConflict(0)
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
			if s.database.plans() != nil {
				currentTime, err := s.database.currentTime()
				if err != nil {
					return err
				}
				if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", currentTime); err != nil {
					return err
				}
			}
			if _, err := w.CreateChange(ctx, changeRecord); err != nil {
				return err
			}
			for _, linkedID := range linked {
				if err := w.LinkIssue(ctx, eventstore.Link{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: linkedID, ChangeID: change.ID}); err != nil {
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
			return nil, projectRecordError(err)
		}
		break
	}
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return receiptRaw, nil
}

func (s *Service) changeRecordReceipt(ctx context.Context, scope nativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(ctx, scope.credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, projectRecordError(err)
	}
	if receipt.RequestHash != hash {
		return nil, false, nativeConflict(0)
	}
	value, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return value, true, err
}

func (s *Service) authorizedChangeRecordReceipt(ctx context.Context, scope nativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	if s.config.AccessRepository == nil {
		return nil, false, nativeNotFound()
	}
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = w.Receipt(ctx, scope.credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, projectRecordError(err)
	}
	if receipt.RequestHash != hash {
		return nil, false, nativeConflict(0)
	}
	value, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return value, true, err
}

func (s *Service) selectedChangeDetail(ctx context.Context, scope nativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error) {
	change, err := s.readChangeRecord(ctx, scope, item, id)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	var versions []eventstore.Version
	var evidence []eventstore.Evidence
	var policyRows currentChangePolicyRows
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		policyRows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeDetail{}, projectRecordError(err)
	}
	if len(versions) > corestore.MaxPageSize || len(evidence) > corestore.MaxPageSize {
		return tracker.ChangeDetail{}, corestore.ErrInvalidPage
	}
	content := eventstore.NewContent(s.runDetailStore())
	result := tracker.ChangeDetail{Change: change}
	for _, row := range versions {
		version, err := content.Version(ctx, row)
		if err != nil {
			return tracker.ChangeDetail{}, err
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
		return tracker.ChangeDetail{}, projectRecordError(err)
	}
	policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(current, currentApproval.Policy, currentApproval.ReviewPolicy)
	result.Summary, err = s.summarizeChangeWithAuditDispositions(ctx, scope, result, policyID, reviewPolicyID, now, false)
	return result, err
}

func (s *Service) nativeClaimCurrentChangeDetail(ctx context.Context, scope nativeScope, plan nativeClaimPlan, now time.Time) (tracker.ChangeDetail, error) {
	if plan.changeRecord.ID == "" || plan.changeRecord.CurrentVersionID == nil || *plan.changeRecord.CurrentVersionID != plan.changeVersion.ID || plan.changeVersion.ChangeID != plan.changeRecord.ID {
		return tracker.ChangeDetail{}, corestore.ErrRevisionConflict
	}
	return s.hydrateCurrentChangeDetailWithFacts(ctx, scope, plan.candidate.WorkItemID, plan.changeRecord, &plan.changeVersion, plan.changePolicy, now, plan.changeFacts, nil, corestore.Serving)
}

func (s *Service) selectedCurrentChangeSummary(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, error) {
	if _, _, err := s.readWorkItemRecord(ctx, scope, string(item)); err != nil {
		return tracker.ChangeSummary{}, err
	}
	var change eventstore.Change
	var version *eventstore.Version
	var policyRows currentChangePolicyRows
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		change, err = r.Change(ctx, id)
		if err != nil {
			return err
		}
		if change.OrganizationID != scope.organization || change.ProjectID != scope.project {
			return corestore.ErrNotFound
		}
		if change.CurrentVersionID != nil {
			row, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
			if err != nil {
				return err
			}
			version = &row
		}
		policyRows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeSummary{}, projectRecordError(err)
	}
	detail, err := s.hydrateCurrentChangeDetail(ctx, scope, item, change, version, policyRows, now)
	return detail.Summary, err
}

func (s *Service) hydrateCurrentChangeDetail(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows currentChangePolicyRows, now time.Time) (tracker.ChangeDetail, error) {
	return s.hydrateCurrentChangeDetailWithFacts(ctx, scope, item, changeRecord, versionRecord, policyRows, now, nil, nil, corestore.Serving)
}

func (s *Service) hydrateCurrentChangeDetailWithFacts(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows currentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *ChangeVersionApproval, workload corestore.Workload) (tracker.ChangeDetail, error) {
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
	if !visible || !slices.Contains(change.LinkedIssues, item) || change.CurrentVersion != valueOrEmpty(changeRecord.CurrentVersionID) {
		return tracker.ChangeDetail{}, corestore.ErrNotFound
	}
	if versionRecord == nil {
		currentApproval, err := boardCurrentApproval(ctx, content, scope, policyRows, approval)
		if err != nil {
			return tracker.ChangeDetail{}, projectRecordError(err)
		}
		detail := tracker.ChangeDetail{Change: change}
		policyID, reviewID := changerequest.CurrentPolicyIdentities(nil, currentApproval.Policy, currentApproval.ReviewPolicy)
		detail.Summary = changerequest.SummarizeCurrentVersion(detail, policyID, reviewID, now, false)
		return detail, nil
	}
	version, err := content.Version(ctx, *versionRecord)
	if err != nil {
		return tracker.ChangeDetail{}, err
	}
	var evidence []eventstore.Evidence
	if facts != nil {
		evidence = facts.Evidence
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		if err != nil {
			return tracker.ChangeDetail{}, projectRecordError(err)
		}
	}

	if len(evidence) > corestore.MaxPageSize {
		return tracker.ChangeDetail{}, corestore.ErrInvalidPage
	}
	detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{version}}
	priorApproval := false
	for _, row := range evidence {
		if row.OrganizationID != scope.organization || row.ChangeID != changeRecord.ID {
			return tracker.ChangeDetail{}, corestore.ErrTenantMismatch
		}
		currentReview := row.Kind == "review" && row.VersionID != nil && *row.VersionID == version.ID
		priorReview := row.Kind == "review" && row.Decision == "approved" && (row.VersionID == nil || *row.VersionID != version.ID) && !priorApproval
		currentCheck := row.Kind == "check" && row.VersionID != nil && *row.VersionID == version.ID
		if !currentReview && !priorReview && !currentCheck {
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
			if review.VersionID != valueOrEmpty(row.VersionID) || review.Decision != row.Decision || review.Actor.Kind != row.ActorKind || review.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(review.CreatedAt, row.CreatedAt) {
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
			if check.VersionID != valueOrEmpty(row.VersionID) || check.CheckRunID != valueOrEmpty(row.SourceKey) || check.Actor.Kind != row.ActorKind || check.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(check.ReceivedAt, row.CreatedAt) {
				return tracker.ChangeDetail{}, eventstore.ErrInvalidRecord
			}
			detail.Checks = append(detail.Checks, check)
		}
	}
	currentApproval, err := boardCurrentApproval(ctx, content, scope, policyRows, approval)
	if err != nil {
		return tracker.ChangeDetail{}, projectRecordError(err)
	}
	policyID, reviewPolicyID := changerequest.CurrentPolicyIdentities(&detail.Versions[0], currentApproval.Policy, currentApproval.ReviewPolicy)
	detail.Summary, err = s.summarizeChangeWithAuditDispositionsFor(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval, workload, facts)
	return detail, err
}

func sameTypedEvidenceTime(payload, typed time.Time) bool {
	return payload.UTC().Truncate(time.Microsecond).Equal(typed.UTC().Truncate(time.Microsecond))
}

func (s *Service) selectedLatestChangeSummary(ctx context.Context, scope nativeScope, item string, now time.Time) (tracker.NativeChangeReference, tracker.ChangeSummary, bool, error) {
	var latest eventstore.Change
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		latest, err = r.LatestChange(ctx, scope.project, tracker.NativeWorkItemID(item))
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, nil
	}
	if err != nil {
		return tracker.NativeChangeReference{}, tracker.ChangeSummary{}, false, projectRecordError(err)
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

func (s *Service) lockChangeRecordOperation(ctx context.Context, w tenantstore.Writer, scope nativeScope, item, changeID, versionID string) (eventstore.Change, error) {
	if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
		return eventstore.Change{}, err
	}
	if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
		return eventstore.Change{}, err
	}
	project, err := w.Project(ctx, scope.project)
	if err != nil {
		return eventstore.Change{}, err
	}
	if project.DeletedAt != nil {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	var issue corestore.WorkItem
	if scope.credential.Scope == apiScopeWorker {
		issue, err = w.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
	} else {
		issue, err = w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
	}
	if err != nil {
		return eventstore.Change{}, err
	}
	if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(issue.ActorPrincipalID)}) {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	var change eventstore.Change
	if scope.credential.Scope == apiScopeWorker {
		change, err = w.Change(ctx, changeID)
	} else {
		change, err = w.LockChange(ctx, changeID)
	}
	if err != nil {
		return eventstore.Change{}, err
	}
	if change.ProjectID != scope.project {
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

func (s *Service) changeOperationRecordReceipt(ctx context.Context, scope nativeScope, item, changeID, versionID, operation, key, hash string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, scope.credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, projectRecordError(err)
	}
	var raw json.RawMessage
	var payloadErr error
	if receipt.RequestHash == hash {
		raw, payloadErr = eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := s.lockChangeRecordOperation(ctx, w, scope, item, changeID, versionID); err != nil {
			return err
		}
		current, err := w.Receipt(ctx, scope.credential.ID, operation, key)
		if err != nil {
			return err
		}
		if current.RequestHash != hash {
			return nativeConflict(0)
		}
		if current.ResponseExpired || current.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= current.ResponseReplayUntilNs {
			return receiptstore.ErrExpired
		}
		if !reflect.DeepEqual(current, receipt) {
			return nativeConflict(0)
		}
		return payloadErr
	})
	if err != nil {
		return nil, false, projectRecordError(err)
	}
	return raw, true, nil
}
