package hubserver

import (
	"context"
	"database/sql"
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
	if s.database.db == nil && s.config.TenantRepository != nil {
		return s.hostedGitHubRepositoriesPostgres(ctx, c)
	}
	rows, err := s.database.reader.QueryContext(ctx, `
SELECT lower(r.github_owner || '/' || r.github_name) FROM projects p
JOIN repositories r ON r.id = p.repository_id WHERE p.deleted_at IS NULL AND p.profile = 'native' AND p.organization_id = ?
UNION
SELECT lower(checkout_repository) FROM projects
WHERE deleted_at IS NULL AND profile = 'native' AND organization_id = ? AND checkout_repository != ''`, s.config.Hosted.OrganizationID, s.config.Hosted.OrganizationID)
	if err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings are unavailable", err)
	}
	defer rows.Close()
	repositories := []string{}
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return err
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, repositories)
}

func (s *Service) hostedGitHubRepositoriesPostgres(ctx context.Context, c echo.Context) error {
	const pageSize = corestore.MaxPageSize
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	var page []projectstore.GitHubRepositoryBinding
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		pageReader, ok := reader.(projectstore.GitHubRepositoryBindingsPageReader)
		if !ok {
			return errors.New("typed GitHub repository binding reads are unavailable")
		}
		var err error
		page, err = pageReader.GitHubRepositoryBindings(ctx, "", pageSize)
		return err
	})
	if err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings are unavailable", err)
	}
	if len(page) > pageSize {
		return s.internalAPIError(c, "tenant_unavailable", "Repository bindings exceed the supported project limit", corestore.ErrInvalidPage)
	}
	repositories := make(map[string]struct{})
	for _, binding := range page {
		for _, repository := range []string{binding.Repository, binding.CheckoutRepository} {
			if repository = strings.ToLower(repository); repository != "" {
				repositories[repository] = struct{}{}
			}
		}
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
	var received sql.NullString
	if err := s.database.reader.QueryRowContext(ctx, "SELECT max(last_received_at) FROM github_webhook_inbox").Scan(&received); err != nil {
		return s.internalAPIError(c, "tenant_unavailable", "GitHub webhook receipt is unavailable", err)
	}
	var result struct {
		ReceivedAt *time.Time `json:"received_at"`
	}
	if received.Valid {
		value, err := time.Parse(time.RFC3339Nano, received.String)
		if err != nil {
			return err
		}
		result.ReceivedAt = &value
	}
	return c.JSON(http.StatusOK, result)
}
