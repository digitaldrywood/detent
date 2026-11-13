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
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) transitionNativeIssueTenant(ctx context.Context, scope nativeScope, item string, request tracker.Transition) (json.RawMessage, error) {
	result, err := s.transitionNativeIssueTenantWithStop(ctx, scope, item, request, nil)
	if err == nil && s.wakeSpriteRunnersAfter != nil {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}

func (s *Service) transitionNativeIssueTenantWithStop(ctx context.Context, scope nativeScope, item string, request tracker.Transition, stop *coordinatorStopRunArguments) (json.RawMessage, error) {
	batchRouting := scope.integrationOwned && scope.integrationRepository != "" && scope.githubIntakeRevision != nil && scope.githubIntakeRevision.batchID != "" && scope.githubIntakeRevision.revision > 0 && scope.credential.Scope == hubapi.APIScopeAdmin && scope.credential.Runner.RunnerID == "" && request.Reason == "user_requested" && request.State != "Cancelled" && stop == nil
	if batchRouting {
		batch, err := s.loadGitHubBatchRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		actor := scope.actor()
		if batch.ID != scope.githubIntakeRevision.batchID || batch.Revision != scope.githubIntakeRevision.revision || batch.Status != "importing" || batch.RequestedBy == nil || batch.RequestedBy.Kind != actor.Kind || batch.RequestedBy.PrincipalID != actor.PrincipalID || batch.Discovery.Repository != scope.integrationRepository {
			return nil, runnerUnauthorized()
		}
		index := slices.IndexFunc(batch.Items, func(selected tracker.GitHubBatchItem) bool {
			return string(selected.WorkItemID) == item && selected.Status == "pending"
		})
		if index < 0 {
			return nil, runnerUnauthorized()
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
			return nil, runnerUnauthorized()
		}
	}
	human := (scope.credential.Scope == hubapi.APIScopeOperator || scope.credential.Scope == hubapi.APIScopeAdmin) && scope.credential.Runner.RunnerID == "" && scope.actor().Kind == "human" || batchRouting
	recordedRecovery := !human && request.Reason == "dependency_ready"
	if !human && (scope.credential.Scope != hubapi.APIScopeWorker || scope.credential.Runner.RunnerID == "" || scope.credential.Runner.OrganizationID != scope.organization || scope.credential.EntryKey != nil) {
		return nil, runnerUnauthorized()
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
	if human && scope.credential.HostedRole == "reporter" && request.State != "Cancelled" {
		return nil, operatortool.ErrAccessDenied
	}
	scope.action = operatortool.MoveItem
	if stop != nil {
		scope.action = operatortool.StopRun
	}
	scope.actionItem, scope.actionTarget = item, request.State
	options := hubapi.NativeCommandOptions{OperationID: nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/workflow"), Item: item, RequireLease: true, Feature: "collaboration"}
	if stop != nil {
		options.OperationID = nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/stop-run")
	}
	payload := struct {
		tracker.Transition
		ConversationID string                       `json:"conversation_id,omitempty"`
		Stop           *coordinatorStopRunArguments `json:"stop,omitempty"`
	}{Transition: request, Stop: stop}
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
		var linkedSource *eventstore.LinkedSource
		if human && issue.LinkedSource != nil {
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				current, err := r.LinkedSource(ctx, scope.project, issue.WorkItemID)
				linkedSource = &current
				return err
			}); err != nil {
				return nil, err
			}
			issue.LinkedSource = &tracker.LinkedIssueSource{URL: linkedSource.URL, Status: "pending"}
			if linkedSource.SnapshotObjectKey != "" {
				snapshot, err := content.LinkedIssueSnapshot(ctx, scope.organization, eventstore.Reference(linkedSource.SnapshotObjectKey, linkedSource.SnapshotSHA256, linkedSource.SnapshotBytes, linkedSource.SnapshotContentBytes))
				if err != nil {
					return nil, err
				}
				if snapshot.URL != linkedSource.URL {
					return nil, artifact.ErrIntegrity
				}
				issue.LinkedSource.Status, issue.LinkedSource.Snapshot = "complete", &snapshot
			}
		}
		project, err := s.readTenantProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		if project.Core.Profile != issue.Profile || project.Core.DeletedAt != nil {
			return nil, corestore.ErrNotFound
		}
		if human && !batchRouting && linkedSource != nil && issue.LinkedSource.Status != "complete" && dispatchableProjectState(project, request.State) {
			return nil, hubapi.NativeWorkflowInvalid("Linked source intake must complete before executable work")
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, request.State, scope.credential.Scope); err != nil {
			return nil, err
		}
		var recovery *leasestore.RecordedRecovery
		if recordedRecovery {
			if !dispatchableProjectState(project, request.State) {
				return nil, hubapi.NativeConflict(tracker.Revision(row.Revision))
			}
			recovery, err = s.prepareRecordedRecoveryTenant(ctx, scope, row, issue, request)
			if err != nil {
				if errors.Is(err, corestore.ErrRevisionConflict) {
					return nil, hubapi.NativeConflict(tracker.Revision(row.Revision))
				}
				return nil, err
			}
		}
		previousContract, err := json.Marshal(issue.IssueContract)
		if err != nil {
			return nil, err
		}
		previousUpdatedAt, previousActivityAt := issue.UpdatedAt, issue.LastActivityAt
		previousState := issue.State
		now := s.config.now().UTC().Truncate(time.Microsecond)
		awaitsFiler := false
		if stop != nil && stop.Priority != nil {
			issue.Priority = new(*stop.Priority - 1)
		}
		issue.State = request.State
		issue.Revision = tracker.Revision(row.Revision + 1)
		issue.UpdatedAt = now
		issue.LastActivityAt = now
		issue.Terminal = tenantProjectStateTerminal(project, request.State)
		if issue.Terminal {
			if issue.ClosedAt == nil {
				issue.ClosedAt = &now
			}
		} else {
			issue.ClosedAt = nil
		}
		if previousState != request.State && nativeDoneState(request.State) && nativeHumanFiled(issue) {
			var workspace bool
			if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				workspace, err = r.IsWorkspaceWorkItem(ctx, scope.project, issue.WorkItemID)
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
		unchanged := human && stop == nil && previousState == request.State && bytes.Equal(previousContract, currentContract)
		if unchanged {
			issue.Revision = tracker.Revision(row.Revision)
			issue.UpdatedAt = previousUpdatedAt
			if !strings.EqualFold(request.State, "Blocked") {
				issue.LastActivityAt = previousActivityAt
			}
		}
		if linkedSource != nil {
			issue.ExternalReferences = slices.DeleteFunc(slices.Clone(issue.ExternalReferences), func(reference tracker.ExternalReference) bool {
				return reference.Provider == "github" && reference.Kind == "issue" && reference.URL == linkedSource.URL
			})
		}
		responseIssue := s.nativeIssueResponse(scope.reporterIssue(issue))
		versionRaw, err := json.Marshal(responseIssue)
		if err != nil {
			return nil, err
		}
		if linkedSource != nil {
			responseIssue.ExternalReferences = append(slices.Clone(responseIssue.ExternalReferences), tracker.GitHubIssueSourceReference(linkedSource.URL, linkedSource.URL))
		}
		responseRaw, err := json.Marshal(relationshipOmittedIssue(responseIssue))
		if err != nil {
			return nil, err
		}
		newVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, versionRaw)
		if err != nil {
			return nil, err
		}
		data := tracker.CollaborationData{Revision: issue.Revision, FromState: previousState, ToState: request.State, Reason: request.Reason, ReasonDetail: strings.TrimSpace(request.ReasonDetail), BlockerAttemptID: request.BlockerAttemptID}
		if previousState != request.State {
			data.Change, err = s.landingTransitionSource(ctx, scope, row)
			if err != nil {
				return nil, err
			}
		}
		if awaitsFiler {
			data.Verification = tracker.VerificationPending
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: data})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return nil, err
		}
		preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: options.OperationID, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, responseRaw)
		if err != nil {
			return nil, err
		}
		searchDocument := searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}
		searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchDocument, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
		if err != nil {
			return nil, err
		}
		plan := workItemRecordPlan{project: project, current: row, issue: issue, eventType: "workflow.transitioned"}
		fields := workItemRecordFields{Body: hubapi.StringValue(row.BodyObjectKey), Contract: row.IssueContractObjectKey, Report: row.ReportContextObjectKey}
		var contractPayload *eventstore.Receipt
		if human && issue.IssueContract != nil {
			raw, err := json.Marshal(issue.IssueContract)
			if err != nil {
				return nil, err
			}
			prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, raw)
			if err != nil {
				return nil, err
			}
			fields.Contract = &prepared.PayloadObjectKey
			contractPayload = &prepared
		}
		var sourceCancellation *projectstore.GitHubOutbound
		if human && linkedSource != nil && previousState != request.State && request.State == "Cancelled" {
			base, _ := ctx.Value(linkedSourceURLKey{}).(string)
			if s.config.Hosted != nil {
				base = s.config.Hosted.PublicURL
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
			key, err := hubapi.CommandHash(struct{ Operation, ActorID, CommandKey string }{options.OperationID, scope.credential.ID, request.IdempotencyKey})
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
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
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
			currentProject, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if currentProject.Core.DeletedAt != nil || !sameWorkItemProjectPlan(project, currentProject) {
				return corestore.ErrSerializationFailure
			}
			currentPolicy, policyErr := w.ProjectPolicy(ctx, scope.project)
			if errors.Is(policyErr, corestore.ErrNotFound) {
				if policyFound {
					return corestore.ErrSerializationFailure
				}
			} else if policyErr != nil {
				return policyErr
			} else if !policyFound || !sameTenantProjectPolicyRevisionContent(policyRevision, currentPolicy) {
				return corestore.ErrSerializationFailure
			}
			locked, err := w.LockWorkItem(ctx, scope.project, issue.WorkItemID)
			if err != nil {
				return err
			}
			if human && scope.credential.HostedRole == "reporter" && (request.State != "Cancelled" || !scope.reporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(locked.ActorPrincipalID)})) {
				return corestore.ErrNotFound
			}
			previous, err := w.Receipt(ctx, scope.credential.ID, options.OperationID, request.IdempotencyKey)
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
			if recordedRecovery && locked.EventSequence != row.EventSequence {
				return corestore.ErrRevisionConflict
			}
			if previousState != request.State && nativeDoneState(request.State) && nativeHumanFiled(issue) {
				workspace, err := w.IsWorkspaceWorkItem(ctx, scope.project, issue.WorkItemID)
				if err != nil {
					return err
				}
				if !workspace != awaitsFiler {
					return corestore.ErrSerializationFailure
				}
			}
			activeLease := false
			if human {
				currentSource, sourceErr := w.LinkedSource(ctx, scope.project, issue.WorkItemID)
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
				if stop != nil && (leaseErr != nil || lease.ReleasedAt != nil || !lease.ExpiresAt.After(s.config.now().UTC())) {
					return tracker.ErrLeaseConflict
				}
				activeLease = leaseErr == nil && lease.ReleasedAt == nil && lease.ExpiresAt.After(s.config.now().UTC()) && (cancellation == nil || cancellation.expired && !cancellation.expiresLease(lease))
				if leaseErr == nil && (!unchanged || stop != nil) {
					if err := s.settleHumanWorkflowLease(ctx, w, scope, lease, request, cancellation, stopState); err != nil {
						return err
					}
				}
			} else if recordedRecovery {
				if err := s.authorizeRecordedRecoveryTenant(ctx, w, scope, *recovery); err != nil {
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
				if lease.OrganizationID != scope.organization || lease.IssueID != row.ID || lease.FencingToken != int64(request.FencingToken) || lease.ReleasedAt != nil {
					return tracker.ErrStaleFencingToken
				}
				credential, err := w.AccessCredential(ctx, scope.credential.ID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if err != nil || credential.OrganizationID != scope.organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" {
					return runnerUnauthorized()
				}
				currentTime := s.config.now().UTC()
				if !lease.ExpiresAt.After(currentTime) || !credential.Valid(scope.credential.Hash, currentTime) {
					return tracker.ErrStaleFencingToken
				}
				if !runnerIdentityMatchesHeldLease(identity, scope.organization, scope.credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), currentTime) || identity.State == "disabled" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
					return tracker.ErrMachineNotFound
				}
				allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, currentTime)
				if err != nil {
					return err
				}
				if !allowed {
					return tracker.ErrMachineNotFound
				}
			}
			currentTime := s.config.now().UTC()
			if options.Feature != "" && (human || s.database.billingRepository != nil) {
				if err := s.database.requireHostedFeatureRecord(ctx, w, options.Feature, currentTime); err != nil {
					return err
				}
			}
			if unchanged {
				if strings.EqualFold(request.State, "Blocked") {
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
				id := "att_" + artifact.Digest([]byte(string(scope.organization) + "\n" + ref.Key))[:32]
				object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.organization, ID: id, Reference: ref, CreatedAt: now})
				if err != nil {
					return err
				}
				if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.organization, ArchiveID: object.ID, Kind: "work-item-contract", ID: string(issue.WorkItemID) + ":" + strconv.FormatInt(int64(issue.Revision), 10), CreatedAt: now}); err != nil {
					return err
				}
			}
			if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
				return err
			}
			if !human && !recordedRecovery {
				if err := w.AdvanceLeaseWorkItemRevision(ctx, leasestore.Fence{LeaseID: string(request.LeaseID), FencingToken: int64(request.FencingToken)}, locked.ID, locked.Revision, int64(issue.Revision)); err != nil {
					return err
				}
			}
			if human && !activeLease && (previousState != request.State || stop != nil) && dispatchableProjectState(currentProject, request.State) {
				if s.config.ConversationDispatch == nil {
					return conversationDispatchUnavailable()
				}
				if err := s.config.ConversationDispatch.RequestDispatch(ctx, w, scope.project, string(issue.WorkItemID), leasestore.Fence{}, currentTime); err != nil {
					return err
				}
			}
			if err := w.PutSearch(ctx, searchDocument, row.Revision); err != nil {
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if err := w.PutSearch(ctx, searchDocument, 0); err != nil {
					return err
				}
			}
			if _, err := w.CreateCollaborationVersion(ctx, newVersion); err != nil {
				return err
			}
			if err := w.BindDoctorProposalIdentities(ctx, corestore.DoctorProposalIdentityBinding{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: newVersion.WorkItemID, Revision: newVersion.Revision, VersionSHA256: newVersion.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(issue.Body)}); err != nil {
				return err
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
		if recordedRecovery {
			err = s.writeNativeClaimCommand(ctx, scope, write)
		} else {
			err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
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

func (s *Service) humanWorkflowReceipt(ctx context.Context, scope nativeScope, item, operation, key, hash string) (eventstore.Receipt, bool, error) {
	var receipt eventstore.Receipt
	var found bool
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeNativeWorkflowScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		row, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if scope.credential.HostedRole == "reporter" && !scope.reporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		receipt, err = w.Receipt(ctx, scope.credential.ID, operation, key)
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
		now := s.config.now().UTC()
		if !receipt.ResponseExpired && receipt.ResponseReplayUntilNs > 0 && receipt.ResponseReplayUntilNs <= now.UnixNano() {
			writer, ok := w.(receiptstore.RetentionWriter)
			if !ok {
				return eventstore.ErrInvalidRecord
			}
			if _, err := writer.ExpireReceipt(ctx, receipt, now.Add(time.Nanosecond)); err != nil {
				return err
			}
			receipt, err = w.Receipt(ctx, scope.credential.ID, operation, key)
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

func tenantProjectStateTerminal(project projectstore.Record, name string) bool {
	for _, state := range project.States {
		if state.Name == name {
			return state.Terminal
		}
	}
	return false
}

func (s *Service) readTenantProjectRecord(ctx context.Context, scope nativeScope) (projectstore.Record, error) {
	var project projectstore.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(ctx, scope.project)
		return err
	})
	return project, hubapi.ProjectRecordError(err)
}

func (s *Service) validateTenantTransitionIssueContract(ctx context.Context, scope nativeScope, issue *tracker.NativeIssue, project *projectstore.Record, previousState string, request tracker.Transition, stopState, workflowLabel *string) (policystore.Revision, bool, error) {
	workflow := config.Workflow{Config: config.Default()}
	var revision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		revision, err = r.ProjectPolicy(ctx, scope.project)
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
		if err := applyHumanWorkflowContract(issue, previousState, request, descriptor, workflow, s.config.now().UTC()); err != nil {
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
	if evaluation := contract.Evaluate(nativeContractIssue(*issue)); len(evaluation.Missing) > 0 {
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

func (s *Service) prepareCompatibilityWorkflowLabel(ctx context.Context, scope nativeScope, issue tracker.NativeIssue, project projectstore.Record, label, key string, now time.Time) (*projectstore.GitHubOutbound, error) {
	repository, number, err := cutoverSourceReference(issue)
	if err != nil || !project.Settings.RepositoryEnabled || !strings.EqualFold(repository, strings.TrimSpace(project.Settings.Repository)) {
		return nil, hubapi.NativePublicInvalid("Compatibility workflow transitions require an enabled matching repository source")
	}
	desired, err := json.Marshal(WorkflowLabelDesired{Label: label, ManagedPrefix: defaultLabelPrefix})
	if err != nil {
		return nil, err
	}
	prepared, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, desired)
	if err != nil {
		return nil, err
	}
	return &projectstore.GitHubOutbound{ProjectID: scope.project, WorkItemID: issue.WorkItemID, Key: key, Repository: repository, IssueNumber: int64(number), Kind: string(MutationWorkflowLabel), CreatedAt: now, Payload: prepared, CoalesceKey: "managed-label:" + repository + ":" + string(issue.WorkItemID) + ":" + defaultLabelPrefix}, nil
}
