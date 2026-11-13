package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
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

type projectSecretRow struct {
	organization, project, kind string
	table, updatedAt            string
	envelope                    hubsecrets.Envelope
}

func secretAAD(organization, project, kind string) []byte {
	// Array framing makes tenant, project and kind unambiguous and binds both
	// the value and the wrapped data key to their original row.
	encoded, _ := json.Marshal([3]string{organization, project, kind}) //nolint:errcheck // A fixed array of strings cannot fail JSON encoding.
	return encoded
}

func readSecretRows(ctx context.Context, db nativeQueryer) ([]projectSecretRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT organization_id, project_id, kind, ciphertext, nonce, wrapped_data_key, master_key_version, 'project_secrets', updated_at FROM project_secrets UNION ALL SELECT organization_id, '', kind, ciphertext, nonce, wrapped_data_key, master_key_version, 'organization_secrets', updated_at FROM organization_secrets ORDER BY organization_id, project_id, kind`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []projectSecretRow
	for rows.Next() {
		var r projectSecretRow
		if err := rows.Scan(&r.organization, &r.project, &r.kind, &r.envelope.Ciphertext, &r.envelope.Nonce, &r.envelope.WrappedKey, &r.envelope.Version, &r.table, &r.updatedAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Service) registerProjectSecretRoutes(e *echo.Echo) {
	read := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	manage := func(next echo.HandlerFunc) echo.HandlerFunc {
		return read(func(c echo.Context) error {
			scope := nativeRequestScope(c)
			if !canManageProjectSecrets(scope.credential) {
				return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Changing provider secrets requires owner or admin access", apiErrorResponse{Code: "forbidden", Message: "Changing provider secrets requires owner or admin access"})
			}
			scope.requireHostedAdmin = true
			c.Set("native_scope", scope)
			return next(c)
		})
	}
	e.GET(nativeBase+"/secrets/:kind", s.projectSecretMetadata, read)
	e.PUT(nativeBase+"/secrets/:kind", s.setProjectSecret, manage)
	e.DELETE(nativeBase+"/secrets/:kind", s.removeProjectSecret, manage)
}

func canManageProjectSecrets(credential apiCredential) bool {
	if credential.Runner.RunnerID != "" {
		return false
	}
	if credential.Hosted != nil {
		return credential.HostedRole == "owner" || credential.HostedRole == "admin"
	}
	return credential.Scope == apiScopeAdmin && !credential.NativeOnly
}

func (s *Service) projectSecretMetadata(c echo.Context) error {
	if c.Param("kind") != flySpritesToken {
		return s.nativeAPIError(c, nativeNotFound())
	}
	status, err := s.readAccessSecretStatus(c.Request().Context(), nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, status)
}

func readSecretStatus(ctx context.Context, db nativeQueryer, scope nativeScope) (projectSecretStatus, error) {
	result := projectSecretStatus{Kind: flySpritesToken}
	if scope.project == "" {
		err := db.QueryRowContext(ctx, `SELECT organization_slug,master_key_version FROM organization_secrets WHERE organization_id=? AND kind=?`, scope.organization, flySpritesToken).Scan(&result.OrganizationSlug, &result.KeyVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return result, nil
		}
		result.Present = err == nil
		return result, err
	}
	err := db.QueryRowContext(ctx, `SELECT organization_slug, master_key_version FROM project_secrets WHERE organization_id = ? AND project_id = ? AND kind = ?`, scope.organization, scope.project, flySpritesToken).Scan(&result.OrganizationSlug, &result.KeyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	result.Present = err == nil
	return result, err
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

func secretAudit(ctx context.Context, exec hostedExecer, organization, project, actor, kind, event string, version int, at string) error {
	if project == "" {
		_, err := exec.ExecContext(ctx, `INSERT INTO organization_secret_audit(organization_id,actor,kind,key_version,event,recorded_at) VALUES(?,?,?,?,?,?)`, organization, actor, kind, version, event, at)
		return err
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO project_secret_audit(organization_id, project_id, actor, kind, key_version, event, recorded_at) VALUES(?,?,?,?,?,?,?)`, organization, project, actor, kind, version, event, at)
	return err
}

func (s *Service) auditSecretUse(ctx context.Context, scope nativeScope, version int) error {
	return s.accessWrite(ctx, scope.organization, func(w tenantstore.Writer) error {
		return w.RecordAccessSecretAudit(ctx, s.accessSecretAudit(scope, "use", version))
	})
}

func (s *Service) setProjectSecret(c echo.Context) error {
	if c.Param("kind") != flySpritesToken {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var request struct {
		hostedIdempotent
		Token string `json:"token"`
	}
	// Decoder errors may include an attacker-controlled field name or token.
	if err := decodeAPIJSON(c, &request); err != nil {
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
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := s.removeAccessProjectSecret(c.Request().Context(), nativeRequestScope(c)); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, projectSecretStatus{Kind: flySpritesToken})
}

func removeProjectSecretInTx(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	old, err := readSecretStatus(ctx, tx, scope)
	if err != nil || !old.Present {
		return err
	}
	if scope.project == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM organization_secrets WHERE organization_id=? AND kind=?`, scope.organization, flySpritesToken)
		if err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, flySpritesToken); err != nil {
		return err
	}
	return secretAudit(ctx, tx, string(scope.organization), string(scope.project), secretActor(scope), flySpritesToken, "remove", old.KeyVersion, formatHubTime(now))
}

func resolveSpritesSecret(ctx context.Context, db nativeQueryer, scope nativeScope) (nativeScope, string, hubsecrets.Envelope, error) {
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
			return s.nativeAPIError(c, &nativeError{Code: "forbidden", Message: "Changing Sprites settings requires owner or admin access", status: http.StatusForbidden, publicMessage: true})
		}
		c.Set("native_scope", scope)
		return next(c)
	}
}
