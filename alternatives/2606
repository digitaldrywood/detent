package orgsettings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository       tenantstore.Repository
	AccessRepository tenantstore.Repository
	Details          attachment.DetailStore
	Now              func() time.Time
	RecordError      func(error) error
	Unavailable      error
	Recheck          func(context.Context) error
	Receipt          func(context.Context, string, string, string) (json.RawMessage, bool, error)
	Replay           func(context.Context, string, string, any) (json.RawMessage, bool, error)
	Mutate           func(context.Context, string, tracker.Mutation, any, any, func(tenantstore.Writer) error) (json.RawMessage, error)
	RequireFeature   func(context.Context, tenantstore.Reader, string, time.Time) error
	CreateIcon       func(context.Context, tenantstore.Writer, attachment.Metadata) error
	Icons            attachment.IconStore
	Audit            *accessstore.Audit
	Access           accessstore.ProjectAccessFilter
	Granted          func(context.Context, accessstore.Reader, []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error)
	Manager          bool
}

type Service struct{ deps Deps }

func New(deps Deps) *Service {
	if deps.RecordError == nil {
		deps.RecordError = func(err error) error { return err }
	}
	return &Service{deps: deps}
}

type NameSettings struct {
	Name string `json:"name"`
}

type RenameRequest struct {
	tracker.Mutation
	NameSettings
}

func (s *Service) Name(ctx context.Context, scope hubapi.Scope) (string, error) {
	if s.deps.AccessRepository == nil {
		return "", hubapi.WorkItemStorageUnavailable()
	}
	var name string
	err := s.deps.AccessRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		row, err := r.AccessOrganization(ctx)
		name = row.Name
		return err
	})
	return name, s.deps.RecordError(err)
}

func (s *Service) Rename(ctx context.Context, scope hubapi.Scope, request RenameRequest) (json.RawMessage, error) {
	return s.visibleMutation(ctx, scope, "PUT /api/v2/organizations/"+string(scope.Organization)+"/name", request.Mutation, request, request.NameSettings, func(w tenantstore.Writer) error { return w.UpdateAccessOrganizationName(ctx, request.Name) })
}

func (s *Service) visibleMutation(ctx context.Context, scope hubapi.Scope, operation string, command tracker.Mutation, input, value any, apply func(tenantstore.Writer) error) (json.RawMessage, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.PublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if err := s.deps.Recheck(ctx); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.deps.Receipt(ctx, operation, command.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.deps.Details)
	now := s.deps.Now()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var existing *eventstore.Receipt
	err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.CredentialID, operation, command.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.Conflict(0)
			}
			existing = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := apply(w); err != nil {
			return err
		}
		if s.deps.Audit != nil {
			audit := *s.deps.Audit
			audit.Event, audit.Route, audit.Status = "action", strings.Replace(operation, "/organizations/"+string(scope.Organization)+"/", "/organizations/:organization/", 1), http.StatusOK
			audit.RecordedAt = s.deps.Now()
			audit.RecordedAtNS = audit.RecordedAt.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}

func (s *Service) VisibleReader(r tenantstore.Reader) (projectstore.OrganizationReader, error) {
	owner, ok := r.(projectstore.OrganizationReader)
	if !ok {
		return nil, s.deps.Unavailable
	}
	return owner, nil
}

func (s *Service) VisibleWriter(w tenantstore.Writer) (projectstore.OrganizationWriter, error) {
	owner, ok := w.(projectstore.OrganizationWriter)
	if !ok {
		return nil, s.deps.Unavailable
	}
	return owner, nil
}
