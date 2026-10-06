package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/connector/github"
)

type githubRoutes struct {
	mu             sync.Mutex
	byRepository   map[string]map[string]struct{}
	byOrganization map[string][]string
	unavailable    map[string]bool
}

func (s *Service) initializeGitHubRoutes(ctx context.Context) error {
	if len(s.config.GitHubWebhookSecret) == 0 {
		return nil
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		return err
	}
	for _, organization := range organizations {
		if organization.State != "ready" {
			continue
		}
		if organization.Managed && s.config.Allocation != nil {
			err = s.runStep(ctx, &organization, "tenant_start")
		} else {
			err = s.refreshGitHubRoutes(ctx, organization)
		}
		if err != nil {
			s.githubRoutes.mu.Lock()
			if s.githubRoutes.unavailable == nil {
				s.githubRoutes.unavailable = make(map[string]bool)
			}
			s.githubRoutes.unavailable[organization.ID] = true
			s.githubRoutes.mu.Unlock()
			s.config.Logger.Warn("read tenant GitHub repository bindings", "organization", organization.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) refreshGitHubRoutes(ctx context.Context, organization Organization) error {
	if len(s.config.GitHubWebhookSecret) == 0 {
		return nil
	}
	routes := &s.githubRoutes
	routes.mu.Lock()
	defer routes.mu.Unlock()
	if routes.byRepository == nil {
		routes.byRepository = make(map[string]map[string]struct{})
		routes.byOrganization = make(map[string][]string)
	}
	if routes.unavailable == nil {
		routes.unavailable = make(map[string]bool)
	}
	routes.unavailable[organization.ID] = true
	status, raw, err := s.serviceCall(ctx, organization, "/internal/v1/github/repositories", struct{}{}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("tenant GitHub repository bindings returned %d", status)
	}
	var repositories []string
	if err := json.Unmarshal(raw, &repositories); err != nil {
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

func (s *Service) githubWebhook(c echo.Context) error {
	fail := func(status int, code, message string) error {
		return c.JSON(status, map[string]string{"code": code, "message": message})
	}
	if len(s.config.GitHubWebhookSecret) == 0 {
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
	if !github.ValidWebhookSignature(s.config.GitHubWebhookSecret, payload, request.Header.Get("X-Hub-Signature-256")) {
		return fail(http.StatusUnauthorized, "invalid_signature", "GitHub webhook signature is invalid")
	}
	deliveryID := strings.TrimSpace(request.Header.Get("X-GitHub-Delivery"))
	eventType := strings.TrimSpace(request.Header.Get("X-GitHub-Event"))
	if deliveryID == "" || eventType == "" {
		return fail(http.StatusBadRequest, "invalid_headers", "GitHub webhook delivery and event headers are required")
	}
	var metadata struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return fail(http.StatusBadRequest, "invalid_payload", "GitHub webhook payload is invalid")
	}
	repository := strings.ToLower(metadata.Repository.FullName)
	s.githubRoutes.mu.Lock()
	ids := make([]string, 0, len(s.githubRoutes.byRepository[repository]))
	for id := range s.githubRoutes.byRepository[repository] {
		ids = append(ids, id)
	}
	unavailable := len(s.githubRoutes.unavailable) != 0
	s.githubRoutes.mu.Unlock()
	if unavailable {
		return fail(http.StatusServiceUnavailable, "tenant_unavailable", "GitHub repository routing is temporarily unavailable")
	}
	query := url.Values{
		"delivery_id": {deliveryID}, "event_type": {eventType},
		"hook_id":             {request.Header.Get("X-GitHub-Hook-ID")},
		"installation_target": {request.Header.Get("X-GitHub-Hook-Installation-Target-ID")},
		"user_agent":          {request.Header.Get("User-Agent")},
	}
	path := "/internal/v1/github/webhook?" + query.Encode()
	duplicate := len(ids) != 0
	var deliveryErr error
	for _, id := range ids {
		organization, err := s.readyOrganization(request.Context(), id)
		if errors.Is(err, ErrOrganizationNotFound) {
			continue
		}
		if err != nil {
			deliveryErr = errors.Join(deliveryErr, err)
			continue
		}
		claims, err := s.claims(organization, cloudassert.KindService, http.MethodPost, path, payload)
		if err != nil {
			deliveryErr = errors.Join(deliveryErr, err)
			continue
		}
		status, raw, err := s.signedCall(request.Context(), organization, claims, payload, "")
		if err != nil || status != http.StatusAccepted {
			deliveryErr = errors.Join(deliveryErr, fmt.Errorf("tenant %s GitHub webhook returned %d", id, status), err)
			continue
		}
		var result struct {
			Duplicate bool `json:"duplicate"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			deliveryErr = errors.Join(deliveryErr, err)
			continue
		}
		duplicate = duplicate && result.Duplicate
	}
	if deliveryErr != nil {
		s.config.Logger.Warn("deliver hosted GitHub webhook", "delivery_id", deliveryID, "error", deliveryErr)
		return fail(http.StatusServiceUnavailable, "tenant_unavailable", "GitHub webhook delivery could not reach every matching organization")
	}
	return c.JSON(http.StatusAccepted, struct {
		DeliveryID string `json:"delivery_id"`
		Duplicate  bool   `json:"duplicate"`
	}{deliveryID, duplicate})
}
