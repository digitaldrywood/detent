package githubhosted

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) Repositories(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	return s.repositories(ctx, c)
}

func (s *Service) repositories(ctx context.Context, c echo.Context) error {
	if s.deps.Repository == nil {
		return s.deps.Error(c, "tenant_unavailable", "Repository bindings are unavailable", errors.New("typed GitHub repository binding reads are unavailable"))
	}
	repository, ok := s.deps.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return s.deps.Error(c, "tenant_unavailable", "Repository bindings are unavailable", errors.New("typed GitHub repository snapshots are unavailable"))
	}
	const pageSize = corestore.MaxPageSize
	organization := s.deps.Organization
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
		return s.deps.Error(c, "tenant_unavailable", "Repository bindings are unavailable", err)
	}
	result := make([]string, 0, len(repositories))
	for repository := range repositories {
		result = append(result, repository)
	}
	sort.Strings(result)
	return c.JSON(http.StatusOK, result)
}

func (s *Service) Receipt(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	var result struct {
		ReceivedAt *time.Time `json:"received_at"`
	}
	if s.deps.Repository == nil {
		return s.deps.Error(c, "tenant_unavailable", "GitHub webhook receipt is unavailable", s.deps.Unavailable)
	}
	err := s.deps.Repository.ReadTenant(ctx, s.deps.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		owner, ok := reader.(projectstore.GitHubIntakeReader)
		if !ok {
			return s.deps.Unavailable
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
		return s.deps.Error(c, "tenant_unavailable", "GitHub webhook receipt is unavailable", err)
	}
	return c.JSON(http.StatusOK, result)
}
