package hubserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const flySpritesToken = "fly_sprites_token"

// Secret metadata is the entire public read surface. No ciphertext or value is
// ever part of an API response, native command receipt, or coordinator tool.
type projectSecretStatus struct {
	Kind             string `json:"kind"`
	Present          bool   `json:"present"`
	OrganizationSlug string `json:"organization_slug,omitempty"`
	KeyVersion       int    `json:"key_version,omitempty"`
}

func secretAAD(organization, project, kind string) []byte {
	// Array framing makes tenant, project and kind unambiguous and binds both
	// the value and the wrapped data key to their original row.
	encoded, _ := json.Marshal([3]string{organization, project, kind}) //nolint:errcheck // A fixed array of strings cannot fail JSON encoding.
	return encoded
}

func (s *Service) registerProjectSecretRoutes(e *echo.Echo) {
	read := s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin)
	manage := func(next echo.HandlerFunc) echo.HandlerFunc {
		return read(func(c echo.Context) error {
			scope := nativeRequestScope(c)
			if !canManageProjectSecrets(scope.credential) {
				return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Changing provider secrets requires owner or admin access", hubapi.APIErrorResponse{Code: "forbidden", Message: "Changing provider secrets requires owner or admin access"})
			}
			scope.requireHostedAdmin = true
			c.Set("native_scope", scope)
			return next(c)
		})
	}
	e.GET(hubapi.NativeBase+"/secrets/:kind", s.projectSecretMetadata, read)
	e.PUT(hubapi.NativeBase+"/secrets/:kind", s.setProjectSecret, manage)
	e.DELETE(hubapi.NativeBase+"/secrets/:kind", s.removeProjectSecret, manage)
}

func canManageProjectSecrets(credential hubapi.APICredential) bool {
	if credential.Runner.RunnerID != "" {
		return false
	}
	if credential.Hosted != nil {
		return credential.HostedRole == "owner" || credential.HostedRole == "admin"
	}
	return credential.Scope == hubapi.APIScopeAdmin && !credential.NativeOnly
}

func (s *Service) projectSecretMetadata(c echo.Context) error {
	if c.Param("kind") != flySpritesToken {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	status, err := s.readAccessSecretStatus(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, status)
}

func secretActor(scope nativeScope) string {
	if scope.credential.Hosted != nil {
		if scope.credential.Hosted.SupportActor != "" {
			return scope.credential.Hosted.SupportActor
		}
		return scope.credential.Hosted.Subject
	}
	return scope.credential.ID
}

func (s *Service) auditSecretUse(ctx context.Context, scope nativeScope, version int) error {
	return s.accessWrite(ctx, scope.organization, func(w tenantstore.Writer) error {
		return w.RecordAccessSecretAudit(ctx, s.accessSecretAudit(scope, "use", version))
	})
}

func (s *Service) setProjectSecret(c echo.Context) error {
	if c.Param("kind") != flySpritesToken {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	var request struct {
		hostedIdempotent
		Token string `json:"token"`
	}
	// Decoder errors may include an attacker-controlled field name or token.
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter a Sprites organization token")
	}
	token := []byte(request.Token)
	request.Token = ""
	defer clear(token)
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	if s.config.SecretKeys == nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "The Hub operator must configure a secret master key")
	}
	scope := nativeRequestScope(c)
	return s.setAccessProjectSecret(c, scope, token)
}

func (s *Service) removeProjectSecret(c echo.Context) error {
	if c.Param("kind") != flySpritesToken {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if err := s.removeAccessProjectSecret(c.Request().Context(), nativeRequestScope(c)); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, projectSecretStatus{Kind: flySpritesToken})
}

func resolveSpritesSecret(ctx context.Context, db hubapi.NativeQueryer, scope nativeScope) (nativeScope, string, hubsecrets.Envelope, error) {
	var envelope hubsecrets.Envelope
	var slug string
	var source tracker.ProjectID
	err := db.QueryRowContext(ctx, `SELECT project_id,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version FROM (
 SELECT project_id,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,0 AS rank FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?
 UNION ALL SELECT '',organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,1 FROM organization_secrets WHERE organization_id=? AND kind=?) ORDER BY rank LIMIT 1`, scope.organization, scope.project, flySpritesToken, scope.organization, flySpritesToken).Scan(&source, &slug, &envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	scope.project = source
	return scope, slug, envelope, err
}

func (s *Service) organizationSpritesScope(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		scope, err := s.organizationModelSelectionScope(c)
		if err != nil {
			return s.hostedAPIError(c, err)
		}
		if c.Request().Method != http.MethodGet && !canManageProjectSecrets(scope.credential) {
			return s.nativeAPIError(c, &hubapi.NativeError{Code: "forbidden", Message: "Changing Sprites settings requires owner or admin access", Status: http.StatusForbidden, PublicMessage: true})
		}
		c.Set("native_scope", scope)
		return next(c)
	}
}
