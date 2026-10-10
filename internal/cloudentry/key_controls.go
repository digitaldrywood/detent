package cloudentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

type keyProject struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CanWrite bool   `json:"can_write"`
}

type keyReach struct {
	OrganizationID string       `json:"organization_id"`
	Name           string       `json:"name"`
	Role           string       `json:"role"`
	Status         string       `json:"status"`
	Blocked        bool         `json:"blocked"`
	Projects       []keyProject `json:"projects"`
	LastUsedAt     *string      `json:"last_used_at"`
}

type keyView struct {
	accessKey
	EffectiveReach []keyReach `json:"effective_reach"`
	LastUsedAt     *string    `json:"last_used_at"`
	ReadOnly       bool       `json:"read_only,omitempty"`
}

func (s *Service) personalKeyPolicy(ctx context.Context, organization string) (string, error) {
	var policy string
	err := s.auth.store.db.QueryRowContext(ctx, "SELECT personal_keys FROM organization_key_policy WHERE organization_id=?", organization).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return "allowed", nil
	}
	return policy, err
}

func (s *Service) ownerKeyReach(ctx context.Context, owner, email string, org Organization) (keyReach, error) {
	reach := keyReach{OrganizationID: org.ID, Name: org.Name, Status: "allowed", Projects: []keyProject{}}
	member, err := s.keyMembership(ctx, owner, org)
	if err != nil {
		return reach, err
	}
	status, raw, err := s.serviceCall(ctx, org, "/internal/v1/keys/context", struct{}{}, func(claims *cloudassert.Claims) {
		claims.Subject, claims.Email, claims.Role = owner, email, member.Role.Slug
	})
	if err != nil {
		return reach, err
	}
	if status == http.StatusForbidden {
		return reach, &apikey.Refusal{Code: "membership_denied", Message: "Current organization membership is required"}
	}
	if status != http.StatusOK {
		return reach, errors.New("key owner context unavailable")
	}
	var result struct {
		Role     string       `json:"role"`
		Projects []keyProject `json:"projects"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return reach, err
	}
	reach.Role, reach.Projects = result.Role, result.Projects
	return reach, nil
}

func (s *Service) accountKeyContext(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return keyError(c, err)
	}
	result := []keyReach{}
	for _, choice := range choices {
		org, err := s.readyOrganization(c.Request().Context(), choice.ID)
		if err != nil {
			return keyError(c, err)
		}
		reach, err := s.ownerKeyReach(c.Request().Context(), session.Subject, session.Email, org)
		if err != nil {
			var refusal *apikey.Refusal
			if errors.As(err, &refusal) {
				continue
			}
			return keyError(c, err)
		}
		result = append(result, reach)
	}
	return c.JSON(http.StatusOK, struct {
		Organizations []keyReach `json:"organizations"`
		MCPEndpoint   string     `json:"mcp_endpoint"`
	}{result, s.config.PublicURL + "/mcp"})
}

func (s *Service) describeKey(ctx context.Context, key accessKey, organizations []Organization) (keyView, error) {
	view := keyView{accessKey: key, EffectiveReach: []keyReach{}}
	var last sql.NullString
	if err := s.auth.store.db.QueryRowContext(ctx, "SELECT MAX(last_used_at) FROM access_key_organizations WHERE key_id=?", key.ID).Scan(&last); err != nil {
		return view, err
	}
	if last.Valid {
		view.LastUsedAt = &last.String
	}
	for _, org := range organizations {
		projects, reaches := key.projectContext(org.ID)
		if !reaches {
			continue
		}
		reach, err := s.ownerKeyReach(ctx, key.Owner, key.OwnerEmail, org)
		if err != nil {
			var refusal *apikey.Refusal
			if errors.As(err, &refusal) {
				continue
			}
			return view, err
		}
		var blocked, approved bool
		var blockedJSON string
		var used sql.NullString
		err = s.auth.store.db.QueryRowContext(ctx, "SELECT blocked,approved_at IS NOT NULL,blocked_projects_json,last_used_at FROM access_key_organizations WHERE key_id=? AND organization_id=?", key.ID, org.ID).Scan(&blocked, &approved, &blockedJSON, &used)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return view, err
		}
		reach.Blocked = blocked
		exclusions := []string{}
		if blockedJSON != "" {
			if err := json.Unmarshal([]byte(blockedJSON), &exclusions); err != nil {
				return view, err
			}
		}
		policy, err := s.personalKeyPolicy(ctx, org.ID)
		if err != nil {
			return view, err
		}
		switch {
		case key.RevokedAt != nil || key.ExpiresAt != nil && !key.ExpiresAt.After(s.config.now()):
			reach.Status = "inactive"
		case blocked || policy == "blocked":
			reach.Status = "blocked"
		case policy == "approval" && !approved:
			reach.Status = "pending"
		}
		reach.Projects = slices.DeleteFunc(reach.Projects, func(project keyProject) bool {
			return !projects.Allows(project.ID) || slices.Contains(exclusions, project.ID)
		})
		for i := range reach.Projects {
			reach.Projects[i].CanWrite = reach.Projects[i].CanWrite && key.Permission != apikey.ScopeRead
		}
		if used.Valid {
			reach.LastUsedAt = &used.String
		}
		view.EffectiveReach = append(view.EffectiveReach, reach)
	}
	return view, nil
}

func (s *Service) rotateAccountKey(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	key, err := s.auth.accessKey(c.Request().Context(), "id", c.Param("key"))
	if err != nil {
		return keyError(c, err)
	}
	if key.Owner != session.Subject || key.Kind != "personal" {
		return c.NoContent(http.StatusNotFound)
	}
	if key.RevokedAt != nil || key.ExpiresAt != nil && !key.ExpiresAt.After(s.config.now()) {
		return c.NoContent(http.StatusConflict)
	}
	key.Token, err = s.config.generateToken()
	if err != nil {
		return keyError(c, err)
	}
	result, err := s.auth.store.db.ExecContext(c.Request().Context(), "UPDATE access_keys SET token_hash=? WHERE id=? AND token_hash=? AND revoked_at IS NULL", apikey.HashToken(key.Token), key.ID, key.Hash)
	if err != nil {
		return keyError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return keyError(c, err)
	}
	if count != 1 {
		return c.NoContent(http.StatusConflict)
	}
	return c.JSON(http.StatusCreated, key)
}

func (s *Service) approveOrganizationKey(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	org, err := s.keyAdmin(c, session)
	if err != nil {
		return keyError(c, err)
	}
	key, err := s.auth.accessKey(c.Request().Context(), "id", c.Param("key"))
	if err != nil {
		return keyError(c, err)
	}
	if _, reaches := key.projectContext(org.ID); !reaches || key.Kind != "personal" || key.RevokedAt != nil || key.ExpiresAt != nil && !key.ExpiresAt.After(s.config.now()) {
		return c.NoContent(http.StatusNotFound)
	}
	if _, err := s.ownerKeyReach(c.Request().Context(), key.Owner, key.OwnerEmail, org); err != nil {
		return keyError(c, err)
	}
	result, err := s.auth.store.db.ExecContext(c.Request().Context(), "INSERT INTO access_key_organizations(key_id,organization_id,approved_at) VALUES(?,?,?) ON CONFLICT(key_id,organization_id) DO UPDATE SET approved_at=excluded.approved_at WHERE access_key_organizations.blocked=0", key.ID, org.ID, formatTime(s.config.now()))
	if err != nil {
		return keyError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return keyError(c, err)
	}
	if count != 1 {
		return c.NoContent(http.StatusConflict)
	}
	if err := s.keyAudit(c.Request().Context(), org, key.ID, session.Subject, "personal_key_approved"); err != nil {
		return keyError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
