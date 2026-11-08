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
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) mutateQueueRecord(ctx context.Context, scope nativeScope, name string, request operatortool.WorkArguments, command tracker.Mutation) (json.RawMessage, error) {
	if request.Scope != string(scope.project) {
		if scope.credential.EntryKey != nil && (scope.credential.EntryKey.Access != "all" || len(scope.credential.EntryKey.BlockedProjects) != 0) {
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
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
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
	now := s.config.now().UTC()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "github_compatible" {
			return nativePublicInvalid("Queue ordering and priority overrides require a GitHub-compatible project; native projects use issue priority")
		}
		row, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(request.Identifier))
		if err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= previous.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			replay = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
			return err
		}
		validState := false
		for _, state := range project.States {
			if state.Name == request.State {
				validState = true
			}
		}
		if !validState {
			return nativePublicInvalid("Queue state is not configured")
		}
		entry, err := w.QueueEntry(ctx, scope.project, row.NativeID, request.Scope)
		if errors.Is(err, corestore.ErrNotFound) {
			entry = claimstore.QueueEntry{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: row.NativeID, Scope: request.Scope, CreatedAt: now}
		} else if err != nil {
			return err
		}
		entry.State, entry.UpdatedAt = request.State, now
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
		if err := s.recordEntryAccessKeyAuditWith(ctx, w, entryKeyAuditFrom(ctx), http.StatusOK); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
