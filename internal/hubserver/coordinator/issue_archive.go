package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ArchiveMaxItems = 50

func ValidateArchiveIDs(ids []string) error {
	if len(ids) < 1 || len(ids) > ArchiveMaxItems {
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

func (d IssueProposalDeps) PreviewArchive(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall) (any, error) {
	var args struct {
		WorkItemIDs []string `json:"work_item_ids"`
	}
	if err := DecodeArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if err := ValidateArchiveIDs(args.WorkItemIDs); err != nil {
		return nil, err
	}
	ctx, err := d.ActionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.Project = record.ProjectID
	for _, id := range args.WorkItemIDs {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID), ResourceKind: "work_item", ResourceID: id}); err != nil {
			return nil, err
		}
	}
	s := d.Commands
	archive := chat.IssueArchive{Items: make([]chat.IssueArchiveItem, 0, len(args.WorkItemIDs))}
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return nil, hubapi.NativePublicInvalid("Archive and restore require an operator")
	}
	for _, id := range args.WorkItemIDs {
		row, version, err := s.ReadWorkItem(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		issue, err := s.HydrateIssue(ctx, row, version)
		if err != nil {
			return nil, err
		}
		if err := hubrecords.RequireNativeEdit(issue, issue.Revision); err != nil {
			return nil, err
		}
		if issue.Archived {
			return nil, hubapi.NativeInvalid("Issue is already archived: " + id)
		}
		if issue.State == "Merging" {
			return nil, hubapi.NativeInvalid("A Merging issue cannot be archived: " + id)
		}
		err = s.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			active, err := r.WorkItemHasActiveLease(ctx, scope.Project, row.NativeID, s.Now())
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
		return nil, fmt.Errorf("%w: approved archive exceeds %d bytes", ErrToolArguments, hubapi.CoordinatorToolArgumentBytes)
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionArchiveItems, ProjectID: string(record.ProjectID), Title: fmt.Sprintf("Archive %d issues", len(archive.Items)), Arguments: arguments, Material: true}
	return d.Submit(ctx, record, call, action)
}

func (s IssueCommandDeps) ExecuteArchive(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	var archive chat.IssueArchive
	if err := DecodeArguments(action.Arguments, &archive); err != nil {
		return chat.ActionExecution{}, err
	}
	ids := make([]string, 0, len(archive.Items))
	for _, item := range archive.Items {
		ids = append(ids, item.WorkItemID)
	}
	if err := ValidateArchiveIDs(ids); err != nil {
		return chat.ActionExecution{}, err
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	scope.Project = tracker.ProjectID(action.ProjectID)
	for _, id := range ids {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: action.ProjectID, ResourceKind: "work_item", ResourceID: id}); err != nil {
			return chat.ActionExecution{}, err
		}
	}
	raw, err := s.Archive(ctx, scope, tracker.MutationForContext(ctx, action.RequestID), archive)
	if err != nil {
		return chat.ActionExecution{}, ActionError(err)
	}
	var names []string
	for _, item := range archive.Items {
		names = append(names, fmt.Sprintf("#%d (%s)", item.Number, item.WorkItemID))
	}
	return chat.ActionExecution{Message: "Archived " + strings.Join(names, ", ") + ". These issues can be restored.", Data: raw}, nil
}

func (s IssueCommandDeps) Archive(ctx context.Context, scope hubscope.NativeScope, command tracker.Mutation, archive chat.IssueArchive) (json.RawMessage, error) {
	if !s.StorageAvailable {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return nil, hubapi.NativePublicInvalid("Archive and restore require an operator")
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	ids := make([]string, len(archive.Items))
	for i, item := range archive.Items {
		ids[i] = item.WorkItemID
	}
	if err := ValidateArchiveIDs(ids); err != nil {
		return nil, err
	}
	lockOrder := slices.Clone(ids)
	slices.Sort(lockOrder)
	operation := s.Operation(scope, http.MethodPost, "/work-items/archive")
	hash, err := hubapi.CommandHash(archive)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.Details())
	type preparedItem struct {
		plan    hubrecords.WorkItemRecordPlan
		fields  hubrecords.WorkItemRecordFields
		version eventstore.CollaborationVersion
		event   eventstore.Event
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.RecheckMutation(ctx, scope); err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, _, err := s.ReadWorkItem(ctx, scope, id); err != nil {
				return nil, err
			}
		}
		previous, found, err := s.Receipt(ctx, scope, operation, command.IdempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if found {
			return content.Receipt(ctx, previous)
		}
		items := make([]preparedItem, 0, len(ids))
		response := make([]tracker.NativeIssue, 0, len(ids))
		for _, item := range archive.Items {
			plan, err := s.LoadArchivePlan(ctx, scope, item.WorkItemID, tracker.Revision(item.Revision))
			if err != nil {
				return nil, err
			}
			if plan.Issue.Archived || plan.Issue.State == "Merging" {
				return nil, hubapi.NativePublicInvalid("The selected issue cannot be archived")
			}
			plan, err = s.PlanArchive(ctx, scope, plan, tracker.Revision(item.Revision))
			if err != nil {
				return nil, err
			}
			prepared := preparedItem{plan: plan}
			prepared.fields, err = s.PrepareFields(ctx, scope.Organization, plan)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(plan.Issue)
			if err != nil {
				return nil, err
			}
			prepared.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RecordID: item.WorkItemID, Revision: int64(plan.Issue.Revision)}, raw)
			if err != nil {
				return nil, err
			}
			raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: plan.Issue.WorkItemID, ID: hubapi.NewID("evt"), Type: plan.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: plan.Issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, Fields: plan.Fields, Operation: "archive"}})
			if err != nil {
				return nil, err
			}
			prepared.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
			if err != nil {
				return nil, err
			}
			items = append(items, prepared)
			response = append(response, s.IssueResponse(hubrecords.RelationshipOmittedIssue(scope.ReporterIssue(plan.Issue))))
		}
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		now := s.Now().UTC().Truncate(time.Microsecond)
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		authority, err := s.BeginAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		err = s.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.Authorize(ctx, w, scope); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			project, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			for _, id := range lockOrder {
				row, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(id))
				if err != nil {
					return err
				}
				if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
					return corestore.ErrNotFound
				}
			}
			if _, err := w.Receipt(ctx, scope.Credential.ID, operation, command.IdempotencyKey); err == nil {
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			for _, item := range items {
				if !hubrecords.SameWorkItemProjectPlan(item.plan.Project, project) {
					return corestore.ErrSerializationFailure
				}
				if err := hubrecords.WriteWorkItemRecord(ctx, w, item.plan, item.fields); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, item.version); err != nil {
					return err
				}
				if err := w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item.version.WorkItemID, Revision: item.version.Revision, VersionSHA256: item.version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(item.plan.Issue.Body)}); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, item.event); err != nil {
					return err
				}
			}
			if err := s.RecordEntryAudit(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK); err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		err = errors.Join(err, hubrecords.ReleaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return nil, hubapi.ProjectRecordError(err)
		}
		s.SignalDispatch(scope)
		return raw, nil
	}
}
