package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) publishChangeVersionRecord(c echo.Context) error {
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	authority := s.config.ChangeVersionAuthority
	if authority == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	var request tracker.PublishChangeVersion
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if found {
		return c.JSONBlob(http.StatusOK, replay)
	}
	if err := validateChangeVersion(request.ChangeVersionInput); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateChangeVersionEvidence(scope, c.Param("item"), request); err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Source != nil {
		if err := request.Source.Validate(request.BaseSHA, request.HeadSHA, request.SourceBundle); err != nil {
			return s.nativeAPIError(c, nativeRejection(nativeInvalid(err.Error()), "version_validation", "source"))
		}
		if err := s.verifyChangeSource(ctx, *request.Source, request.SourceBundle); err != nil {
			return s.nativeAPIError(c, err)
		}
	} else if len(request.SourceBundle) > 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Source bundle requires immutable source metadata"))
	}
	change, err := s.readChangeRecord(ctx, scope, c.Param("item"), c.Param("change"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if change.WorkItemID != tracker.NativeWorkItemID(c.Param("item")) {
		return s.nativeAPIError(c, nativePublicInvalid("Publish through the Change Request's primary issue"))
	}
	if change.CurrentVersion != request.ExpectedVersionID {
		return s.nativeAPIError(c, nativeConflict(change.Revision))
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	approval, err := authority.Prepare(ctx, scope, change, request, now)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if approval.Policy.ID == "" || approval.Policy.ID != request.PolicyID || approval.ReviewPolicy.ID == "" || approval.ReviewPolicy.PolicyID != approval.Policy.ID || changerequest.PolicyID(approval.ReviewPolicy) != approval.ReviewPolicy.ID {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	if err := changerequest.ValidatePolicy(approval.ReviewPolicy, approval.Policy); err != nil {
		return s.nativeAPIError(c, nativeConflict(change.Revision))
	}
	checks := make([]tracker.ChangeCheckExpectation, 0, len(approval.ReviewPolicy.RequiredChecks))
	for _, spec := range approval.ReviewPolicy.RequiredChecks {
		checks = append(checks, tracker.ChangeCheckExpectation{ChangeCheckSpec: spec, CheckRunID: payloadNativeID(ctx, "check")})
	}
	version := tracker.ChangeVersion{ChangeVersionInput: request.ChangeVersionInput, ID: payloadNativeID(ctx, "version"), ChangeID: change.ID, Number: int64(change.Revision), Policy: approval.Policy, ReviewPolicy: approval.ReviewPolicy, Checks: checks, Actor: scope.actor(), CreatedAt: payloadTimestamp(ctx, now)}
	versionRaw, err := json.Marshal(version)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	versionRecord, err := content.PrepareVersion(ctx, scope.organization, versionRaw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Source != nil {
		sourceRef, err := content.PrepareSourceBundle(ctx, scope.organization, request.SourceBundle)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		versionRecord.SourceObjectKey = sourceRef.Key
		versionRecord.SourceSHA256 = sourceRef.SHA256
		versionRecord.SourceBytes = sourceRef.CompressedSize
		versionRecord.SourceContentBytes = sourceRef.UncompressedSize
	}
	response := versionRaw
	newChange := change
	newChange.CurrentVersion = version.ID
	newChange.Revision++
	newChange.UpdatedAt = payloadTimestamp(ctx, now)
	changeRaw, err := json.Marshal(newChange)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	changeRecord, err := content.PrepareChange(ctx, scope.organization, changeRaw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	events := make([]eventstore.Event, 0, len(change.LinkedIssues))
	for _, item := range change.LinkedIssues {
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: item, Type: "change.version_published", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: newChange.UpdatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}}})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		events = append(events, event)
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if s.config.AccessRepository != nil {
				if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
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
			current, err := w.Change(ctx, change.ID)
			if err != nil {
				return err
			}
			if current.Revision != int64(change.Revision) || current.CurrentVersionID != nil && *current.CurrentVersionID != change.CurrentVersion {
				return corestore.ErrRevisionConflict
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if err := authority.Authorize(ctx, w, scope, change, request, approval, now); err != nil {
				return err
			}
			for _, item := range change.LinkedIssues {
				if _, err := w.WorkItem(ctx, scope.project, item); err != nil {
					return err
				}
			}
			if _, err := w.CreateVersion(ctx, versionRecord); err != nil {
				return err
			}
			update := changeRecord
			update.Revision = int64(change.Revision)
			if _, err := w.UpdateChange(ctx, update); err != nil {
				return err
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
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return c.JSONBlob(http.StatusOK, response)
}
