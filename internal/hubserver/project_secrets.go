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
	envelope                    hubsecrets.Envelope
}

func secretAAD(organization, project, kind string) []byte {
	// Array framing makes tenant, project and kind unambiguous and binds both
	// the value and the wrapped data key to their original row.
	encoded, _ := json.Marshal([3]string{organization, project, kind}) //nolint:errcheck // A fixed array of strings cannot fail JSON encoding.
	return encoded
}

func readSecretRows(ctx context.Context, db nativeQueryer) ([]projectSecretRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT organization_id, project_id, kind, ciphertext, nonce, wrapped_data_key, master_key_version FROM project_secrets UNION ALL SELECT organization_id, '', kind, ciphertext, nonce, wrapped_data_key, master_key_version FROM organization_secrets ORDER BY organization_id, project_id, kind`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []projectSecretRow
	for rows.Next() {
		var r projectSecretRow
		if err := rows.Scan(&r.organization, &r.project, &r.kind, &r.envelope.Ciphertext, &r.envelope.Nonce, &r.envelope.WrappedKey, &r.envelope.Version); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (d *database) checkProjectSecretKeys(ctx context.Context, keys *hubsecrets.Keyring) error {
	rows, err := readSecretRows(ctx, d.db)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := keys.Check(row.envelope, secretAAD(row.organization, row.project, row.kind)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) registerProjectSecretRoutes(e *echo.Echo) {
	read := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	manage := func(next echo.HandlerFunc) echo.HandlerFunc {
		return read(func(c echo.Context) error {
			scope := nativeRequestScope(c)
			if !canManageProjectSecrets(scope.credential) {
				return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "Changing provider secrets requires owner or admin access"})
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
	scope := nativeRequestScope(c)
	status, err := readSecretStatus(c.Request().Context(), s.database.db, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, status)
}

func readSecretStatus(ctx context.Context, db nativeQueryer, scope nativeScope) (projectSecretStatus, error) {
	result := projectSecretStatus{Kind: flySpritesToken}
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
	return secretAudit(ctx, s.database.db, string(scope.organization), string(scope.project), secretActor(scope), flySpritesToken, "use", version, formatHubTime(s.config.now()))
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
	ctx := c.Request().Context()
	// Recheck current membership before making any provider call; the same
	// authority is rechecked again in the write transaction after validation.
	if err := s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		return secretAudit(ctx, tx, string(scope.organization), string(scope.project), secretActor(scope), flySpritesToken, "use", s.config.SecretKeys.Version(), formatHubTime(s.config.now()))
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	slug, err := validateSpritesToken(ctx, s.config.SpritesHTTPClient, token)
	if err != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, err.Error())
	}
	envelope, err := s.config.SecretKeys.Seal(token, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	clear(token)
	var result projectSecretStatus
	err = s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		old, err := readSecretStatus(ctx, tx, scope)
		if err != nil {
			return err
		}
		event := "set"
		if old.Present {
			event = "replace"
		}
		now := formatHubTime(s.config.now())
		_, err = tx.ExecContext(ctx, `INSERT INTO project_secrets(organization_id, project_id, kind, organization_slug, ciphertext, nonce, wrapped_data_key, master_key_version, updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(organization_id, project_id, kind) DO UPDATE SET organization_slug=excluded.organization_slug, ciphertext=excluded.ciphertext, nonce=excluded.nonce, wrapped_data_key=excluded.wrapped_data_key, master_key_version=excluded.master_key_version, updated_at=excluded.updated_at`, scope.organization, scope.project, flySpritesToken, slug, envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, now)
		if err != nil {
			return err
		}
		if err := secretAudit(ctx, tx, string(scope.organization), string(scope.project), secretActor(scope), flySpritesToken, event, envelope.Version, now); err != nil {
			return err
		}
		result, err = readSecretStatus(ctx, tx, scope)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) removeProjectSecret(c echo.Context) error {
	if c.Param("kind") != flySpritesToken {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope, ctx := nativeRequestScope(c), c.Request().Context()
	err := s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		return removeProjectSecretInTx(ctx, tx, scope, s.config.now())
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, projectSecretStatus{Kind: flySpritesToken})
}

func (s *Service) secretMutation(ctx context.Context, scope nativeScope, operation func(*sql.Tx) error) (err error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollback := tx.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return err
	}
	if err := operation(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateProjectSecrets is an offline operation protected by the database's
// existing ownership lock. Data keys are rewrapped atomically; values stay sealed.
func RotateProjectSecrets(ctx context.Context, cfg Config, actor string) (count int, err error) {
	if cfg.SecretKeys == nil || actor == "" {
		return 0, hubsecrets.ErrUnavailable
	}
	cfg = cfg.normalized()
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if rollback := tx.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	rows, err := readSecretRows(ctx, tx)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if row.envelope.Version == cfg.SecretKeys.Version() {
			if err := cfg.SecretKeys.Check(row.envelope, secretAAD(row.organization, row.project, row.kind)); err != nil {
				return 0, err
			}
			continue
		}
		wrapped, err := cfg.SecretKeys.Rewrap(row.envelope, secretAAD(row.organization, row.project, row.kind))
		if err != nil {
			return 0, err
		}
		if row.project == "" {
			_, err = tx.ExecContext(ctx, `UPDATE organization_secrets SET wrapped_data_key=?,master_key_version=? WHERE organization_id=? AND kind=?`, wrapped.WrappedKey, wrapped.Version, row.organization, row.kind)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE project_secrets SET wrapped_data_key=?, master_key_version=? WHERE organization_id=? AND project_id=? AND kind=?`, wrapped.WrappedKey, wrapped.Version, row.organization, row.project, row.kind)
		}
		if err != nil {
			return 0, err
		}
		if err := secretAudit(ctx, tx, row.organization, row.project, actor, row.kind, "rotate", wrapped.Version, formatHubTime(cfg.now())); err != nil {
			return 0, err
		}
		count++
	}
	return count, tx.Commit()
}

func removeProjectSecretInTx(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	old, err := readSecretStatus(ctx, tx, scope)
	if err != nil || !old.Present {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, flySpritesToken); err != nil {
		return err
	}
	return secretAudit(ctx, tx, string(scope.organization), string(scope.project), secretActor(scope), flySpritesToken, "remove", old.KeyVersion, formatHubTime(now))
}
