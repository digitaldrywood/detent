package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) transitionNativeIssueTenant(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.Transition) (json.RawMessage, error) {
	return s.transitionNativeIssueTenantFromChat(ctx, scope, item, request, "")
}

func (s *Service) transitionNativeIssueTenantFromChat(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.Transition, conversationID string) (json.RawMessage, error) {
	result, err := s.transitionNativeIssueTenantWithStop(ctx, scope, item, request, nil, conversationID)
	if err == nil && s.wakeSpriteRunnersAfter != nil {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}

func (s *Service) transitionNativeIssueTenantWithStop(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.Transition, stop *coordinatorStopRunArguments, conversationID string) (json.RawMessage, error) {
	return s.prepareOrTransitionNativeIssue(ctx, scope, item, request, stop, conversationID, nil)
}

type nativeRecordedRecoveryWrite struct {
	recovery *leasestore.RecordedRecovery
	audit    *releaseaudits.AuditHoldClearance
	write    func(context.Context, tenantstore.Writer) error
}

func (s *Service) prepareOrTransitionNativeIssue(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.Transition, stop *coordinatorStopRunArguments, conversationID string, claim *nativeRecordedRecoveryWrite) (json.RawMessage, error) {
	batchRouting := scope.IntegrationOwned && scope.IntegrationRepository != "" && scope.GitHubIntakeRevision != nil && scope.GitHubIntakeRevision.BatchID != "" && scope.GitHubIntakeRevision.Revision > 0 && scope.Credential.Scope == hubapi.APIScopeAdmin && scope.Credential.Runner.RunnerID == "" && request.Reason == "user_requested" && request.State != "Cancelled" && stop == nil
	if batchRouting {
		batch, err := s.loadGitHubBatchRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		actor := scope.Actor()
		if batch.ID != scope.GitHubIntakeRevision.BatchID || batch.Revision != scope.GitHubIntakeRevision.Revision || batch.Status != "importing" || batch.RequestedBy == nil || batch.RequestedBy.Kind != actor.Kind || batch.RequestedBy.PrincipalID != actor.PrincipalID || batch.Discovery.Repository != scope.IntegrationRepository {
			return nil, hubscope.RunnerUnauthorized()
		}
		index := slices.IndexFunc(batch.Items, func(selected tracker.GitHubBatchItem) bool {
			return string(selected.WorkItemID) == item && selected.Status == "pending"
		})
		if index < 0 {
			return nil, hubscope.RunnerUnauthorized()
		}
		previewIndex := slices.IndexFunc(batch.Page.Issues, func(preview tracker.GitHubIssuePreview) bool { return preview.Number == batch.Items[index].Number })
		if previewIndex < 0 {
			return nil, eventstore.ErrInvalidRecord
		}
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		destination, _, err := batchIssueDestination(project.States, batch, batch.Page.Issues[previewIndex])
		if err != nil {
			return nil, err
		}
		if destination != request.State {
			return nil, hubscope.RunnerUnauthorized()
		}
	}
	human := (scope.Credential.Scope == hubapi.APIScopeOperator || scope.Credential.Scope == hubapi.APIScopeAdmin) && scope.Credential.Runner.RunnerID == "" && scope.Actor().Kind == "human" || batchRouting
	auditRecovery := claim != nil && claim.audit != nil
	recordedRecovery := !human && request.Reason == "dependency_ready"
	if !human && !auditRecovery && (conversationID != "" || scope.Credential.Scope != hubapi.APIScopeWorker || scope.Credential.Runner.RunnerID == "" || scope.Credential.Runner.OrganizationID != scope.Organization || scope.Credential.EntryKey != nil) {
		return nil, hubscope.RunnerUnauthorized()
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || request.ExpectedRevision <= 0 || !human && (request.LeaseID == "" || request.FencingToken <= 0) {
		return nil, hubapi.NativePublicInvalid("A current revision and idempotency key are required; workers also require a lease and fencing token")
	}
	if human && request.Reason != "user_requested" || !human && !recordedRecovery && request.Reason != "worker_progress" && request.Reason != string(gate.ReasonValidatorRework) {
		return nil, hubapi.NativePublicInvalid("Transition reason is not allowed for this caller")
	}
	if len(request.ReasonDetail) > workpad.MaxFinalSummaryBytes || !utf8.ValidString(request.ReasonDetail) {
		return nil, hubapi.NativePublicInvalid("Transition reason detail must be bounded UTF-8 text")
	}
	if !human && strings.EqualFold(request.State, "Cancelled") {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	untypedWorkerHold := !human && !recordedRecovery && strings.EqualFold(request.State, "Blocked") && !typedWorkerHumanAction(request.ReasonDetail)
	if human && scope.Credential.HostedRole == "reporter" && request.State != "Cancelled" {
		return nil, operatortool.ErrAccessDenied
	}
	scope.Action = operatortool.MoveItem
	if stop != nil {
		scope.Action = operatortool.StopRun
	}
	scope.ActionItem, scope.ActionTarget = item, request.State
	options := hubapi.NativeCommandOptions{OperationID: nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/workflow"), Item: item, RequireLease: true, Feature: "collaboration"}
	if stop != nil {
		options.OperationID = nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/stop-run")
	}
	payload := struct {
		tracker.Transition
		ConversationID string                       `json:"conversation_id,omitempty"`
		Stop           *coordinatorStopRunArguments `json:"stop,omitempty"`
	}{Transition: request, ConversationID: conversationID, Stop: stop}
	hash, err := hubapi.CommandHash(payload)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var receipt eventstore.Receipt
		var found bool
		if human {
			receipt, found, err = s.humanWorkflowReceipt(ctx, scope, item, options.OperationID, request.IdempotencyKey, hash)
		} else {
			receipt, found, err = s.workItemRecordReceipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash)
		}
		if err != nil {
			return nil, err
		}
		if found {
			if claim != nil {
				return nil, ErrNoClaimableWork
			}
			if receipt.ResponseExpired {
				return nil, commandResponseExpired()
			}
			return s.readWorkItemRecordReceipt(ctx, scope, receipt, request, item)
		}
		row, currentVersion, err := s.readWorkItemRecord(ctx, scope, item)
		if err != nil {
			return nil, err
		}
		issue, err := s.hydrateWorkItemRecord(ctx, row, currentVersion)
		if err != nil {
			return nil, err
		}
		var answerAttempt coordinatorMoveAnswerAttempt
		if conversationID != "" || human && !batchRouting && strings.TrimSpace(request.ReasonDetail) != "" {
			answerAttempt, err = s.checkCoordinatorMoveAnswer(ctx, scope, issue, request.State, request.ReasonDetail)
			if err != nil {
				return nil, err
			}
		}
		compatible := issue.Profile == "github_compatible" && human && !batchRouting
		if issue.Profile == "native" && request.WorkflowLabel != "" {
			return nil, hubapi.NativePublicInvalid("Native workflow transitions do not accept a workflow label")
		}
		if compatible && strings.TrimSpace(request.WorkflowLabel) == "" {
			return nil, hubapi.NativePublicInvalid("Compatibility workflow transitions require an explicit workflow label")
		}
		if issue.Profile != "native" && !compatible || issue.Archived || int64(request.ExpectedRevision) != row.Revision {
			return nil, hubapi.NativeConflict(tracker.Revision(row.Revision))
		}
		reasonDetail := strings.TrimSpace(request.ReasonDetail)
		var recordedHold *recordedWorkerHumanAction
		if untypedWorkerHold {
			recordedHold = issueContractWorkerHumanAction(row, issue)
		}
		if recordedHold != nil {
			reasonDetail = recordedHold.reasonDetail(reasonDetail)
		}
		var linkedSource *eventstore.LinkedSource
		if human && issue.LinkedSource != nil {
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				current, err := r.LinkedSource(ctx, scope.Project, issue.WorkItemID)
				linkedSource = &current
				return err
			}); err != nil {
				return nil, err
			}
			issue.LinkedSource = &tracker.LinkedIssueSource{URL: linkedSource.URL, Status: "pending"}
			if linkedSource.SnapshotObjectKey != "" {
				snapshot, err := content.LinkedIssueSnapshot(ctx, scope.Organization, eventstore.Reference(linkedSource.SnapshotObjectKey, linkedSource.SnapshotSHA256, linkedSource.SnapshotBytes, linkedSource.SnapshotContentBytes))
				if err != nil {
					return nil, err
				}
				if snapshot.URL != linkedSource.URL {
					return nil, artifact.ErrIntegrity
				}
				issue.LinkedSource.Status, issue.LinkedSource.Snapshot = "complete", &snapshot
			}
		}
		project, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		if project.Core.Profile != issue.Profile || project.Core.DeletedAt != nil {
			return nil, corestore.ErrNotFound
		}
		if human && !batchRouting && linkedSource != nil && issue.LinkedSource.Status != "complete" && hubrecords.DispatchableProjectState(project, request.State) {
			return nil, hubapi.NativeWorkflowInvalid("Linked source intake must complete before executable work")
		}
		if err := hubrecords.ValidateNativeWorkflowTransition(project.States, issue.State, request.State, scope.Credential.Scope); err != nil {
			return nil, err
		}
		var recovery *leasestore.RecordedRecovery
		if recordedRecovery {
			if !hubrecords.DispatchableProjectState(project, request.State) {
				return nil, hubapi.NativeConflict(tracker.Revision(row.Revision))
			}
			if claim != nil {
				recovery = claim.recovery
				if row.Revision != recovery.ExpectedRevision || row.EventSequence != recovery.EventSequence {
					return nil, ErrNoClaimableWork
				}
			} else {
				recovery, err = s.prepareRecordedRecoveryTenant(ctx, scope, row, issue, request)
			}
			if err != nil {
				if errors.Is(err, corestore.ErrRevisionConflict) {
					return nil, hubapi.NativeConflict(tracker.Revision(row.Revision))
				}
				return nil, err
			}
		}
		var machineHold *leasestore.Attempt
		if !human && !recordedRecovery && strings.EqualFold(request.State, "Blocked") && issueContractWorkerHumanAction(row, issue) == nil {
			machineHold, err = s.recordedWorkerMachineHold(ctx, scope, row, request)
			if err != nil {
				return nil, err
			}
		}
		destination := request.State
		var retryPolicy policystore.Revision
		if machineHold != nil {
			if hubrecords.DispatchableProjectState(project, issue.State) {
				destination = issue.State
			} else {
				_, retryPolicy, err = s.readTenantProjectPolicyRevision(ctx, scope.Organization, scope.Project)
				if err != nil {
					return nil, err
				}
				if retryPolicy.PolicyID != "" {
					retry, err := recordedRetryLane(ctx, content, retryPolicy)
					if err != nil {
						return nil, err
					}
					if retry = recordedMachineRetryLane(project, issue.State, retry); retry != "" {
						destination = retry
					}
				}
			}
		}
		previousContract, err := json.Marshal(issue.IssueContract)
		if err != nil {
			return nil, err
		}
		previousUpdatedAt, previousActivityAt := issue.UpdatedAt, issue.LastActivityAt
		previousState := issue.State
		now := s.config.Clock().UTC().Truncate(time.Microsecond)
		awaitsFiler := false
		if stop != nil && stop.Priority != nil {
			issue.Priority = new(*stop.Priority - 1)
		}
		issue.State = destination
		issue.Revision = tracker.Revision(row.Revision + 1)
		issue.UpdatedAt = now
		issue.LastActivityAt = now
		issue.Terminal = hubrecords.TenantProjectStateTerminal(project, destination)
		if issue.Terminal {
			if issue.ClosedAt == nil {
				issue.ClosedAt = &now
			}
		} else {
			issue.ClosedAt = nil
		}
		if previousState != request.State && nativeDoneState(request.State) && nativeHumanFiled(issue) {
			var workspace bool
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				workspace, err = r.IsWorkspaceWorkItem(ctx, scope.Project, issue.WorkItemID)
				return err
			}); err != nil {
				return nil, err
			}
			awaitsFiler = !workspace
			if awaitsFiler {
				issue.Verification = &tracker.NativeVerification{State: tracker.VerificationPending}
			}
		} else if nativeDoneState(previousState) && !nativeDoneState(request.State) {
			issue.Verification = nil
		}
		var configuredProject *projectstore.Record
		if human {
			configuredProject = &project
		}
		var stopState, workflowLabel string
		policyRevision, policyFound, err := s.validateTenantTransitionIssueContract(ctx, scope, &issue, configuredProject, previousState, request, &stopState, &workflowLabel)
		if err != nil {
			return nil, err
		}
		if retryPolicy.PolicyID != "" && (!policyFound || !sameTenantProjectPolicyRevisionContent(retryPolicy, policyRevision)) {
			continue
		}
		if stop != nil {
			stopState = request.State
		}
		var cancellation *preparedNativeLeaseCancellation
		if human {
			prepared, present, err := s.prepareHumanWorkflowCancellation(ctx, scope, row.ID, request, stopState)
			if err != nil {
				return nil, err
			}
			if present {
				cancellation = prepared
			}
		}
		currentContract, err := json.Marshal(issue.IssueContract)
		if err != nil {
			return nil, err
		}
		unchanged := machineHold != nil && previousState == destination || human && stop == nil && previousState == destination && bytes.Equal(previousContract, currentContract)
		if unchanged {
			issue.Revision = tracker.Revision(row.Revision)
			issue.UpdatedAt = previousUpdatedAt
			if machineHold != nil || !strings.EqualFold(destination, "Blocked") {
				issue.LastActivityAt = previousActivityAt
			}
		}
		if linkedSource != nil {
			issue.ExternalReferences = slices.DeleteFunc(slices.Clone(issue.ExternalReferences), func(reference tracker.ExternalReference) bool {
				return reference.Provider == "github" && reference.Kind == "issue" && reference.URL == linkedSource.URL
			})
		}
		responseIssue := s.nativeIssueResponse(scope.ReporterIssue(issue))
		versionRaw, err := json.Marshal(responseIssue)
		if err != nil {
			return nil, err
		}
		if linkedSource != nil {
			responseIssue.ExternalReferences = append(slices.Clone(responseIssue.ExternalReferences), tracker.GitHubIssueSourceReference(linkedSource.URL, linkedSource.URL))
		}
		responseRaw, err := json.Marshal(hubrecords.RelationshipOmittedIssue(responseIssue))
		if err != nil {
			return nil, err
		}
		newVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, versionRaw)
		if err != nil {
			return nil, err
		}
		data := tracker.CollaborationData{Revision: issue.Revision, FromState: previousState, ToState: destination, Reason: request.Reason, ReasonDetail: reasonDetail, BlockerAttemptID: request.BlockerAttemptID}
		if answerAttempt.BlockerAttemptID != "" {
			data.BlockerAttemptID = answerAttempt.BlockerAttemptID
		}
		if human && !batchRouting && (stop != nil || strings.EqualFold(request.State, "Blocked")) {
			data.Operation = "operator_stop"
		}
		if previousState != destination {
			data.Change, err = s.landingTransitionSource(ctx, scope, row)
			if err != nil {
				return nil, err
			}
		}
		if awaitsFiler {
			data.Verification = tracker.VerificationPending
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: data})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
		if err != nil {
			return nil, err
		}
		var answerComment *coordinatorAnswerComment
		if conversationID != "" && !unchanged && strings.TrimSpace(request.ReasonDetail) != "" {
			answerComment, err = s.prepareCoordinatorAnswerComment(ctx, scope, row, conversationID, request, now)
			if err != nil {
				return nil, err
			}
		}
		preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: options.OperationID, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, responseRaw)
		if err != nil {
			return nil, err
		}
		plan := hubrecords.WorkItemRecordPlan{Project: project, Current: row, Issue: issue, EventType: "workflow.transitioned"}
		fields := hubrecords.WorkItemRecordFields{Body: hubapi.StringValue(row.BodyObjectKey), Contract: row.IssueContractObjectKey, Report: row.ReportContextObjectKey}
		var contractPayload *eventstore.Receipt
		if human && issue.IssueContract != nil {
			raw, err := json.Marshal(issue.IssueContract)
			if err != nil {
				return nil, err
			}
			prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, raw)
			if err != nil {
				return nil, err
			}
			fields.Contract = &prepared.PayloadObjectKey
			contractPayload = &prepared
		}
		var sourceCancellation *projectstore.GitHubOutbound
		if human && linkedSource != nil && previousState != request.State && request.State == "Cancelled" {
			var base string
			if s.config.Hosted != nil {
				base = s.config.Hosted.PublicURL
			} else if linked, ok := ctx.Value(linkedSourceURLKey{}).(string); ok {
				base = linked
			}
			if base != "" {
				path, err := s.nativeIssueRecordBrowserPath(ctx, scope, item)
				if err != nil {
					return nil, err
				}
				_, repository, number, err := tracker.ParseGitHubIssueURL(linkedSource.URL)
				if err != nil {
					return nil, err
				}
				outbound, err := s.prepareNativeSourceSummary(ctx, scope, issue.WorkItemID, repository, number, "cancelled:"+strconv.FormatInt(int64(issue.Revision), 10), "Cancelled in Detent: "+strings.TrimRight(base, "/")+path, true, "not_planned")
				if err != nil {
					return nil, err
				}
				sourceCancellation = &outbound
			}
		}
		var workflowOutbound *projectstore.GitHubOutbound
		if compatible {
			key, err := hubapi.CommandHash(struct{ Operation, ActorID, CommandKey string }{options.OperationID, scope.Credential.ID, request.IdempotencyKey})
			if err != nil {
				return nil, err
			}
			workflowOutbound, err = s.prepareCompatibilityWorkflowLabel(ctx, scope, issue, project, workflowLabel, "workflow:"+key, now)
			if err != nil {
				return nil, err
			}
		}
		var committed json.RawMessage
		write := func(ctx context.Context, w tenantstore.Writer) error {
			if err := lockRunnerCredential(ctx, w, scope); err != nil {
				return err
			}
			if human {
				if err := s.authorizeNativeWorkflowScope(ctx, w, scope); err != nil {
					return err
				}
			} else if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if recordedRecovery {
				if err := w.LockRelationshipGraph(ctx); err != nil {
					return err
				}
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if batchRouting {
				if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
					return err
				}
				if err := s.requireGitHubPolicyRecord(ctx, w, scope); err != nil {
					return err
				}
			}
			currentProject, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if currentProject.Core.DeletedAt != nil || !hubrecords.SameWorkItemProjectPlan(project, currentProject) {
				return corestore.ErrSerializationFailure
			}
			currentPolicy, policyErr := w.ProjectPolicy(ctx, scope.Project)
			if errors.Is(policyErr, corestore.ErrNotFound) {
				if policyFound {
					return corestore.ErrSerializationFailure
				}
			} else if policyErr != nil {
				return policyErr
			} else if !policyFound || !sameTenantProjectPolicyRevisionContent(policyRevision, currentPolicy) {
				return corestore.ErrSerializationFailure
			}
			locked, err := w.LockWorkItem(ctx, scope.Project, issue.WorkItemID)
			if err != nil {
				return err
			}
			if human && scope.Credential.HostedRole == "reporter" && (request.State != "Cancelled" || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(locked.ActorPrincipalID)})) {
				return corestore.ErrNotFound
			}
			previous, err := w.Receipt(ctx, scope.Credential.ID, options.OperationID, request.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return hubapi.NativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if locked.Archived || locked.Revision != row.Revision || locked.ID != row.ID || locked.NativeID != issue.WorkItemID {
				return corestore.ErrRevisionConflict
			}
			if (recordedRecovery || machineHold != nil) && locked.EventSequence != row.EventSequence {
				return corestore.ErrRevisionConflict
			}
			if answerComment != nil && locked.EventSequence != row.EventSequence {
				return corestore.ErrSerializationFailure
			}
			if err := answerAttempt.recheck(ctx, w, locked.ID); err != nil {
				return err
			}
			if previousState != destination && nativeDoneState(destination) && nativeHumanFiled(issue) {
				workspace, err := w.IsWorkspaceWorkItem(ctx, scope.Project, issue.WorkItemID)
				if err != nil {
					return err
				}
				if !workspace != awaitsFiler {
					return corestore.ErrSerializationFailure
				}
			}
			activeLease := false
			if human {
				currentSource, sourceErr := w.LinkedSource(ctx, scope.Project, issue.WorkItemID)
				if sourceErr == nil && (linkedSource == nil || !sameNativeClaimLinkedSource(currentSource, *linkedSource)) || (errors.Is(sourceErr, corestore.ErrNotFound) || errors.Is(sourceErr, sql.ErrNoRows)) && linkedSource != nil {
					return corestore.ErrSerializationFailure
				}
				if sourceErr != nil && !errors.Is(sourceErr, corestore.ErrNotFound) && !errors.Is(sourceErr, sql.ErrNoRows) {
					return sourceErr
				}
				locker, ok := w.(leasestore.UnreleasedIssueLeaseLocker)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				lease, leaseErr := locker.LockUnreleasedIssueLease(ctx, row.ID)
				if leaseErr != nil && !errors.Is(leaseErr, corestore.ErrNotFound) && !errors.Is(leaseErr, sql.ErrNoRows) {
					return leaseErr
				}
				if request.LeaseID != "" || request.FencingToken != 0 {
					if leaseErr != nil || string(request.LeaseID) != lease.LeaseID || int64(request.FencingToken) != lease.FencingToken {
						return tracker.ErrStaleFencingToken
					}
				}
				if stop != nil && (leaseErr != nil || lease.ReleasedAt != nil || !lease.ExpiresAt.After(s.config.Clock().UTC())) {
					return tracker.ErrLeaseConflict
				}
				activeLease = leaseErr == nil && lease.ReleasedAt == nil && lease.ExpiresAt.After(s.config.Clock().UTC()) && (cancellation == nil || cancellation.expired && !cancellation.expiresLease(lease))
				if leaseErr == nil && (!unchanged || stop != nil) {
					if err := s.settleHumanWorkflowLease(ctx, w, scope, lease, request, cancellation, stopState); err != nil {
						return err
					}
				}
			} else if recordedRecovery {
				var err error
				if auditRecovery {
					err = s.authorizeRecordedRecoveryState(ctx, w, scope, *recovery)
				} else {
					err = s.authorizeRecordedRecoveryTenant(ctx, w, scope, *recovery)
				}
				if err != nil {
					return err
				}
			} else {
				locker, ok := w.(leasestore.FencedLeaseLocker)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				identity, err := w.RunnerForLease(ctx, string(request.LeaseID))
				if err != nil {
					return err
				}
				lease, err := locker.LockLease(ctx, string(request.LeaseID))
				if err != nil {
					return err
				}
				if lease.OrganizationID != scope.Organization || lease.IssueID != row.ID || lease.FencingToken != int64(request.FencingToken) || lease.ReleasedAt != nil {
					return tracker.ErrStaleFencingToken
				}
				credential, err := w.AccessCredential(ctx, scope.Credential.ID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if err != nil || credential.OrganizationID != scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" {
					return hubscope.RunnerUnauthorized()
				}
				currentTime := s.config.Clock().UTC()
				if !lease.ExpiresAt.After(currentTime) || !credential.Valid(scope.Credential.Hash, currentTime) {
					return tracker.ErrStaleFencingToken
				}
				if !runnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), currentTime) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
					return tracker.ErrMachineNotFound
				}
				allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, currentTime)
				if err != nil {
					return err
				}
				if !allowed {
					return tracker.ErrMachineNotFound
				}
				if machineHold != nil {
					latest, err := w.LatestAttempt(ctx, locked.ID)
					if err != nil {
						return err
					}
					if latest.ID != machineHold.ID || latest.Sequence != machineHold.Sequence || latest.PayloadObjectKey != machineHold.PayloadObjectKey {
						return corestore.ErrRevisionConflict
					}
				}
				if recordedHold != nil && !recordedHold.current(locked) {
					return corestore.ErrRevisionConflict
				}
			}
			currentTime := s.config.Clock().UTC()
			if options.Feature != "" && (human || s.database.billingRepository != nil) {
				if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, options.Feature, currentTime)); err != nil {
					return err
				}
			}
			if unchanged {
				if machineHold == nil && strings.EqualFold(destination, "Blocked") {
					if _, err := w.AppendEvent(ctx, event); err != nil {
						return err
					}
				}
				if _, err := w.PutReceipt(ctx, preparedReceipt); err != nil {
					return err
				}
				committed = responseRaw
				return nil
			}
			if contractPayload != nil {
				ref := eventstore.Reference(contractPayload.PayloadObjectKey, contractPayload.PayloadSHA256, contractPayload.PayloadBytes, contractPayload.PayloadContentBytes)
				id := "att_" + artifact.Digest([]byte(string(scope.Organization) + "\n" + ref.Key))[:32]
				object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: id, Reference: ref, CreatedAt: now})
				if err != nil {
					return err
				}
				if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "work-item-contract", ID: string(issue.WorkItemID) + ":" + strconv.FormatInt(int64(issue.Revision), 10), CreatedAt: now}); err != nil {
					return err
				}
			}
			if err := hubrecords.WriteWorkItemRecord(ctx, w, plan, fields); err != nil {
				return err
			}
			if !human && !recordedRecovery {
				if err := w.AdvanceLeaseWorkItemRevision(ctx, leasestore.Fence{LeaseID: string(request.LeaseID), FencingToken: int64(request.FencingToken)}, locked.ID, locked.Revision, int64(issue.Revision)); err != nil {
					return err
				}
			}
			if human && !activeLease && (previousState != request.State || stop != nil) && hubrecords.DispatchableProjectState(currentProject, request.State) {
				if s.config.ConversationDispatch == nil {
					return conversationDispatchUnavailable()
				}
				if err := s.config.ConversationDispatch.RequestDispatch(ctx, w, scope.Project, string(issue.WorkItemID), leasestore.Fence{}, currentTime); err != nil {
					return err
				}
			}
			if _, err := w.CreateCollaborationVersion(ctx, newVersion); err != nil {
				return err
			}
			if err := w.BindDoctorProposalIdentities(ctx, corestore.DoctorProposalIdentityBinding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: newVersion.WorkItemID, Revision: newVersion.Revision, VersionSHA256: newVersion.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(issue.Body)}); err != nil {
				return err
			}
			if answerComment != nil {
				if err := answerComment.write(ctx, w); err != nil {
					return err
				}
			}
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return err
			}
			if workflowOutbound != nil {
				owner, ok := w.(projectstore.GitHubOutboxWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				if err := owner.EnqueueGitHubOutbound(ctx, *workflowOutbound); err != nil {
					return err
				}
			}
			if sourceCancellation != nil {
				owner, ok := w.(projectstore.GitHubOutboxWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				if err := owner.EnqueueGitHubOutbound(ctx, *sourceCancellation); err != nil {
					return err
				}
			}
			if _, err := w.PutReceipt(ctx, preparedReceipt); err != nil {
				return err
			}
			committed = responseRaw
			return nil
		}
		if claim != nil {
			claim.write = write
			return nil, nil
		}
		if recordedRecovery {
			err = s.writeNativeClaimCommand(ctx, scope, write)
		} else {
			err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
		}
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			if recordedRecovery && errors.Is(err, corestore.ErrRevisionConflict) {
				return nil, hubapi.NativeConflict(tracker.Revision(row.Revision))
			}
			return nil, hubapi.ProjectRecordError(err)
		}
		if human {
			s.signalNativeDispatch(scope)
		}
		return committed, nil
	}
}

func typedWorkerHumanAction(detail string) bool {
	content, found := workpad.LastStatusBlock(detail)
	if !found {
		return false
	}
	signal, err := workpad.ParseStatusBlock(content, "")
	return err == nil && signal.Invalid == nil && signal.Status == workpad.StatusBlocked && strings.TrimSpace(signal.HumanAction) != ""
}

const maxRecordedWorkerHumanActionBytes = 1 << 10

type recordedWorkerHumanAction struct {
	action   string
	source   string
	sourceID string
	revision int64
	contract *string
}

func issueContractWorkerHumanAction(row corestore.WorkItem, issue tracker.NativeIssue) *recordedWorkerHumanAction {
	if issue.IssueContract == nil || strings.TrimSpace(issue.IssueContract.HumanAction) == "" {
		return nil
	}
	return &recordedWorkerHumanAction{action: strings.TrimSpace(issue.IssueContract.HumanAction), source: "issue_contract", sourceID: strconv.FormatInt(row.Revision, 10), revision: row.Revision, contract: row.IssueContractObjectKey}
}

func (h *recordedWorkerHumanAction) current(locked corestore.WorkItem) bool {
	return locked.Revision == h.revision && hubapi.StringValue(locked.IssueContractObjectKey) == hubapi.StringValue(h.contract)
}

func (h *recordedWorkerHumanAction) reasonDetail(detail string) string {
	action := h.action
	if len(action) > maxRecordedWorkerHumanActionBytes {
		action = truncateUTF8(action, maxRecordedWorkerHumanActionBytes)
	}
	block := "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: " + strconv.Quote(action) + "\nfields:\n  human_action_source: " + h.source + "\n  human_action_source_id: " + strconv.Quote(h.sourceID) + "\n```"
	detail = strings.TrimSpace(workpad.FinalSummary(detail))
	if budget := workpad.MaxFinalSummaryBytes - len(block) - 2; len(detail) > budget {
		detail = strings.TrimSpace(truncateUTF8(detail, max(budget, 0)))
	}
	if detail == "" {
		return block
	}
	return detail + "\n\n" + block
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Service) humanWorkflowReceipt(ctx context.Context, scope hubscope.NativeScope, item, operation, key, hash string) (eventstore.Receipt, bool, error) {
	var receipt eventstore.Receipt
	var found bool
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeNativeWorkflowScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		row, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if scope.Credential.HostedRole == "reporter" && !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		receipt, err = w.Receipt(ctx, scope.Credential.ID, operation, key)
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if receipt.RequestHash != hash {
			return hubapi.NativeConflict(0)
		}
		now := s.config.Clock().UTC()
		if !receipt.ResponseExpired && receipt.ResponseReplayUntilNs > 0 && receipt.ResponseReplayUntilNs <= now.UnixNano() {
			writer, ok := w.(receiptstore.RetentionWriter)
			if !ok {
				return eventstore.ErrInvalidRecord
			}
			if _, err := writer.ExpireReceipt(ctx, receipt, now.Add(time.Nanosecond)); err != nil {
				return err
			}
			receipt, err = w.Receipt(ctx, scope.Credential.ID, operation, key)
			return err
		}
		return nil
	})
	if err != nil {
		return receipt, false, hubapi.ProjectRecordError(err)
	}
	if !found {
		return receipt, false, nil
	}
	if receipt.RequestHash != hash {
		return receipt, false, hubapi.NativeConflict(0)
	}
	return receipt, true, nil
}

func (s *Service) validateTenantTransitionIssueContract(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue, project *projectstore.Record, previousState string, request tracker.Transition, stopState, workflowLabel *string) (policystore.Revision, bool, error) {
	workflow := config.Workflow{Config: config.Default()}
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		revision, err = r.ProjectPolicy(ctx, scope.Project)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return policystore.Revision{}, false, err
	}
	descriptor := policy.Descriptor{}
	found := err == nil
	exempt := issue.LinkedSource != nil && issue.LinkedSource.Status != "complete" || strings.EqualFold(issue.State, "Triage") || issue.IssueContract != nil && issue.IssueContract.Exempt || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch")
	if found {
		descriptor, err = eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
		if err != nil {
			return policystore.Revision{}, false, err
		}
	}
	if project != nil {
		if err := validateNativeWorkflowPolicy(*project, descriptor, found); err != nil {
			return revision, found, err
		}
	}
	if found {
		workflow, err = config.ApplyNativePolicy(workflow, descriptor)
		if err != nil {
			return policystore.Revision{}, false, hubapi.PolicyMismatch(err.Error())
		}
	}
	if workflowLabel != nil {
		*workflowLabel = ""
	}
	if issue.Profile == "github_compatible" {
		label, labelErr := approvedCompatibilityWorkflowLabel(workflow, found, request.State)
		if request.WorkflowLabel != "" {
			if labelErr != nil {
				return revision, found, labelErr
			}
			if label != strings.TrimSpace(request.WorkflowLabel) {
				return revision, found, hubapi.NativePublicInvalid("Workflow label must match the approved target state mapping")
			}
		}
		if labelErr == nil && workflowLabel != nil {
			*workflowLabel = label
		}
	}
	*stopState = workflow.Config.Agent.StopRun.TargetState
	if project != nil {
		if err := applyHumanWorkflowContract(issue, previousState, request, descriptor, workflow, s.config.Clock().UTC()); err != nil {
			return revision, found, err
		}
	}
	if exempt {
		return revision, found, nil
	}
	target := workflow.Config.BacklogAdmission.TargetState
	if target == "" {
		target = "Todo"
	}
	if !strings.EqualFold(issue.State, target) {
		return revision, found, nil
	}
	contract, err := config.ResolvePolicyIssueContract(descriptor)
	if err != nil {
		return revision, found, err
	}
	if evaluation := contract.Evaluate(hubrecords.NativeContractIssue(*issue)); len(evaluation.Missing) > 0 {
		return revision, found, hubapi.NativeWorkflowInvalid(strings.Join(evaluation.Missing, ", ") + ". File into Backlog/Triage to draft it first.")
	}
	return revision, found, nil
}

func approvedCompatibilityWorkflowLabel(workflow config.Workflow, found bool, state string) (string, error) {
	label, mapped := workflow.Config.Tracker.StateMap.Map[state].(string)
	label = strings.TrimSpace(label)
	if !found || !workflow.Config.Tracker.StateMap.IsMap || !mapped || label == "" {
		return "", hubapi.NativePublicInvalid("Compatibility workflow transitions require an approved state-to-label mapping")
	}
	if len(label) > 200 || !utf8.ValidString(label) || strings.ContainsAny(label, "\r\n\x00") || !strings.HasPrefix(strings.ToLower(label), defaultLabelPrefix) {
		return "", hubapi.NativePublicInvalid("Workflow label must match the approved target state mapping")
	}
	return label, nil
}

func (s *Service) prepareCompatibilityWorkflowLabel(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, project projectstore.Record, label, key string, now time.Time) (*projectstore.GitHubOutbound, error) {
	repository, number, err := cutoverSourceReference(issue)
	if err != nil || !project.Settings.RepositoryEnabled || !strings.EqualFold(repository, strings.TrimSpace(project.Settings.Repository)) {
		return nil, hubapi.NativePublicInvalid("Compatibility workflow transitions require an enabled matching repository source")
	}
	desired, err := json.Marshal(WorkflowLabelDesired{Label: label, ManagedPrefix: defaultLabelPrefix})
	if err != nil {
		return nil, err
	}
	prepared, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, desired)
	if err != nil {
		return nil, err
	}
	return &projectstore.GitHubOutbound{ProjectID: scope.Project, WorkItemID: issue.WorkItemID, Key: key, Repository: repository, IssueNumber: int64(number), Kind: string(MutationWorkflowLabel), CreatedAt: now, Payload: prepared, CoalesceKey: "managed-label:" + repository + ":" + string(issue.WorkItemID) + ":" + defaultLabelPrefix}, nil
}
