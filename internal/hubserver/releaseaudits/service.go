package releaseaudits

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseauditstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository     tenantstore.Repository
	Now            func() time.Time
	Details        func() attachment.DetailStore
	Scope          func(echo.Context) hubapi.Scope
	Decode         func(echo.Context, any) error
	Error          func(echo.Context, error) error
	InvalidRequest func(echo.Context, error) error
	RecordError    func(error) error
	Recheck        func(context.Context, hubapi.Scope) error
	RequireFeature func(context.Context, tenantstore.Reader, string, time.Time) error
	Audit          func(hubapi.Scope) (accessstore.Audit, bool)
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) read(c echo.Context, read func(context.Context, releaseauditstore.Reader, hubapi.Scope) (any, error)) error {
	scope := s.deps.Scope(c)
	if scope.HostedRole == "reporter" {
		return s.deps.Error(c, hubapi.NotFound())
	}
	if s.deps.Repository == nil {
		return s.deps.Error(c, hubapi.WorkItemStorageUnavailable())
	}
	ctx := c.Request().Context()
	var value any
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		owner, ok := r.(releaseauditstore.Reader)
		if !ok {
			return hubapi.WorkItemStorageUnavailable()
		}
		var err error
		value, err = read(ctx, owner, scope)
		return err
	})
	if err != nil {
		return s.deps.Error(c, s.deps.RecordError(err))
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) Get(c echo.Context) error {
	return s.read(c, func(ctx context.Context, r releaseauditstore.Reader, scope hubapi.Scope) (any, error) {
		return r.ReleaseAudit(ctx, scope.Project, c.Param("checkpoint"))
	})
}

func (s *Service) List(c echo.Context) error {
	if s.deps.Scope(c).HostedRole == "reporter" {
		return s.deps.Error(c, hubapi.NotFound())
	}
	if len(c.QueryParam("cursor")) > 64 {
		return s.deps.Error(c, hubapi.Invalid("Invalid release audit cursor"))
	}
	return s.read(c, func(ctx context.Context, r releaseauditstore.Reader, scope hubapi.Scope) (any, error) {
		return r.ReleaseAudits(ctx, scope.Project, c.QueryParam("cursor"))
	})
}

type Request struct {
	tracker.Mutation
	releaseaudit.Checkpoint
}

func (s *Service) Publish(c echo.Context) error {
	scope := s.deps.Scope(c)
	if scope.HostedRole == "reporter" {
		return s.deps.Error(c, hubapi.NotFound())
	}
	var request Request
	if err := s.deps.Decode(c, &request); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	raw, err := s.PublishRecord(c.Request().Context(), scope, request)
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func operation(scope hubapi.Scope, method, suffix string) string {
	return method + " /api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + suffix
}

func (s *Service) PublishRecord(ctx context.Context, scope hubapi.Scope, request Request) (json.RawMessage, error) {
	if !request.Valid() || request.Identity.ProjectID != string(scope.Project) {
		return nil, hubapi.Invalid("Invalid release audit checkpoint")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.Invalid("An idempotency key of at most 128 bytes is required")
	}
	if s.deps.Repository == nil {
		return nil, hubapi.WorkItemStorageUnavailable()
	}
	if err := s.deps.Recheck(ctx, scope); err != nil {
		return nil, err
	}
	operation := operation(scope, http.MethodPost, "/release-audits")
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(request.Checkpoint)
	if err != nil {
		return nil, err
	}
	now := s.deps.Now()
	content := eventstore.NewContent(s.deps.Details())
	prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		owner, ok := w.(releaseauditstore.Writer)
		if !ok {
			return hubapi.WorkItemStorageUnavailable()
		}
		prior, err := w.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey)
		if err == nil {
			if prior.RequestHash != hash {
				return hubapi.Conflict(0)
			}
			replay = &prior
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.deps.RequireFeature(ctx, w, "collaboration", now); err != nil {
			return err
		}
		project, err := w.ProjectMetadata(ctx, scope.Project)
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
			return hubapi.Invalid("Release audit repository does not match the selected project")
		}
		if err := owner.PutReleaseAudit(ctx, scope.Project, request.Checkpoint, now); err != nil {
			return err
		}
		if audit, hosted := s.deps.Audit(scope); hosted {
			audit.Event, audit.Route, audit.Status = "action", operation, http.StatusOK
			audit.ProjectID = scope.Project
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
		return nil, hubapi.Conflict(0)
	}
	if errors.Is(err, eventstore.ErrInvalidRecord) {
		return nil, hubapi.Invalid("Release audit evidence does not match its retained attachment")
	}
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
