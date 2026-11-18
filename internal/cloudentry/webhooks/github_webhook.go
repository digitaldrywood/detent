package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/cloudentry/internal/keylock"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubwebhook"
)

type githubRoutes struct {
	mu             sync.Mutex
	refreshes      keylock.Mutex
	byRepository   map[string]map[string]struct{}
	byOrganization map[string][]string
	unavailable    map[string]bool
}

func (s *Service) Initialize(ctx context.Context) error {
	if len(s.deps.Secret) == 0 || s.deps.Accept != nil {
		return nil
	}
	organizations, err := s.deps.List(ctx)
	if err != nil {
		return err
	}
	for _, organization := range organizations {
		if organization.State != "ready" {
			continue
		}
		if organization.Managed && s.deps.StartManaged {
			s.githubRoutes.mu.Lock()
			if s.githubRoutes.unavailable == nil {
				s.githubRoutes.unavailable = make(map[string]bool)
			}
			s.githubRoutes.unavailable[organization.ID] = true
			s.githubRoutes.mu.Unlock()
			err = s.deps.StartTenant(ctx, &organization)
		} else {
			err = s.Refresh(ctx, organization)
		}
		if err != nil {
			s.deps.Logger.Warn("read tenant GitHub repository bindings", "organization", organization.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) Refresh(ctx context.Context, organization tenant.Organization) error {
	if len(s.deps.Secret) == 0 || s.deps.Accept != nil {
		return nil
	}
	routes := &s.githubRoutes
	unlock := routes.refreshes.Lock(organization.ID)
	defer unlock()
	routes.mu.Lock()
	if routes.byRepository == nil {
		routes.byRepository = make(map[string]map[string]struct{})
		routes.byOrganization = make(map[string][]string)
	}
	if routes.unavailable == nil {
		routes.unavailable = make(map[string]bool)
	}
	if _, loaded := routes.byOrganization[organization.ID]; !loaded {
		routes.unavailable[organization.ID] = true
	}
	routes.mu.Unlock()
	status, raw, err := s.deps.ServiceCall(ctx, organization, "/internal/v1/github/repositories", struct{}{}, nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("tenant GitHub repository bindings returned %d", status)
	}
	var repositories []string
	if err == nil {
		err = json.Unmarshal(raw, &repositories)
	}
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if err != nil {
		routes.unavailable[organization.ID] = true
		return err
	}
	for _, repository := range routes.byOrganization[organization.ID] {
		delete(routes.byRepository[repository], organization.ID)
		if len(routes.byRepository[repository]) == 0 {
			delete(routes.byRepository, repository)
		}
	}
	for _, repository := range repositories {
		if routes.byRepository[repository] == nil {
			routes.byRepository[repository] = make(map[string]struct{})
		}
		routes.byRepository[repository][organization.ID] = struct{}{}
	}
	routes.byOrganization[organization.ID] = repositories
	delete(routes.unavailable, organization.ID)
	return nil
}

func (s *Service) Webhook(c echo.Context) error {
	fail := func(status int, code, message string) error {
		return c.JSON(status, map[string]string{"code": code, "message": message})
	}
	if len(s.deps.Secret) == 0 {
		return fail(http.StatusServiceUnavailable, "webhook_unavailable", "GitHub webhook verification is not configured")
	}
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, cloudassert.MaxBodyBytes)
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return fail(http.StatusRequestEntityTooLarge, "payload_too_large", "GitHub webhook payload is too large")
		}
		return fail(http.StatusBadRequest, "invalid_payload", "GitHub webhook payload is invalid")
	}
	if !github.ValidWebhookSignature(s.deps.Secret, payload, request.Header.Get("X-Hub-Signature-256")) {
		return fail(http.StatusUnauthorized, "invalid_signature", "GitHub webhook signature is invalid")
	}
	deliveryID := strings.TrimSpace(request.Header.Get("X-GitHub-Delivery"))
	eventType := strings.TrimSpace(request.Header.Get("X-GitHub-Event"))
	if deliveryID == "" || len(deliveryID) > 256 || eventType == "" || len(eventType) > 100 {
		return fail(http.StatusBadRequest, "invalid_headers", "GitHub webhook delivery and event headers are required")
	}
	_, repository, err := githubwebhook.ParsePayload(payload, eventType)
	if err != nil {
		return fail(http.StatusBadRequest, "invalid_payload", "GitHub webhook payload is invalid")
	}
	delivery := Delivery{ID: deliveryID, Event: eventType, Repository: repository, HookID: request.Header.Get("X-GitHub-Hook-ID"), InstallationTarget: request.Header.Get("X-GitHub-Hook-Installation-Target-ID"), UserAgent: request.UserAgent()}
	ctx, cancel := context.WithTimeout(request.Context(), tenant.CallTimeout)
	defer cancel()
	var duplicate bool
	if s.deps.Accept != nil {
		duplicate, err = s.deps.Accept(ctx, delivery, payload)
	} else {
		duplicate, err = s.deliverRegistered(ctx, delivery, payload)
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return fail(http.StatusConflict, "delivery_conflict", "The delivery ID already belongs to another event")
	}
	if err != nil {
		s.deps.Logger.Warn("accept hosted GitHub webhook", "error", err)
		return fail(http.StatusServiceUnavailable, "tenant_unavailable", "GitHub webhook delivery is temporarily unavailable")
	}
	return c.JSON(http.StatusAccepted, struct {
		DeliveryID string `json:"delivery_id"`
		Duplicate  bool   `json:"duplicate"`
	}{deliveryID, duplicate})
}

type Delivery struct {
	ID, Event, Repository, HookID, InstallationTarget, UserAgent, BindingProject string
}

func (s *Service) deliverRegistered(ctx context.Context, delivery Delivery, payload []byte) (bool, error) {
	s.githubRoutes.mu.Lock()
	ids := make([]string, 0, len(s.githubRoutes.byRepository[delivery.Repository]))
	for id := range s.githubRoutes.byRepository[delivery.Repository] {
		ids = append(ids, id)
	}
	unavailable := len(s.githubRoutes.unavailable) != 0
	s.githubRoutes.mu.Unlock()
	if unavailable {
		return false, errors.New("GitHub repository routing unavailable")
	}
	duplicate := len(ids) != 0
	var failure error
	for _, id := range ids {
		org, err := s.deps.ReadyOrganization(ctx, id)
		if errors.Is(err, s.deps.OrganizationNotFound) {
			continue
		}
		if err == nil {
			var repeated bool
			repeated, err = s.send(ctx, org, delivery, payload, false)
			duplicate = duplicate && repeated
		}
		failure = errors.Join(failure, err)
	}
	return duplicate, failure
}

func (s *Service) Deliver(ctx context.Context, org tenant.Organization, delivery Delivery, payload []byte) error {
	_, err := s.send(ctx, org, delivery, payload, true)
	return err
}

func (s *Service) send(ctx context.Context, org tenant.Organization, delivery Delivery, payload []byte, deferred bool) (bool, error) {
	query := url.Values{"delivery_id": {delivery.ID}, "event_type": {delivery.Event}}
	if deferred {
		query.Set("binding_project", delivery.BindingProject)
		query.Set("defer_processing", "1")
	} else {
		query.Set("hook_id", delivery.HookID)
		query.Set("installation_target", delivery.InstallationTarget)
		query.Set("user_agent", delivery.UserAgent)
	}
	claims, err := s.deps.Claims(org, cloudassert.KindService, http.MethodPost, "/internal/v1/github/webhook?"+query.Encode(), payload)
	if err != nil {
		return false, err
	}
	status, raw, err := s.deps.SignedCall(ctx, org, claims, payload, "")
	if deferred && status == http.StatusNoContent && err == nil {
		return false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("GitHub webhook transport failed for organization %s", org.ID)
	}
	if status != http.StatusAccepted {
		return false, fmt.Errorf("tenant %s GitHub webhook returned %d", org.ID, status)
	}
	var result struct {
		Duplicate bool `json:"duplicate"`
	}
	err = json.Unmarshal(raw, &result)
	return result.Duplicate, err
}

type Deps struct {
	Accept               func(context.Context, Delivery, []byte) (bool, error)
	Secret               []byte
	Logger               *slog.Logger
	List                 func(context.Context) ([]tenant.Organization, error)
	ReadyOrganization    func(context.Context, string) (tenant.Organization, error)
	StartManaged         bool
	StartTenant          func(context.Context, *tenant.Organization) error
	ServiceCall          func(context.Context, tenant.Organization, string, any, func(*cloudassert.Claims)) (int, []byte, error)
	Claims               func(tenant.Organization, string, string, string, []byte) (cloudassert.Claims, error)
	SignedCall           func(context.Context, tenant.Organization, cloudassert.Claims, []byte, string) (int, []byte, error)
	OrganizationNotFound error
}

type Service struct {
	deps         Deps
	githubRoutes githubRoutes
}

func New(deps Deps) *Service { return &Service{deps: deps} }
