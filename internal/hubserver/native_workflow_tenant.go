package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) transitionNativeIssueTenant(ctx context.Context, scope nativeScope, item string, request tracker.Transition) (json.RawMessage, error) {
	if scope.credential.Scope != apiScopeWorker || scope.credential.Runner.RunnerID == "" || scope.credential.Runner.OrganizationID != scope.organization || scope.credential.EntryKey != nil {
		return nil, runnerUnauthorized()
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || request.ExpectedRevision <= 0 || request.LeaseID == "" || request.FencingToken <= 0 {
		return nil, nativePublicInvalid("A current revision, lease, fencing token, and idempotency key are required")
	}
	if request.Reason != "worker_progress" && request.Reason != string(gate.ReasonValidatorRework) {
		return nil, workItemStorageUnavailable()
	}
	if len(request.ReasonDetail) > workpad.MaxFinalSummaryBytes || !utf8.ValidString(request.ReasonDetail) {
		return nil, nativePublicInvalid("Transition reason detail must be bounded UTF-8 text")
	}
	if strings.EqualFold(request.State, "Cancelled") {
		return nil, workItemStorageUnavailable()
	}
	scope.action = operatortool.MoveItem
	scope.actionItem, scope.actionTarget = item, request.State
	options := nativeCommandOptions{OperationID: nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/workflow"), Item: item, RequireLease: true, Feature: "collaboration"}
	payload := struct {
		tracker.Transition
		ConversationID string `json:"conversation_id,omitempty"`
	}{Transition: request}
	hash, err := nativeCommandHash(payload)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		receipt, found, err := s.workItemRecordReceipt(ctx, scope, options.OperationID, request.IdempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if found {
			return content.Receipt(ctx, receipt)
		}
		row, currentVersion, err := s.readWorkItemRecord(ctx, scope, item)
		if err != nil {
			return nil, err
		}
		issue, err := s.hydrateWorkItemRecord(ctx, row, currentVersion)
		if err != nil {
			return nil, err
		}
		if issue.Profile != "native" || issue.Archived || int64(request.ExpectedRevision) != row.Revision {
			return nil, nativeConflict(tracker.Revision(row.Revision))
		}
		project, err := s.readTenantProjectRecord(ctx, scope)
		if err != nil {
			return nil, err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return nil, corestore.ErrNotFound
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, request.State, scope.credential.Scope); err != nil {
			return nil, err
		}
		previousState := issue.State
		now := s.config.now().UTC().Truncate(time.Microsecond)
		awaitsFiler := false
		issue.State = request.State
		issue.Revision = tracker.Revision(row.Revision + 1)
		issue.UpdatedAt = now
		issue.LastActivityAt = now
		issue.Terminal = tenantProjectStateTerminal(project, request.State)
		if nativeDoneState(request.State) && nativeHumanFiled(issue) {
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
		policyRevision, policyFound, err := s.validateTenantTransitionIssueContract(ctx, scope, issue)
		if err != nil {
			return nil, err
		}
		responseIssue := s.nativeIssueResponse(scope.reporterIssue(issue))
		responseRaw, err := json.Marshal(responseIssue)
		if err != nil {
			return nil, err
		}
		newVersion, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, responseRaw)
		if err != nil {
			return nil, err
		}
		data := tracker.CollaborationData{Revision: issue.Revision, FromState: previousState, ToState: request.State, Reason: request.Reason, ReasonDetail: strings.TrimSpace(request.ReasonDetail)}
		if awaitsFiler {
			data.Verification = tracker.VerificationPending
		}
		eventRaw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: data})
		if err != nil {
			return nil, err
		}
		event, err := content.PrepareEvent(ctx, scope.organization, eventRaw)
		if err != nil {
			return nil, err
		}
		preparedReceipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: options.OperationID, CommandKey: request.IdempotencyKey, RequestHash: hash}, responseRaw)
		if err != nil {
			return nil, err
		}
		searchDocument := searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}
		searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchDocument, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
		if err != nil {
			return nil, err
		}
		plan := workItemRecordPlan{project: project, current: row, issue: issue, eventType: "workflow.transitioned"}
		fields := workItemRecordFields{Body: stringValue(row.BodyObjectKey), Contract: row.IssueContractObjectKey, Report: row.ReportContextObjectKey}
		var committed json.RawMessage
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
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
			previous, err := w.Receipt(ctx, scope.credential.ID, options.OperationID, request.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			locked, err := w.LockWorkItem(ctx, scope.project, issue.WorkItemID)
			if err != nil {
				return err
			}
			if locked.Archived || locked.Revision != row.Revision || locked.ID != row.ID || locked.NativeID != issue.WorkItemID {
				return corestore.ErrRevisionConflict
			}
			if nativeDoneState(request.State) && nativeHumanFiled(issue) {
				workspace, err := w.IsWorkspaceWorkItem(ctx, scope.project, issue.WorkItemID)
				if err != nil {
					return err
				}
				if !workspace != awaitsFiler {
					return corestore.ErrSerializationFailure
				}
			}
			locker, ok := w.(leasestore.FencedLeaseLocker)
			if !ok {
				return workItemStorageUnavailable()
			}
			lease, err := locker.LockLease(ctx, string(request.LeaseID))
			if err != nil {
				return err
			}
			if lease.OrganizationID != scope.organization || lease.IssueID != row.ID || lease.FencingToken != int64(request.FencingToken) || lease.ReleasedAt != nil {
				return tracker.ErrStaleFencingToken
			}
			credential, err := w.AccessCredential(ctx, scope.credential.ID)
			if err != nil || credential.OrganizationID != scope.organization || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" {
				return runnerUnauthorized()
			}
			identity, err := w.RunnerForLease(ctx, lease.LeaseID)
			if err != nil {
				return err
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
			if options.Feature != "" && s.database.billingRepository != nil {
				if err := s.database.requireHostedFeatureRecord(ctx, w, options.Feature, currentTime); err != nil {
					return err
				}
			}
			if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
				return err
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
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return err
			}
			if _, err := w.PutReceipt(ctx, preparedReceipt); err != nil {
				return err
			}
			committed = responseRaw
			return nil
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		return committed, nil
	}
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
	return project, projectRecordError(err)
}

func (s *Service) validateTenantTransitionIssueContract(ctx context.Context, scope nativeScope, issue tracker.NativeIssue) (policystore.Revision, bool, error) {
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
	if issue.LinkedSource != nil && issue.LinkedSource.Status != "complete" || strings.EqualFold(issue.State, "Triage") || issue.IssueContract != nil && issue.IssueContract.Exempt || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return revision, found, nil
	}
	if err == nil {
		descriptor, err = eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, revision)
		if err != nil {
			return policystore.Revision{}, false, err
		}
		workflow, err = config.ApplyNativePolicy(workflow, descriptor)
		if err != nil {
			return policystore.Revision{}, false, policyMismatch(err.Error())
		}
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
	if evaluation := contract.Evaluate(nativeContractIssue(issue)); len(evaluation.Missing) > 0 {
		return revision, found, nativeWorkflowInvalid(strings.Join(evaluation.Missing, ", ") + ". File into Backlog/Triage to draft it first.")
	}
	return revision, found, nil
}
