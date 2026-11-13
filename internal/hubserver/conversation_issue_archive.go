package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const coordinatorArchiveMaxItems = 50

func validateCoordinatorArchiveIDs(ids []string) error {
	if len(ids) < 1 || len(ids) > coordinatorArchiveMaxItems {
		return hubapi.NativePublicInvalid("An archive must contain 1 to 50 issues")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !strings.HasPrefix(id, "wi_") || len(id) > 256 || seen[id] {
			return hubapi.NativePublicInvalid("Unique native work item IDs are required")
		}
		seen[id] = true
	}
	return nil
}

func (t *coordinatorToolset) proposeIssueArchive(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	var args struct {
		WorkItemIDs []string `json:"work_item_ids"`
	}
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if err := validateCoordinatorArchiveIDs(args.WorkItemIDs); err != nil {
		return nil, err
	}
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.project = record.ProjectID
	for _, id := range args.WorkItemIDs {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID), ResourceKind: "work_item", ResourceID: id}); err != nil {
			return nil, err
		}
	}
	s := t.coordinator.server
	archive := chat.IssueArchive{Items: make([]chat.IssueArchiveItem, 0, len(args.WorkItemIDs))}
	if scope.credential.Scope == hubapi.APIScopeWorker {
		return nil, hubapi.NativePublicInvalid("Archive and restore require an operator")
	}
	for _, id := range args.WorkItemIDs {
		row, version, err := s.readWorkItemRecord(readCtx, scope, id)
		if err != nil {
			return nil, err
		}
		issue, err := s.hydrateWorkItemRecord(readCtx, row, version)
		if err != nil {
			return nil, err
		}
		if err := requireNativeEdit(issue, issue.Revision); err != nil {
			return nil, err
		}
		if issue.Archived {
			return nil, hubapi.NativeInvalid("Issue is already archived: " + id)
		}
		if issue.State == "Merging" {
			return nil, hubapi.NativeInvalid("A Merging issue cannot be archived: " + id)
		}
		err = s.config.TenantRepository.ReadTenant(readCtx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			active, err := r.WorkItemHasActiveLease(readCtx, scope.project, row.NativeID, s.config.now())
			if err != nil {
				return err
			}
			if active {
				return hubapi.NativeInvalid("Finish or stop the running issue before archiving: " + id)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		archive.Items = append(archive.Items, chat.IssueArchiveItem{WorkItemID: id, Revision: int64(issue.Revision), Number: issue.Number, Title: issue.Title, State: issue.State})
	}
	arguments, err := json.Marshal(archive)
	if err != nil {
		return nil, err
	}
	if len(arguments) > hubapi.CoordinatorToolArgumentBytes {
		return nil, fmt.Errorf("%w: approved archive exceeds %d bytes", errCoordinatorToolArguments, hubapi.CoordinatorToolArgumentBytes)
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionArchiveItems, ProjectID: string(record.ProjectID), Title: fmt.Sprintf("Archive %d issues", len(archive.Items)), Arguments: arguments, Material: true}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

func (s *Service) executeCoordinatorIssueArchive(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	var archive chat.IssueArchive
	if err := decodeCoordinatorArguments(action.Arguments, &archive); err != nil {
		return chat.ActionExecution{}, err
	}
	ids := make([]string, 0, len(archive.Items))
	for _, item := range archive.Items {
		ids = append(ids, item.WorkItemID)
	}
	if err := validateCoordinatorArchiveIDs(ids); err != nil {
		return chat.ActionExecution{}, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	scope.project = tracker.ProjectID(action.ProjectID)
	for _, id := range ids {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: action.ProjectID, ResourceKind: "work_item", ResourceID: id}); err != nil {
			return chat.ActionExecution{}, err
		}
	}
	raw, err := s.archiveCoordinatorWorkItems(ctx, scope, tracker.MutationForContext(ctx, action.RequestID), archive)
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	var names []string
	for _, item := range archive.Items {
		names = append(names, fmt.Sprintf("#%d (%s)", item.Number, item.WorkItemID))
	}
	return chat.ActionExecution{Message: "Archived " + strings.Join(names, ", ") + ". These issues can be restored.", Data: raw}, nil
}

func (s *Service) archiveCoordinatorWorkItems(ctx context.Context, scope nativeScope, command tracker.Mutation, archive chat.IssueArchive) (json.RawMessage, error) {
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if scope.credential.Scope == hubapi.APIScopeWorker {
		return nil, hubapi.NativePublicInvalid("Archive and restore require an operator")
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	ids := make([]string, len(archive.Items))
	for i, item := range archive.Items {
		ids[i] = item.WorkItemID
	}
	if err := validateCoordinatorArchiveIDs(ids); err != nil {
		return nil, err
	}
	lockOrder := slices.Clone(ids)
	slices.Sort(lockOrder)
	operation := nativeOperation(scope, http.MethodPost, "/work-items/archive")
	hash, err := hubapi.CommandHash(archive)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	type preparedItem struct {
		plan    workItemRecordPlan
		fields  workItemRecordFields
		version eventstore.CollaborationVersion
		event   eventstore.Event
		search  searchstore.Document
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, _, err := s.readWorkItemRecord(ctx, scope, id); err != nil {
				return nil, err
			}
		}
		previous, found, err := s.workItemRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if found {
			return content.Receipt(ctx, previous)
		}
		items := make([]preparedItem, 0, len(ids))
		response := make([]tracker.NativeIssue, 0, len(ids))
		for _, item := range archive.Items {
			input := workItemArchive{archiveIssueRequest: archiveIssueRequest{ExpectedRevision: tracker.Revision(item.Revision)}, Archived: true}
			plan, err := s.loadWorkItemPlan(ctx, scope, item.WorkItemID, "", input)
			if err != nil {
				return nil, err
			}
			if plan.issue.Archived || plan.issue.State == "Merging" {
				return nil, hubapi.NativePublicInvalid("The selected issue cannot be archived")
			}
			plan, err = s.planWorkItemRecord(ctx, scope, plan, input)
			if err != nil {
				return nil, err
			}
			prepared := preparedItem{plan: plan}
			prepared.fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(plan.issue)
			if err != nil {
				return nil, err
			}
			prepared.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: item.WorkItemID, Revision: int64(plan.issue.Revision)}, raw)
			if err != nil {
				return nil, err
			}
			raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: plan.issue.WorkItemID, ID: hubapi.NewID("evt"), Type: plan.eventType, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields, Operation: "archive"}})
			if err != nil {
				return nil, err
			}
			prepared.event, err = content.PrepareEvent(ctx, scope.organization, raw)
			if err != nil {
				return nil, err
			}
			prepared.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, ID: "issue:" + item.WorkItemID, SourceID: item.WorkItemID, Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: plan.issue.UpdatedAt}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
			if err != nil {
				return nil, err
			}
			items = append(items, prepared)
			response = append(response, s.nativeIssueResponse(relationshipOmittedIssue(scope.reporterIssue(plan.issue))))
		}
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		now := s.config.now().UTC().Truncate(time.Microsecond)
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			project, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			for _, id := range lockOrder {
				row, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(id))
				if err != nil {
					return err
				}
				if !scope.reporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
					return corestore.ErrNotFound
				}
			}
			if _, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey); err == nil {
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			for _, item := range items {
				if !sameWorkItemProjectPlan(item.plan.project, project) {
					return corestore.ErrSerializationFailure
				}
				if err := writeWorkItemRecord(ctx, w, item.plan, item.fields); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, item.version); err != nil {
					return err
				}
				if err := w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item.version.WorkItemID, Revision: item.version.Revision, VersionSHA256: item.version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(item.plan.issue.Body)}); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, item.event); err != nil {
					return err
				}
				if err := w.PutSearch(ctx, item.search, item.plan.current.Revision); err != nil {
					return err
				}
			}
			if err := s.recordEntryAccessKeyAuditWith(ctx, w, entryKeyAuditFrom(ctx), http.StatusOK); err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		err = errors.Join(err, releaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		s.signalNativeDispatch(scope)
		return raw, nil
	}
}
