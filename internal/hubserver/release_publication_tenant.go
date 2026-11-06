package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) recordReleasePublication(c echo.Context, request tracker.PublishReleasePick, result tracker.ReleasePickPublication) error {
	ctx, scope := c.Request().Context(), nativeRequestScope(c)
	operation := releasePublicationOperation(scope, c.Param("item"))
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return s.nativeAPIError(c, err)
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
				return nativeConflict(0)
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
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if replay != nil {
		raw, err = content.Receipt(ctx, *replay)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSONBlob(http.StatusOK, raw)
}
