package changeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *VersionPublication) PublishChangeVersionRecord(c echo.Context) error {
	ctx := hubcontent.IsolateContentContext(s.deps.RunDetailContext(c.Request().Context()))
	var request tracker.PublishChangeVersion
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	raw, err := s.PublishChangeVersionRecordCommand(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Param("change"), operation, request)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *VersionPublication) PublishChangeVersionRecordCommand(ctx context.Context, scope hubscope.NativeScope, item, changeID, operation string, request tracker.PublishChangeVersion) (json.RawMessage, error) {
	authority := s.deps.Authority
	if authority == nil {
		if s.deps.Repository == nil {
			return nil, hubapi.NativeWorkItemStorageUnavailable()
		}
		authority = s.deps.DefaultAuthority()
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	replay, found, err := s.deps.ChangeOperationRecordReceipt(ctx, scope, item, changeID, "", operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	if err := s.deps.ValidateChangeVersion(request.ChangeVersionInput); err != nil {
		if visualErr := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			return s.deps.ValidateVersionAttachments(ctx, r, scope, "", request.Artifacts)
		}); visualErr != nil {
			return nil, hubapi.ProjectRecordError(visualErr)
		}
		return nil, err
	}
	if err := s.deps.ValidateChangeVersionEvidence(scope, item, request); err != nil {
		return nil, err
	}
	change, err := s.deps.ReadChangeRecord(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	if change.WorkItemID != tracker.NativeWorkItemID(item) {
		return nil, hubapi.NativePublicInvalid("Publish through the Change Request's primary issue")
	}
	if change.CurrentVersion != request.ExpectedVersionID {
		return nil, hubapi.NativeConflict(change.Revision)
	}
	if err := validateImplementationContent(scope, request); err != nil {
		return nil, err
	}
	sourceRun, err := s.PrepareChangeVersionRun(ctx, scope, change, request)
	if errors.Is(err, errNoChangeVersionRun) {
		sourceRun, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	evidenceIDs, err := implementationEvidenceIDs(scope, request, sourceRun)
	if err != nil {
		return nil, err
	}
	if request.Source != nil {
		if err := request.Source.Validate(request.BaseSHA, request.HeadSHA, request.SourceBundle); err != nil {
			return nil, s.deps.NativeRejection(hubapi.NativeInvalid(err.Error()), "version_validation", "source")
		}
		if err := s.deps.VerifyChangeSource(ctx, *request.Source, request.SourceBundle); err != nil {
			return nil, err
		}
	} else if len(request.SourceBundle) > 0 {
		return nil, hubapi.NativePublicInvalid("Source bundle requires immutable source metadata")
	}
	suppliedArtifacts := request.Artifacts
	var evidence []tracker.ChangeArtifact
	if err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if len(evidenceIDs) > 0 {
			var err error
			evidence, err = readImplementationEvidence(ctx, scope, change.WorkItemID, evidenceIDs, r.Attachment)
			if err != nil {
				return err
			}
			request.Artifacts = append(request.Artifacts, evidence...)
		}
		return s.deps.ValidateVersionAttachments(ctx, r, scope, request.Outcome, suppliedArtifacts)
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	now, err := s.deps.CurrentTime()
	if err != nil {
		return nil, err
	}
	approval, err := authority.Prepare(ctx, scope, change, request, now)
	if err != nil {
		return nil, err
	}
	if approval.Policy.ID == "" || approval.Policy.ID != request.PolicyID || approval.ReviewPolicy.ID == "" || approval.ReviewPolicy.PolicyID != approval.Policy.ID || changerequest.PolicyID(approval.ReviewPolicy) != approval.ReviewPolicy.ID {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := changerequest.ValidatePolicy(approval.ReviewPolicy, approval.Policy); err != nil {
		return nil, hubapi.NativeConflict(change.Revision)
	}
	checks := make([]tracker.ChangeCheckExpectation, 0, len(approval.ReviewPolicy.RequiredChecks))
	for _, spec := range approval.ReviewPolicy.RequiredChecks {
		checks = append(checks, tracker.ChangeCheckExpectation{ChangeCheckSpec: spec, CheckRunID: hubcontent.PayloadNativeID(ctx, "check")})
	}
	version := tracker.ChangeVersion{ChangeVersionInput: request.ChangeVersionInput, ID: hubcontent.PayloadNativeID(ctx, "version"), ChangeID: change.ID, Number: int64(change.Revision), Policy: approval.Policy, ReviewPolicy: approval.ReviewPolicy, Checks: checks, Actor: scope.Actor(), CreatedAt: hubcontent.PayloadTimestamp(ctx, now)}
	versionRaw, err := json.Marshal(version)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	versionRecord, err := content.PrepareVersion(ctx, scope.Organization, versionRaw)
	if err != nil {
		return nil, err
	}
	versionRecord.ImplementingSessionID = -1
	if sourceRun != nil {
		versionRecord.ImplementingSessionID = sourceRun.sessionID
		versionRecord.ImplementingMachineID = sourceRun.machineID
	}
	if request.Source != nil {
		sourceRef, err := content.PrepareSourceBundle(ctx, scope.Organization, request.SourceBundle)
		if err != nil {
			return nil, err
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
	newChange.UpdatedAt = hubcontent.PayloadTimestamp(ctx, now)
	changeRaw, err := json.Marshal(newChange)
	if err != nil {
		return nil, err
	}
	changeRecord, err := content.PrepareChange(ctx, scope.Organization, changeRaw)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	events := make([]eventstore.Event, 0, len(change.LinkedIssues))
	for _, item := range change.LinkedIssues {
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: item, Type: "change.version_published", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: newChange.UpdatedAt, Data: tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}}})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	var transition *changelanding.WorkflowTransition
	candidate := change
	candidate.CurrentVersion = version.ID
	policyID, reviewID := changerequest.CurrentPolicyIdentities(&version, approval.Policy, approval.ReviewPolicy)
	if changerequest.SummarizeCurrentVersion(tracker.ChangeDetail{Change: candidate, Versions: []tracker.ChangeVersion{version}}, policyID, reviewID, now, false).Status == "reviewed" {
		transition, err = s.deps.PrepareChangeWorkflowTransition(ctx, scope, change, approval, false, now)
		if err != nil {
			return nil, err
		}
	}
	for {
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.deps.LockRunnerCredential(ctx, w, scope); err != nil {
				return err
			}
			transitionReady, err := s.deps.LockChangeWorkflowTransition(ctx, w, scope, transition)
			if err != nil {
				return err
			}
			if _, err := s.deps.LockChangeRecordOperation(ctx, w, scope, item, changeID, ""); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey); err == nil {
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
			if err := authority.Authorize(ctx, w, scope, change, request, approval, now); err != nil {
				return err
			}
			if err := s.recheckChangeVersionRun(ctx, w, sourceRun); err != nil {
				return err
			}
			if err := bindImplementationEvidence(ctx, w, scope, change.WorkItemID, evidenceIDs, evidence); err != nil {
				return err
			}
			if err := s.deps.BindVersionAttachments(ctx, w, scope, item, suppliedArtifacts); err != nil {
				return err
			}
			for _, item := range change.LinkedIssues {
				if _, err := w.WorkItem(ctx, scope.Project, item); err != nil {
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
			if transitionReady {
				if err := s.deps.ApplyChangeWorkflowTransition(ctx, w, scope, transition); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.deps.ChangeOperationRecordReceipt(ctx, scope, item, changeID, "", operation, request.IdempotencyKey, hash)
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
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
	case s.deps.BoardRefresh <- struct{}{}:
	default:
	}
	return response, nil
}
