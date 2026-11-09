package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HostedSecretStatus struct {
	OrganizationID tracker.OrganizationID `json:"organization_id"`
	Counts         []SecretVersionCount   `json:"counts"`
	Problems       []SecretRowProblem     `json:"problems"`
	NextProjectID  tracker.ProjectID      `json:"next_project_id,omitempty"`
	NextKind       string                 `json:"next_kind,omitempty"`
}

func (s *Service) hostedSecretStatus(c echo.Context) error {
	scope := nativeRequestScope(c)
	limit := 200
	if raw := c.QueryParam("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return s.nativeAPIError(c, nativePublicInvalid("Invalid secret status page limit"))
		}
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	status := HostedSecretStatus{OrganizationID: scope.organization, Counts: []SecretVersionCount{}, Problems: []SecretRowProblem{}}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		rows, err := r.AccessSecrets(ctx, tracker.ProjectID(c.QueryParam("after_project")), c.QueryParam("after_kind"), limit)
		if err != nil {
			return err
		}
		counts := make(map[SecretVersionCount]int)
		for _, row := range rows {
			table := "organization_secrets"
			if row.ProjectID != "" {
				table = "project_secrets"
			}
			group := SecretVersionCount{Table: table, Kind: row.Kind, MasterKeyVersion: row.Envelope.Version}
			index, ok := counts[group]
			if !ok {
				index = len(status.Counts)
				counts[group] = index
				status.Counts = append(status.Counts, group)
			}
			status.Counts[index].Count++
			if problem := secretStatusProblem(s.config.SecretKeys, row); problem != "" {
				status.Problems = append(status.Problems, SecretRowProblem{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), Kind: row.Kind, MasterKeyVersion: row.Envelope.Version, UpdatedAt: formatHubTime(row.UpdatedAt), Problem: problem})
			}
		}
		if len(rows) == limit {
			last := rows[len(rows)-1]
			status.NextProjectID, status.NextKind = last.ProjectID, last.Kind
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, status)
}

func (s *Service) hostedSecretStatusAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.authenticateAPIRequest(c)
		if err != nil {
			if status >= http.StatusInternalServerError {
				return s.nativeAPIError(c, err)
			}
			return s.refusedAPIError(c, status, "unauthorized", "Valid scoped API token is required", nil)
		}
		if credential.Scope != apiScopeAdmin || credential.Hosted != nil && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
			return s.nativeAPIError(c, nativeNotFound())
		}
		c.Set("native_scope", nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential, requireHostedAdmin: true})
		c.Response().Header().Set("Cache-Control", "no-store")
		return next(c)
	}
}

func secretStatusProblem(keys *hubsecrets.Keyring, row accessstore.Secret) string {
	switch err := keys.Check(row.Envelope, accessstore.SecretAAD(row.OrganizationID, row.ProjectID, row.Kind)); {
	case errors.Is(err, hubsecrets.ErrUnavailable):
		return "missing_key"
	case err != nil:
		return "unwrap_failed"
	case row.Envelope.Version != keys.Version():
		return "not_active_version"
	default:
		return ""
	}
}
