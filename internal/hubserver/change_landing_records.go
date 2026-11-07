package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (s *Service) landChangeRecord(c echo.Context) error {
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	var request tracker.LandChangeVersion
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	if !changerequest.ValidHash(request.MergeSHA, 40) && !changerequest.ValidHash(request.MergeSHA, 64) {
		return s.nativeAPIError(c, nativePublicInvalid("The landed commit must be a lowercase commit identity"))
	}
	if strings.TrimSpace(request.BaseRef) == "" || len(request.BaseRef) > 256 || !slices.Contains([]string{"squash", "merge", "rebase"}, request.Method) {
		return s.nativeAPIError(c, nativePublicInvalid("Landing names the base branch and a merge method of squash, merge or rebase"))
	}
	if request.Receipt != nil && (!request.Receipt.Landed || request.Receipt.Waiting || request.Receipt.GateFailed || request.Receipt.Refusal != "") {
		return s.nativeAPIError(c, nativePublicInvalid("Landing receipt must attest a completed landing"))
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
	change, err := s.readChangeRecord(ctx, scope, c.Param("item"), c.Param("change"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if change.WorkItemID != tracker.NativeWorkItemID(c.Param("item")) {
		return s.nativeAPIError(c, nativePublicInvalid("Land through the Change Request's primary issue"))
	}
	var changeRow eventstore.Change
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		changeRow, err = r.Change(ctx, change.ID)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if changeRow.OrganizationID != scope.organization || changeRow.ProjectID != scope.project || changeRow.WorkItemID != change.WorkItemID || changeRow.Revision != int64(change.Revision) || changeRow.CurrentVersionID == nil || *changeRow.CurrentVersionID != change.CurrentVersion {
		return s.nativeAPIError(c, nativeConflict(change.Revision))
	}
	versionRow, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	version, err := eventstore.NewContent(s.runDetailStore()).Version(ctx, versionRow)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if request.Receipt != nil && (request.Receipt.VersionID != version.ID || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA || request.Receipt.BaseRef != request.BaseRef) {
		return s.nativeAPIError(c, nativePublicInvalid("Landing receipt identities differ from the reviewed version"))
	}
	if change.CurrentVersion != version.ID {
		if landed := change.CurrentLanding(); landed != nil && landed.VersionID == version.ID && landed.MergeSHA == request.MergeSHA {
			return s.recordExistingLandingReceipt(c, ctx, scope, operation, hash, request, change)
		}
		return s.nativeAPIError(c, nativeConflict(change.Revision))
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	detail, err := s.selectedChangeDetail(ctx, scope, c.Param("item"), change.ID, now)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if detail.Summary.Status != "reviewed" {
		return s.nativeAPIError(c, &nativeError{Code: "not_reviewed", Message: "Only a reviewed current version lands: " + strings.Join(detail.Summary.Messages, " "), status: http.StatusConflict})
	}
	itemRow, itemVersion, err := s.readWorkItemRecord(ctx, scope, string(change.WorkItemID))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	issue, err := s.hydrateWorkItemRecord(ctx, itemRow, itemVersion)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var project projectstore.Record
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(ctx, scope.project)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	delivery := issue.ReleaseDelivery
	associated := delivery != nil
	if associated && (delivery.ChangeID != change.ID || delivery.VersionID != version.ID || delivery.HeadSHA != version.HeadSHA || delivery.BaseRef != request.BaseRef || version.External == nil || version.External.Provider != "github" || version.External.ID != strconv.Itoa(delivery.PRNumber) && delivery.PRNumber > 0) {
		return s.nativeAPIError(c, nativePublicInvalid("Landing must cover the associated release version and branch"))
	}
	if associated && (request.Receipt == nil || request.Receipt.BaseSHA != delivery.BaseSHA || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA) {
		return s.nativeAPIError(c, nativePublicInvalid("Release landing must retain the reviewed base and head"))
	}
	if err := s.verifyChangeLandingIncorporation(ctx, version, request); err != nil {
		return s.nativeAPIError(c, err)
	}
	auditHead := request.MergeSHA
	if request.Receipt != nil && request.Receipt.Incorporation != nil {
		auditHead = version.HeadSHA
	}
	if err := validateChangeLandingSecurityAudit(ctx, detail, scope, version, request, associated, auditHead); err != nil {
		return s.nativeAPIError(c, err)
	}
	quality, err := changeLandingQuality(issue, detail, version, now)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	change.Landed = &tracker.ChangeLanding{Quality: quality, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: request.MergeSHA, BaseRef: request.BaseRef, Method: request.Method, Actor: scope.actor(), LandedAt: payloadTimestamp(ctx, now), Rebased: request.Rebased}
	change.UpdatedAt = payloadTimestamp(ctx, now)
	change.Revision++
	content := eventstore.NewContent(s.runDetailStore())
	changeRaw, err := json.Marshal(change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	changeRecord, err := content.PrepareChange(ctx, scope.organization, changeRaw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var landingReceipt *eventstore.LandingReceipt
	if request.Receipt != nil {
		if version.AttemptID == "" {
			return s.nativeAPIError(c, nativePublicInvalid("Landing receipts require the source attempt bound by the immutable version"))
		}
		record, err := content.PrepareLandingReceipt(ctx, eventstore.LandingReceipt{OrganizationID: scope.organization, AttemptID: version.AttemptID, VersionID: version.ID, Repository: version.Repository, BaseRef: request.BaseRef}, *request.Receipt)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		landingReceipt = &record
	}
	response, err := json.Marshal(change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	mutationReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var event eventstore.Event
	var nextIssue *tracker.NativeIssue
	var nextItem *corestore.WorkItem
	var nextItemVersion *eventstore.CollaborationVersion
	fromState := stringValue(itemRow.WorkflowStateName)
	if !issue.Terminal {
		projectWorkflow := tracker.NativeProject{States: project.States}
		target := terminalTarget(projectWorkflow, fromState)
		if target == "" {
			return s.nativeAPIError(c, nativeInvalid("The workflow allows no move from "+fromState+" to a terminal lane"))
		}
		updatedIssue := issue
		updatedIssue.State = target
		updatedIssue.Revision++
		updatedIssue.UpdatedAt = payloadTimestamp(ctx, now)
		updatedRow := itemRow
		updatedRow.WorkflowStateName = &target
		updatedRow.LastActivityAt = payloadTimestamp(ctx, now)
		updated := tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: payloadTimestamp(ctx, now), Data: tracker.CollaborationData{Revision: tracker.Revision(itemRow.Revision), FromState: fromState, ToState: target, Reason: "worker_progress"}}
		raw, err := json.Marshal(updated)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		event, err = content.PrepareEvent(ctx, scope.organization, raw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		issueRaw, err := json.Marshal(updatedIssue)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		itemVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: change.WorkItemID, RecordID: string(change.WorkItemID), Revision: int64(updatedIssue.Revision)}, issueRaw)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		nextIssue, nextItem, nextItemVersion = &updatedIssue, &updatedRow, &itemVersion
	}

	changeRecord.Revision = changeRow.Revision
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return nativeConflict(change.Revision - 1)
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			current, err := w.LockChange(ctx, change.ID)
			if err != nil {
				return err
			}
			if current.Revision != changeRow.Revision || current.EvidenceSequence != changeRow.EvidenceSequence || current.CurrentVersionID == nil || *current.CurrentVersionID != version.ID {
				return corestore.ErrRevisionConflict
			}
			currentVersion, err := w.Version(ctx, change.ID, version.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(currentVersion, versionRow) {
				return corestore.ErrRevisionConflict
			}
			currentItem, err := w.WorkItem(ctx, scope.project, change.WorkItemID)
			if err != nil {
				return err
			}
			if currentItem.Revision != itemRow.Revision {
				return corestore.ErrRevisionConflict
			}
			currentProject, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(currentProject, project) {
				return corestore.ErrRevisionConflict
			}
			policyRows, err := readCurrentChangePolicyRows(ctx, w, scope)
			if err != nil {
				return err
			}
			if policyRows.Project.PolicyID != version.Policy.ID || policyRows.hasReview && policyRows.Review.ReviewPolicyID != version.ReviewPolicy.ID || !policyRows.hasReview && defaultChangeReviewPolicy(version.Policy).ID != version.ReviewPolicy.ID {
				return policyMismatch("Current approved policy changed; the reviewed version must be republished")
			}
			if scope.credential.Scope == apiScopeWorker {
				locker, ok := w.(leasestore.FencedLeaseLocker)
				if !ok {
					return nativeRunEventStorageUnavailable()
				}
				if _, err := locker.LockLease(ctx, string(request.LeaseID)); err != nil {
					return err
				}
				if err := requireLinkedIssueLease(ctx, w, scope, currentItem, request.Mutation, s.config.now().UTC()); err != nil {
					return err
				}
			}
			if landingReceipt != nil {
				if _, err := w.CreateLandingReceipt(ctx, *landingReceipt); err != nil {
					return err
				}
			}
			if _, err := w.UpdateChange(ctx, changeRecord); err != nil {
				return err
			}
			if nextIssue != nil {
				if _, err := w.CreateCollaborationVersion(ctx, *nextItemVersion); err != nil {
					return err
				}
				storedEvent, err := w.AppendEvent(ctx, event)
				if err != nil {
					return err
				}
				nextItem.EventSequence = storedEvent.Sequence
				if _, err := w.UpdateWorkItem(ctx, *nextItem); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, mutationReceipt)
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

func (s *Service) recordExistingLandingReceipt(c echo.Context, ctx context.Context, scope nativeScope, operation, hash string, request tracker.LandChangeVersion, change tracker.ChangeRequest) error {
	response, err := json.Marshal(change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	receipt, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: change.UpdatedAt}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		current, err := w.Change(ctx, change.ID)
		if err != nil {
			return err
		}
		if current.CurrentVersionID == nil || *current.CurrentVersionID != change.CurrentVersion {
			return corestore.ErrRevisionConflict
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	}); err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) verifyChangeLandingIncorporation(ctx context.Context, version tracker.ChangeVersion, request tracker.LandChangeVersion) error {
	if request.Receipt == nil || request.Receipt.Incorporation == nil {
		return nil
	}
	proof := request.Receipt.Incorporation
	if proof.SourceBaseSHA != version.BaseSHA || proof.SourceHeadSHA != version.HeadSHA || proof.TargetHeadSHA != request.MergeSHA || !changerequest.ValidHash(proof.TargetTreeSHA, len(version.HeadSHA)) || proof.MergeTreeSHA != proof.TargetTreeSHA || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA {
		return nativePublicInvalid("landing incorporation proof must bind the reviewed range to the target tree")
	}
	select {
	case s.sourceVerification <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := workspace.VerifyLandingIncorporation(ctx, *proof, request.IncorporationObjects)
	<-s.sourceVerification
	if err != nil {
		return nativePublicInvalid("landing incorporation objects do not prove the reviewed range")
	}
	return nil
}

func validateChangeLandingSecurityAudit(ctx context.Context, detail tracker.ChangeDetail, scope nativeScope, version tracker.ChangeVersion, request tracker.LandChangeVersion, associated bool, auditHead string) error {
	if !version.Policy.Gates.SecurityAudit || version.Policy.Gates.GitHubPullRequest || associated {
		return nil
	}
	if request.Receipt == nil || request.Receipt.SecurityAudit == nil || request.Receipt.BaseSHA == "" || request.Receipt.MergeSHA != request.MergeSHA || request.Receipt.Incorporation != nil && request.Receipt.BaseSHA != version.BaseSHA {
		return nativePublicInvalid("landing requires security audit evidence for its exact base and head")
	}
	audit := *request.Receipt.SecurityAudit
	securityaudit.NormalizeAttribution(&audit.Run)
	evaluation := securityaudit.Evaluate(audit.Run, nil, securityaudit.Key{ProjectID: string(scope.project), Repository: version.Repository, PRNumber: audit.Run.PRNumber, BaseSHA: request.Receipt.BaseSHA, HeadSHA: auditHead}, "", []string{"none"})
	auditRaw, err := marshalNative(audit)
	if err != nil {
		return err
	}
	recorded := false
	for _, review := range detail.Reviews {
		if review.VersionID != version.ID || review.SecurityAudit == nil || !review.SecurityAudit.Landing || review.Decision != "approved" {
			continue
		}
		reviewRaw, err := marshalNative(review.SecurityAudit)
		if err != nil {
			return err
		}
		if auditRaw == reviewRaw {
			recorded = true
		}
	}
	if !evaluation.Allowed || audit.Verdict != "pass" || !audit.Landing || !recorded {
		return nativePublicInvalid("landing security audit does not match recorded approval for the integrated change")
	}
	return nil
}

func changeLandingQuality(issue tracker.NativeIssue, detail tracker.ChangeDetail, version tracker.ChangeVersion, at time.Time) (*tracker.LandingQualitySnapshot, error) {
	contract, err := config.ResolvePolicyIssueContract(version.Policy)
	if err != nil {
		return nil, err
	}
	snapshot := &tracker.LandingQualitySnapshot{IssueRevision: issue.Revision, IssueBody: issue.Body, Criteria: contract.AcceptanceCriteria(issue.Body), Missing: contract.Evaluate(nativeContractIssue(issue)).Missing, Evidence: []gate.CriterionEvidence{}}
	for index := len(detail.Reviews) - 1; index >= 0; index-- {
		review := detail.Reviews[index]
		if review.CreatedAt.After(at) || review.Decision != "approved" || review.Validator == nil {
			continue
		}
		snapshot.ReviewID = review.ID
		snapshot.Evidence = review.Validator.CriteriaEvidence
		break
	}
	return snapshot, nil
}
