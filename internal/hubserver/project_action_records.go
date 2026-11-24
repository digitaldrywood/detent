package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) actionRecords(ctx context.Context, scope hubscope.NativeScope) ([]actionstore.Record, error) {
	var records []actionstore.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(actionstore.Reader)
		if !ok {
			return projectActionsUnavailable("Project action storage is unavailable")
		}
		var err error
		records, err = reader.ProjectActions(ctx, scope.Project)
		return err
	})
	return records, hubapi.ProjectRecordError(err)
}

func (s *Service) hydrateActionRecord(ctx context.Context, scope hubscope.NativeScope, r actionstore.Record) (workspacesession.Action, error) {
	if r.Command.Validate(string(scope.Organization)) != nil {
		return workspacesession.Action{}, artifact.ErrIntegrity
	}
	raw, err := s.runDetailStore().ReadDetail(ctx, string(scope.Organization), r.Command)
	if err != nil {
		return workspacesession.Action{}, err
	}
	r.Action.Command = string(raw)
	return r.Action, nil
}

func (s *Service) readActionRecords(ctx context.Context, scope hubscope.NativeScope) (json.RawMessage, error) {
	records, err := s.actionRecords(ctx, scope)
	if err != nil {
		return nil, err
	}
	result := projectActionList{Items: make([]workspacesession.Action, 0, len(records))}
	for _, r := range records {
		a, err := s.hydrateActionRecord(ctx, scope, r)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, a)
	}
	return json.Marshal(result)
}

func normalizeActionRecord(a *workspacesession.Action) error {
	fields := []struct {
		value     *string
		normalize func(string) (string, error)
	}{
		{&a.Name, workspacesession.NormalizeActionName}, {&a.Command, workspacesession.NormalizeActionCommand}, {&a.Keybinding, workspacesession.NormalizeActionKeybinding}, {&a.Icon, workspacesession.NormalizeActionIcon}, {&a.PreviewURL, workspacesession.NormalizeActionPreviewURL},
	}
	for _, f := range fields {
		value, err := f.normalize(*f.value)
		if err != nil {
			if f.value == &a.Icon {
				return hubapi.NativeInvalid(err.Error())
			}
			return hubapi.NativePublicInvalid(err.Error())
		}
		*f.value = value
	}
	return nil
}

func (s *Service) mutateActionRecord(ctx context.Context, scope hubscope.NativeScope, method, id string, mutation tracker.Mutation, input any) (json.RawMessage, error) {
	if method != "DELETE" && (strings.TrimSpace(mutation.IdempotencyKey) == "" || len(mutation.IdempotencyKey) > 128) {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	suffix := "/actions"
	if id != "" {
		suffix += "/" + url.PathEscape(id)
	}
	operation := nativeOperation(scope, method, suffix)
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	if method != "DELETE" {
		if receipt, found, err := s.workItemRecordReceipt(ctx, scope, operation, mutation.IdempotencyKey, hash); found || err != nil {
			if err != nil {
				return nil, err
			}
			return content.Receipt(ctx, receipt)
		}
	}
	records, err := s.actionRecords(ctx, scope)
	if err != nil {
		return nil, err
	}
	var previous actionstore.Record
	for _, r := range records {
		if r.Action.ID == id {
			previous = r
		}
	}
	now := s.config.Clock().UTC()
	next := previous
	switch request := input.(type) {
	case projectActionRequest:
		if len(records) >= workspacesession.MaxActionsPerProject {
			return nil, hubapi.NativePublicInvalid("A project may hold at most 50 actions")
		}
		next.Action = workspacesession.Action{ID: hubapi.NewID("action"), OrganizationID: string(scope.Organization), ProjectID: string(scope.Project), Name: request.Name, Command: request.Command, Keybinding: request.Keybinding, Icon: request.Icon, PreviewURL: request.PreviewURL, OpenPreview: request.OpenPreview, RunOnWorktreeCreation: request.RunOnWorktreeCreation, CreatedBy: scope.Actor().PrincipalID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	case projectActionPatch:
		if previous.Action.ID == "" {
			return nil, hubapi.NativeNotFound()
		}
		if request.ExpectedRevision <= 0 {
			return nil, hubapi.NativePublicInvalid("Expected revision must be positive")
		}
		if int64(request.ExpectedRevision) != previous.Action.Revision {
			return nil, hubapi.NativeConflict(tracker.Revision(previous.Action.Revision))
		}
		next.Action, err = s.hydrateActionRecord(ctx, scope, previous)
		if err != nil {
			return nil, err
		}
		for _, field := range []struct {
			from *string
			to   *string
		}{{request.Name, &next.Action.Name}, {request.Command, &next.Action.Command}, {request.Keybinding, &next.Action.Keybinding}, {request.Icon, &next.Action.Icon}, {request.PreviewURL, &next.Action.PreviewURL}} {
			if field.from != nil {
				*field.to = *field.from
			}
		}
		if request.OpenPreview != nil {
			next.Action.OpenPreview = *request.OpenPreview
		}
		if request.RunOnWorktreeCreation != nil {
			next.Action.RunOnWorktreeCreation = *request.RunOnWorktreeCreation
		}
		next.Action.Revision++
		next.Action.UpdatedAt = now
	default:
		if previous.Action.ID == "" {
			return nil, hubapi.NativeNotFound()
		}
	}
	raw := json.RawMessage(`{"deleted":true}`)
	var receipt eventstore.Receipt
	if method != "DELETE" {
		if err := normalizeActionRecord(&next.Action); err != nil {
			return nil, err
		}
		command := []byte(next.Action.Command)
		next.Command, err = s.runDetailStore().StoreDetail(ctx, string(scope.Organization), command)
		if err != nil {
			return nil, err
		}
		if next.Command.Validate(string(scope.Organization)) != nil || next.Command.BindingID != "" || next.Command.CustomerOwned || next.Command.SchemaVersion != 1 || next.Command.UncompressedSize != int64(len(command)) {
			return nil, artifact.ErrIntegrity
		}
		verified, err := s.runDetailStore().ReadDetail(ctx, string(scope.Organization), next.Command)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(command, verified) {
			return nil, artifact.ErrIntegrity
		}
		raw, err = json.Marshal(next.Action)
		if err != nil {
			return nil, err
		}
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: mutation.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		writer, ok := w.(actionstore.Writer)
		if !ok {
			return projectActionsUnavailable("Project action storage is unavailable")
		}
		if method != "DELETE" {
			stored, err := w.Receipt(ctx, scope.Credential.ID, operation, mutation.IdempotencyKey)
			if err == nil {
				if stored.RequestHash != hash {
					return hubapi.NativeConflict(0)
				}
				replay = &stored
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if method != "DELETE" {
			if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", s.config.Clock())); err != nil {
				return err
			}
			if err := s.chargeProjectCommand(ctx, w, scope, operation, mutation.IdempotencyKey); err != nil {
				return err
			}
		}
		var err error
		if previous.Action.ID != "" {
			current, err := writer.ProjectAction(ctx, scope.Project, previous.Action.ID)
			if err != nil {
				return err
			}
			if current.Action.Revision != previous.Action.Revision {
				return hubapi.NativeConflict(tracker.Revision(current.Action.Revision))
			}
		}
		if previous.Action.ID != "" && (method == "DELETE" || previous.Command != next.Command) {
			if err := w.LockArchiveAccounting(ctx); err != nil {
				return err
			}
		}
		if method == "DELETE" {
			err = writer.DeleteProjectAction(ctx, scope.Project, id)
		} else {
			rows, err := writer.ProjectActions(ctx, scope.Project)
			if err != nil {
				return err
			}
			if method == "POST" && len(rows) >= workspacesession.MaxActionsPerProject {
				return hubapi.NativePublicInvalid("A project may hold at most 50 actions")
			}
			for _, r := range rows {
				if next.Action.Keybinding != "" && r.Action.ID != next.Action.ID && r.Action.Keybinding == next.Action.Keybinding {
					return actionKeybindingTaken()
				}
			}
			if method == "POST" {
				err = writer.CreateProjectAction(ctx, next)
			} else {
				err = writer.UpdateProjectAction(ctx, next, previous.Action.Revision)
			}
			if err != nil {
				return err
			}
			object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: hubapi.NewID("att"), Reference: next.Command, CreatedAt: now})
			if err != nil {
				return err
			}
			if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(scope.Project, next.Action.ID), CreatedAt: now}); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		if previous.Action.ID != "" && (method == "DELETE" || previous.Command != next.Command) {
			object, err := w.ArchiveObjectByReference(ctx, previous.Command)
			if err != nil {
				return err
			}
			if err := w.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(scope.Project, previous.Action.ID), CreatedAt: now}); err != nil {
				return err
			}
		}
		if method != "DELETE" {
			_, err = w.PutReceipt(ctx, receipt)
		}
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

func projectActionsUnavailable(message string) error {
	return &hubapi.NativeError{Code: "not_ready", Message: message, Status: http.StatusServiceUnavailable, PublicMessage: true}
}
