package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) mutateQueueRecord(ctx context.Context, scope hubscope.NativeScope, name string, request operatortool.WorkArguments, command tracker.Mutation) (json.RawMessage, error) {
	if request.Scope != string(scope.Project) {
		if scope.Credential.EntryKey != nil && (scope.Credential.EntryKey.Access != "all" || len(scope.Credential.EntryKey.BlockedProjects) != 0) {
			return nil, operatortool.ErrAccessDenied
		}
		var err error
		ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationWide: true, Action: name})
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, operatortool.ErrInvalidArguments
	}
	operation := nativeOperation(scope, http.MethodPost, "/work-items/"+request.Identifier+"/"+name)
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	var outbound *projectstore.GitHubOutbound
	var source corestore.WorkItem
	var sourceProject projectstore.Metadata
	var orderEvent *eventstore.Event
	if name == operatortool.SetQueuePriority || name == operatortool.OrderItem {
		var version eventstore.CollaborationVersion
		source, version, err = s.readWorkItemRecord(ctx, scope, request.Identifier)
		if err != nil {
			return nil, err
		}
		if name == operatortool.OrderItem {
			raw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "work_item", AggregateID: source.NativeID, ID: hubapi.NewID("evt"), Type: "queue.order_changed", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: s.config.Clock().UTC(), Data: tracker.CollaborationData{Revision: tracker.Revision(source.Revision), Operation: "order", Fields: []string{"queue_order"}}})
			if err != nil {
				return nil, err
			}
			event, err := eventstore.NewContent(s.runDetailStore()).PrepareEvent(ctx, scope.Organization, raw)
			if err != nil {
				return nil, err
			}
			orderEvent = &event
		} else {
			sourceProject, err = s.readProjectMetadata(ctx, scope)
			if err != nil {
				return nil, err
			}
			issue, err := s.hydrateCanonicalWorkItemRecord(ctx, source, version)
			if err != nil {
				return nil, err
			}
			repository, number, err := cutoverSourceReference(issue)
			if err == nil && sourceProject.Settings.RepositoryEnabled && strings.EqualFold(repository, strings.TrimSpace(sourceProject.Settings.Repository)) {
				desired := WorkflowLabelDesired{ManagedPrefix: "priority:", Clear: request.QueuePriority == "none"}
				if !desired.Clear {
					if _, valid := queuePriority(request.QueuePriority); !valid {
						return nil, operatortool.ErrInvalidArguments
					}
					desired.Label = "priority:" + request.QueuePriority
				}
				raw, err := json.Marshal(desired)
				if err != nil {
					return nil, err
				}
				payload, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, raw)
				if err != nil {
					return nil, err
				}
				key, err := hubapi.CommandHash(struct{ Operation, ActorID, CommandKey string }{operation, scope.Credential.ID, command.IdempotencyKey})
				if err != nil {
					return nil, err
				}
				outbound = &projectstore.GitHubOutbound{ProjectID: scope.Project, WorkItemID: source.NativeID, Key: "queue-priority:" + key, Repository: repository, IssueNumber: int64(number), Kind: string(MutationWorkflowLabel), CreatedAt: s.config.Clock().UTC(), Payload: payload, CoalesceKey: "managed-label:" + repository + ":" + string(source.NativeID) + ":priority:"}
			}
		}
	}

	content := eventstore.NewContent(s.runDetailStore())
	raw, err := json.Marshal(struct {
		WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
		Kind       string                   `json:"kind"`
		Status     string                   `json:"status"`
	}{tracker.NativeWorkItemID(request.Identifier), name, "applied"})
	if err != nil {
		return nil, err
	}
	now := s.config.Clock().UTC()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "github_compatible" {
			return hubapi.NativePublicInvalid("Queue ordering and priority overrides require a GitHub-compatible project; native projects use issue priority")
		}
		row, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(request.Identifier))
		if err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.Credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			replay = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if row.Revision != source.Revision || name == operatortool.SetQueuePriority && project.Settings.Revision != sourceProject.Settings.Revision {
			return hubapi.NativeConflict(tracker.Revision(row.Revision))
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", now)); err != nil {
			return err
		}
		validState := false
		for _, state := range project.States {
			if state.Name == request.State {
				validState = true
			}
		}
		if !validState {
			return hubapi.NativePublicInvalid("Queue state is not configured")
		}
		entry, err := w.QueueEntry(ctx, scope.Project, row.NativeID, request.Scope)
		if errors.Is(err, corestore.ErrNotFound) {
			entry = claimstore.QueueEntry{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, Scope: request.Scope, CreatedAt: now}
		} else if err != nil {
			return err
		}
		entry.State = request.State
		entry.Touch(now)
		if name == operatortool.OrderItem {
			entry.Rank = request.Rank
		} else {
			entry.Priority = nil
			if request.QueuePriority != "none" {
				level, ok := queuePriority(request.QueuePriority)
				if !ok {
					return operatortool.ErrInvalidArguments
				}
				entry.Priority = new(int64(level))
			}
		}
		if err := w.PutQueueEntry(ctx, entry); err != nil {
			return err
		}
		if orderEvent != nil {
			if _, err := w.AppendEvent(ctx, *orderEvent); err != nil {
				return err
			}
		}
		if outbound != nil {
			owner, ok := w.(projectstore.GitHubOutboxWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			if err := owner.EnqueueGitHubOutbound(ctx, *outbound, s.outbox.wake); err != nil {
				return err
			}
		}
		if err := s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
