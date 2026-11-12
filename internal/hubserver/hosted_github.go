package hubserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedGitHubRepositories(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	return s.hostedGitHubRepositoriesPostgres(ctx, c)
}

func (s *Service) hostedGitHubRepositoriesPostgres(ctx context.Context, c echo.Context) error {
	if s.config.TenantRepository == nil {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings are unavailable", errors.New("typed GitHub repository binding reads are unavailable"))
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings are unavailable", errors.New("typed GitHub repository snapshots are unavailable"))
	}
	const pageSize = corestore.MaxPageSize
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	repositories := make(map[string]struct{})
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		pageReader, ok := reader.(projectstore.GitHubRepositoryBindingsPageReader)
		if !ok {
			return errors.New("typed GitHub repository binding reads are unavailable")
		}
		var after tracker.ProjectID
		for {
			page, err := pageReader.GitHubRepositoryBindings(ctx, after, pageSize)
			if err != nil {
				return err
			}
			more := len(page) > pageSize
			if more {
				page = page[:pageSize]
			}
			for _, binding := range page {
				for _, repository := range []string{binding.Repository, binding.CheckoutRepository} {
					if repository = strings.ToLower(repository); repository != "" {
						repositories[repository] = struct{}{}
					}
				}
			}
			if !more {
				return nil
			}
			after = page[len(page)-1].ProjectID
		}
	})
	if err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings are unavailable", err)
	}
	result := make([]string, 0, len(repositories))
	for repository := range repositories {
		result = append(result, repository)
	}
	sort.Strings(result)
	return c.JSON(http.StatusOK, result)
}

func (s *Service) hostedGitHubReceipt(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	var result struct {
		ReceivedAt *time.Time `json:"received_at"`
	}
	if s.config.TenantRepository == nil {
		return s.internalAPIError(c, "tenant_unavailable", "GitHub webhook receipt is unavailable", ErrPostgresFeatureUnavailable)
	}
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		owner, ok := reader.(projectstore.GitHubIntakeReader)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		received, err := owner.LatestGitHubWebhookReceipt(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		received = received.UTC()
		result.ReceivedAt = &received
		return nil
	})
	if err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "GitHub webhook receipt is unavailable", err)
	}
	return c.JSON(http.StatusOK, result)
}
