package releasefeed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Delivery) RecordReleasePublicationCommand(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.PublishReleasePick, result tracker.ReleasePickPublication) (json.RawMessage, error) {
	operation := releasePublicationOperation(scope, item)
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.deps.RunDetailStore())
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.deps.Clock()}, raw)
	if err != nil {
		return nil, err
	}
	if err := s.deps.RecheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
			return err
		}
		if err := s.deps.RequireCollaboration(ctx, w, s.deps.Clock()); err != nil {
			return err
		}
		prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if prior.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			replay = &prior
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if scope.Credential.Hosted != nil {
			audit := s.deps.ProjectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status, audit.ProjectID = "action", operation, http.StatusOK, scope.Project
			audit.RecordedAt = s.deps.Clock()
			audit.RecordedAtNS = audit.RecordedAt.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if replay != nil {
		raw, err = content.Receipt(ctx, *replay)
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}
