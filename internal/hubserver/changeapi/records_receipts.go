package changeapi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *RecordReceipts) ChangeRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key string, hashes ...string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(ctx, scope.Credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, hubapi.ProjectRecordError(err)
	}
	if !slices.Contains(hashes, receipt.RequestHash) {
		return nil, false, hubapi.NativeConflict(0)
	}
	value, err := eventstore.NewContent(s.deps.RunDetailStore()).Receipt(ctx, receipt)
	return value, true, err
}

func (s *RecordReceipts) AuthorizedChangeRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	if !s.deps.HasAccessRepository {
		return nil, false, hubapi.NativeNotFound()
	}
	var receipt eventstore.Receipt
	err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.deps.AuthorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = w.Receipt(ctx, scope.Credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, hubapi.ProjectRecordError(err)
	}
	if receipt.RequestHash != hash {
		return nil, false, hubapi.NativeConflict(0)
	}
	value, err := eventstore.NewContent(s.deps.RunDetailStore()).Receipt(ctx, receipt)
	return value, true, err
}

func (s *RecordReceipts) LockChangeRecordOperation(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item, changeID, versionID string) (eventstore.Change, error) {
	if err := s.deps.AuthorizeWorkItemRecordScope(ctx, w, scope); err != nil {
		return eventstore.Change{}, err
	}
	if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
		return eventstore.Change{}, err
	}
	project, err := w.Project(ctx, scope.Project)
	if err != nil {
		return eventstore.Change{}, err
	}
	if project.DeletedAt != nil {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	var issue corestore.WorkItem
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		issue, err = w.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
	} else {
		issue, err = w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
	}
	if err != nil {
		return eventstore.Change{}, err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(issue.ActorPrincipalID)}) {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	var change eventstore.Change
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		change, err = w.Change(ctx, changeID)
	} else {
		change, err = w.LockChange(ctx, changeID)
	}
	if err != nil {
		return eventstore.Change{}, err
	}
	if change.ProjectID != scope.Project {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	if change.WorkItemID != tracker.NativeWorkItemID(item) {
		return eventstore.Change{}, corestore.ErrNotFound
	}
	if versionID != "" {
		if _, err := w.Version(ctx, changeID, versionID); err != nil {
			return eventstore.Change{}, err
		}
	}
	return change, nil
}

func (s *RecordReceipts) ChangeOperationRecordReceipt(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation, key, hash string) (json.RawMessage, bool, error) {
	var receipt eventstore.Receipt
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.deps.AuthorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, scope.Credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, hubapi.ProjectRecordError(err)
	}
	var raw json.RawMessage
	var payloadErr error
	if receipt.RequestHash == hash {
		raw, payloadErr = eventstore.NewContent(s.deps.RunDetailStore()).Receipt(ctx, receipt)
	}
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := s.LockChangeRecordOperation(ctx, w, scope, item, changeID, versionID); err != nil {
			return err
		}
		current, err := w.Receipt(ctx, scope.Credential.ID, operation, key)
		if err != nil {
			return err
		}
		if current.RequestHash != hash {
			return hubapi.NativeConflict(0)
		}
		if current.ResponseExpired || current.ResponseReplayUntilNs > 0 && s.deps.Clock().UnixNano() >= current.ResponseReplayUntilNs {
			return receiptstore.ErrExpired
		}
		if !reflect.DeepEqual(current, receipt) {
			return hubapi.NativeConflict(0)
		}
		return payloadErr
	})
	if err != nil {
		return nil, false, hubapi.ProjectRecordError(err)
	}
	return raw, true, nil
}
