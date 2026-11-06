package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseauditstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerReleaseAuditRoutes(e *echo.Echo) {
	authority := s.requireNativeScope(apiScopeOperator)
	e.GET(nativeBase+"/release-audits", s.listReleaseAudits, authority)
	e.GET(nativeBase+"/release-audits/:checkpoint", s.getReleaseAudit, authority)
	e.POST(nativeBase+"/release-audits", s.publishReleaseAudit, authority)
}

func (s *Service) releaseAuditRead(c echo.Context, read func(context.Context, releaseauditstore.Reader, nativeScope) (any, error)) error {
	scope := nativeRequestScope(c)
	if scope.credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var value any
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		owner, ok := r.(releaseauditstore.Reader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		value, err = read(ctx, owner, scope)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) getReleaseAudit(c echo.Context) error {
	return s.releaseAuditRead(c, func(ctx context.Context, r releaseauditstore.Reader, scope nativeScope) (any, error) {
		return r.ReleaseAudit(ctx, scope.project, c.Param("checkpoint"))
	})
}

func (s *Service) listReleaseAudits(c echo.Context) error {
	if nativeRequestScope(c).credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if len(c.QueryParam("cursor")) > 64 {
		return s.nativeAPIError(c, nativeInvalid("Invalid release audit cursor"))
	}
	return s.releaseAuditRead(c, func(ctx context.Context, r releaseauditstore.Reader, scope nativeScope) (any, error) {
		return r.ReleaseAudits(ctx, scope.project, c.QueryParam("cursor"))
	})
}

type releaseAuditRequest struct {
	tracker.Mutation
	releaseaudit.Checkpoint
}

func (s *Service) publishReleaseAudit(c echo.Context) error {
	scope := nativeRequestScope(c)
	if scope.credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var request releaseAuditRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	raw, err := s.publishReleaseAuditRecord(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) publishReleaseAuditRecord(ctx context.Context, scope nativeScope, request releaseAuditRequest) (json.RawMessage, error) {
	if !request.Valid() || request.Identity.ProjectID != string(scope.project) {
		return nil, nativeInvalid("Invalid release audit checkpoint")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativeInvalid("An idempotency key of at most 128 bytes is required")
	}
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	operation := nativeOperation(scope, http.MethodPost, "/release-audits")
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(request.Checkpoint)
	if err != nil {
		return nil, err
	}
	now := s.config.now()
	content := eventstore.NewContent(s.runDetailStore())
	prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		owner, ok := w.(releaseauditstore.Writer)
		if !ok {
			return workItemStorageUnavailable()
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
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		repository := project.Settings.CheckoutRepository
		if repository == "" {
			repository = project.Settings.Repository
		}
		if !strings.EqualFold(repository, request.Identity.Repository) {
			return nativeInvalid("Release audit repository does not match the selected project")
		}
		if err := owner.PutReleaseAudit(ctx, scope.project, request.Checkpoint, now); err != nil {
			return err
		}
		if scope.credential.Hosted != nil {
			audit := projectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status = "action", operation, http.StatusOK
			audit.ProjectID = scope.project
			audit.RecordedAt = now
			audit.RecordedAtNS = now.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, prepared)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, nativeConflict(0)
	}
	if errors.Is(err, eventstore.ErrInvalidRecord) {
		return nil, nativeInvalid("Release audit evidence does not match its retained attachment")
	}
	if err != nil {
		return nil, projectRecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
