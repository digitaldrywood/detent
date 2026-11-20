package changelanding

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
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (s *Landing) LandChangeRecord(c echo.Context) error {
	ctx := hubcontent.IsolateContentContext(s.deps.RunDetailContext(c.Request().Context()))
	scope := hubscope.NativeRequestScope(c)
	var request tracker.LandChangeVersion
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	if !changerequest.ValidHash(request.MergeSHA, 40) && !changerequest.ValidHash(request.MergeSHA, 64) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("The landed commit must be a lowercase commit identity"))
	}
	if strings.TrimSpace(request.BaseRef) == "" || len(request.BaseRef) > 256 || !slices.Contains([]string{"squash", "merge", "rebase"}, request.Method) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Landing names the base branch and a merge method of squash, merge or rebase"))
	}
	if request.Receipt != nil && (!request.Receipt.Landed || request.Receipt.Waiting || request.Receipt.GateFailed || request.Receipt.Refusal != "") {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Landing receipt must attest a completed landing"))
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	replay, found, err := s.deps.ChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
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
	if change.WorkItemID != tracker.NativeWorkItemID(c.Param("item")) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Land through the Change Request's primary issue"))
	}
	var changeRow eventstore.Change
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		changeRow, err = r.Change(ctx, change.ID)
		return err
	})
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	if changeRow.OrganizationID != scope.Organization || changeRow.ProjectID != scope.Project || changeRow.WorkItemID != change.WorkItemID || changeRow.Revision != int64(change.Revision) || changeRow.CurrentVersionID == nil || *changeRow.CurrentVersionID != change.CurrentVersion {
		return s.deps.NativeAPIError(c, hubapi.NativeConflict(change.Revision))
	}
	versionRow, err := s.deps.ReadChangeVersionRecord(ctx, scope.Organization, change.ID, c.Param("version"))
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	version, err := eventstore.NewContent(s.deps.RunDetailStore()).Version(ctx, versionRow)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if request.Receipt != nil && (request.Receipt.VersionID != version.ID || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA || request.Receipt.BaseRef != request.BaseRef) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Landing receipt identities differ from the reviewed version"))
	}
	if change.CurrentVersion != version.ID {
		if landed := change.CurrentLanding(); landed != nil && landed.VersionID == version.ID && landed.MergeSHA == request.MergeSHA {
			return s.recordExistingLandingReceipt(c, ctx, scope, operation, hash, request, change)
		}
		return s.deps.NativeAPIError(c, hubapi.NativeConflict(change.Revision))
	}
	now, err := s.deps.CurrentTime()
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var reviewedPolicy hubrecords.CurrentChangePolicyRows
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reviewedPolicy, err = changerules.ReadCurrentChangePolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	detail, err := s.deps.SelectedChangeDetail(ctx, scope, c.Param("item"), change.ID, now)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if detail.Summary.Status != "reviewed" {
		return s.deps.NativeAPIError(c, &hubapi.NativeError{Code: "not_reviewed", Message: "Only a reviewed current version lands: " + strings.Join(detail.Summary.Messages, " "), Status: http.StatusConflict})
	}
	itemRow, itemVersion, err := s.deps.ReadWorkItemRecord(ctx, scope, string(change.WorkItemID))
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	issue, err := s.deps.HydrateWorkItemRecord(ctx, itemRow, itemVersion)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var project projectstore.Record
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(ctx, scope.Project)
		return err
	})
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	delivery := issue.ReleaseDelivery
	associated := delivery != nil
	if associated && (delivery.ChangeID != change.ID || delivery.VersionID != version.ID || delivery.HeadSHA != version.HeadSHA || delivery.BaseRef != request.BaseRef || version.External == nil || version.External.Provider != "github" || version.External.ID != strconv.Itoa(delivery.PRNumber) && delivery.PRNumber > 0) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Landing must cover the associated release version and branch"))
	}
	if associated && (request.Receipt == nil || request.Receipt.BaseSHA != delivery.BaseSHA || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA) {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Release landing must retain the reviewed base and head"))
	}
	if err := s.verifyChangeLandingIncorporation(ctx, version, request); err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	auditHead := request.MergeSHA
	if request.Receipt != nil && request.Receipt.Incorporation != nil {
		auditHead = version.HeadSHA
	}
	if err := validateChangeLandingSecurityAudit(ctx, detail, scope, version, request, associated, auditHead); err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	quality, err := changeLandingQuality(issue, detail, version, now)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	change.Landed = &tracker.ChangeLanding{Quality: quality, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: request.MergeSHA, BaseRef: request.BaseRef, Method: request.Method, Actor: scope.Actor(), LandedAt: hubcontent.PayloadTimestamp(ctx, now), Rebased: request.Rebased}
	change.UpdatedAt = hubcontent.PayloadTimestamp(ctx, now)
	change.Revision++
	content := eventstore.NewContent(s.deps.RunDetailStore())
	changeRaw, err := json.Marshal(change)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	changeRecord, err := content.PrepareChange(ctx, scope.Organization, changeRaw)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	versionLanding, err := eventstore.VersionLandingFromChange(changeRecord)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var landingAttempt leasestore.Attempt
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			landingAttempt, _, err = s.deps.RunnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(change.WorkItemID), false)
			return err
		})
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
	}
	var landingReceipt *eventstore.LandingReceipt
	if request.Receipt != nil {
		if version.AttemptID == "" && landingAttempt.ID == "" {
			return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("Landing receipts require the source attempt bound by the immutable version"))
		}
		receiptAttemptID := version.AttemptID
		if landingAttempt.ID != "" {
			receiptAttemptID = landingAttempt.ID
		}
		record, err := content.PrepareLandingReceipt(ctx, eventstore.LandingReceipt{OrganizationID: scope.Organization, AttemptID: receiptAttemptID, VersionID: version.ID, Repository: version.Repository, BaseRef: request.BaseRef}, *request.Receipt)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		landingReceipt = &record
	}
	response, err := json.Marshal(change)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	mutationReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	var event eventstore.Event
	var nextIssue *tracker.NativeIssue
	var nextItem *corestore.WorkItem
	var nextItemVersion *eventstore.CollaborationVersion
	fromState := hubapi.StringValue(itemRow.WorkflowStateName)
	if !issue.Terminal {
		projectWorkflow := tracker.NativeProject{States: project.States}
		target := changerules.TerminalTarget(projectWorkflow, fromState)
		if target == "" {
			return s.deps.NativeAPIError(c, hubapi.NativeInvalid("The workflow allows no move from "+fromState+" to a terminal lane"))
		}
		updatedIssue := issue
		updatedIssue.State = target
		updatedIssue.Terminal = hubrecords.TenantProjectStateTerminal(project, target)
		updatedIssue.ClosedAt = &now
		updatedIssue.Revision++
		updatedIssue.UpdatedAt = hubcontent.PayloadTimestamp(ctx, now)
		updatedIssue.LastActivityAt = updatedIssue.UpdatedAt
		updatedRow := itemRow
		updatedRow.WorkflowStateName = &target
		updatedRow.LastActivityAt = updatedIssue.LastActivityAt
		updatedRow.NativeUpdatedAt = updatedIssue.UpdatedAt
		updated := tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: change.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: hubcontent.PayloadTimestamp(ctx, now), Data: tracker.CollaborationData{Revision: tracker.Revision(itemRow.Revision), FromState: fromState, ToState: target, Reason: "worker_progress", Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}}}
		raw, err := json.Marshal(updated)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		event, err = content.PrepareEvent(ctx, scope.Organization, raw)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		issueRaw, err := json.Marshal(updatedIssue)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		itemVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: change.WorkItemID, RecordID: string(change.WorkItemID), Revision: int64(updatedIssue.Revision)}, issueRaw)
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		nextIssue, nextItem, nextItemVersion = &updatedIssue, &updatedRow, &itemVersion
	}

	changeRecord.Revision = changeRow.Revision
	for {
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if scope.Credential.Scope == hubapi.APIScopeWorker {
				if _, err := w.RunnerForLease(ctx, string(request.LeaseID)); err != nil {
					return err
				}
			}
			if err := s.deps.AuthorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return hubapi.NativeConflict(change.Revision - 1)
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
			readItem := w.WorkItem
			if scope.Credential.Scope == hubapi.APIScopeWorker && nextIssue != nil {
				readItem = w.LockWorkItem
			}
			currentItem, err := readItem(ctx, scope.Project, change.WorkItemID)
			if err != nil {
				return err
			}
			if currentItem.Revision != itemRow.Revision {
				return corestore.ErrRevisionConflict
			}
			currentProject, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(currentProject, project) {
				return corestore.ErrRevisionConflict
			}
			policyRows, err := changerules.ReadCurrentChangePolicyRows(ctx, w, scope)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(policyRows, reviewedPolicy) {
				return hubapi.PolicyMismatch("Current approved policy changed; the reviewed version must be republished")
			}
			if scope.Credential.Scope == hubapi.APIScopeWorker {
				locker, ok := w.(leasestore.FencedLeaseLocker)
				if !ok {
					return hubapi.NativeRunEventStorageUnavailable()
				}
				if _, err := locker.LockLease(ctx, string(request.LeaseID)); err != nil {
					return err
				}
				current, _, err := s.deps.RunnerSessionFence(ctx, w, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(change.WorkItemID), true)
				if err != nil {
					return err
				}
				if current.ID != landingAttempt.ID {
					return corestore.ErrRevisionConflict
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
			if err := w.RecordVersionLanding(ctx, versionLanding); err != nil {
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
				if scope.Credential.Scope == hubapi.APIScopeWorker {
					if err := w.AdvanceLeaseWorkItemRevision(ctx, leasestore.Fence{LeaseID: string(request.LeaseID), FencingToken: int64(request.FencingToken)}, itemRow.ID, itemRow.Revision, int64(nextIssue.Revision)); err != nil {
						return err
					}
				}

			}
			_, err = w.PutReceipt(ctx, mutationReceipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.deps.ChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
			if readErr != nil {
				return s.deps.NativeAPIError(c, readErr)
			}
			if found {
				return c.JSONBlob(http.StatusOK, replay)
			}
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return s.deps.NativeAPIError(c, hubapi.NativeConflict(change.Revision))
		}
		if err != nil {
			return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
		}
		break
	}
	select {
	case s.deps.BoardRefresh <- struct{}{}:
	default:
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Landing) recordExistingLandingReceipt(c echo.Context, ctx context.Context, scope hubscope.NativeScope, operation, hash string, request tracker.LandChangeVersion, change tracker.ChangeRequest) error {
	response, err := json.Marshal(change)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	receipt, err := eventstore.NewContent(s.deps.RunDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: change.UpdatedAt}, response)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	if err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.deps.AuthorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
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
		return s.deps.NativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Landing) verifyChangeLandingIncorporation(ctx context.Context, version tracker.ChangeVersion, request tracker.LandChangeVersion) error {
	if request.Receipt == nil || request.Receipt.Incorporation == nil {
		return nil
	}
	proof := request.Receipt.Incorporation
	if proof.SourceBaseSHA != version.BaseSHA || proof.SourceHeadSHA != version.HeadSHA || proof.TargetHeadSHA != request.MergeSHA || !changerequest.ValidHash(proof.TargetTreeSHA, len(version.HeadSHA)) || proof.MergeTreeSHA != proof.TargetTreeSHA || request.Receipt.HeadSHA != version.HeadSHA || request.Receipt.MergeSHA != request.MergeSHA {
		return hubapi.NativePublicInvalid("landing incorporation proof must bind the reviewed range to the target tree")
	}
	select {
	case s.deps.SourceVerification <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := workspace.VerifyLandingIncorporation(ctx, *proof, request.IncorporationObjects)
	<-s.deps.SourceVerification
	if err != nil {
		return hubapi.NativePublicInvalid("landing incorporation objects do not prove the reviewed range")
	}
	return nil
}

func validateChangeLandingSecurityAudit(ctx context.Context, detail tracker.ChangeDetail, scope hubscope.NativeScope, version tracker.ChangeVersion, request tracker.LandChangeVersion, associated bool, auditHead string) error {
	if !version.Policy.Gates.SecurityAudit || version.Policy.Gates.GitHubPullRequest || associated {
		return nil
	}
	if request.Receipt == nil || request.Receipt.SecurityAudit == nil || request.Receipt.BaseSHA == "" || request.Receipt.MergeSHA != request.MergeSHA || request.Receipt.Incorporation != nil && request.Receipt.BaseSHA != version.BaseSHA {
		return hubapi.NativePublicInvalid("landing requires security audit evidence for its exact base and head")
	}
	audit := *request.Receipt.SecurityAudit
	securityaudit.NormalizeAttribution(&audit.Run)
	evaluation := securityaudit.Evaluate(audit.Run, nil, securityaudit.Key{ProjectID: string(scope.Project), Repository: version.Repository, PRNumber: audit.Run.PRNumber, BaseSHA: request.Receipt.BaseSHA, HeadSHA: auditHead}, "", []string{"none"})
	auditRaw, err := hubapi.MarshalNative(audit)
	if err != nil {
		return err
	}
	recorded := false
	for _, review := range detail.Reviews {
		if review.VersionID != version.ID || review.SecurityAudit == nil || !review.SecurityAudit.Landing || review.Decision != "approved" {
			continue
		}
		reviewRaw, err := hubapi.MarshalNative(review.SecurityAudit)
		if err != nil {
			return err
		}
		if auditRaw == reviewRaw {
			recorded = true
		}
	}
	if !evaluation.Allowed || audit.Verdict != "pass" || !audit.Landing || !recorded {
		return hubapi.NativePublicInvalid("landing security audit does not match recorded approval for the integrated change")
	}
	return nil
}

func changeLandingQuality(issue tracker.NativeIssue, detail tracker.ChangeDetail, version tracker.ChangeVersion, at time.Time) (*tracker.LandingQualitySnapshot, error) {
	contract, err := config.ResolvePolicyIssueContract(version.Policy)
	if err != nil {
		return nil, err
	}
	snapshot := &tracker.LandingQualitySnapshot{IssueRevision: issue.Revision, IssueBody: issue.Body, Criteria: contract.AcceptanceCriteria(issue.Body), Missing: contract.Evaluate(hubrecords.NativeContractIssue(issue)).Missing, Evidence: []gate.CriterionEvidence{}}
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
