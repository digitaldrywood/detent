package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) recordReleasePublicationCommand(ctx context.Context, scope nativeScope, item string, request tracker.PublishReleasePick, result tracker.ReleasePickPublication) (json.RawMessage, error) {
	operation := releasePublicationOperation(scope, item)
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
			return err
		}
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
			return err
		}
		prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
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
		if scope.credential.Hosted != nil {
			audit := projectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status, audit.ProjectID = "action", operation, http.StatusOK, scope.project
			audit.RecordedAt = s.config.now()
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
