package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runnerOperationAllowed(c echo.Context, operations []string) bool {
	path := c.Path()
	if path == runnerBase+"/:runner" && c.Request().Method == http.MethodGet || path == runnerBase+"/:runner/renew" || path == runnerBase+"/:runner/rotate" {
		return true
	}
	operation := ""
	switch {
	case path == "/api/v2/capabilities":
		operation = runnerauth.Read
	case !strings.HasPrefix(path, nativeBase):
		return false
	case c.Request().Method == http.MethodGet:
		operation = runnerauth.Read
	case path == nativeBase+"/claims", path == nativeBase+"/leases/:lease/renew", path == nativeBase+"/leases/:lease/release":
		operation = runnerauth.Claim
	case path == nativeBase+"/machines/register", path == nativeBase+"/machines/:machine/heartbeat":
		operation = runnerauth.Heartbeat
	case path == nativeBase+"/work-items/:item/events":
		operation = runnerauth.Events
	case strings.HasPrefix(path, nativeBase+"/work-items"):
		operation = runnerauth.Collaborate
	}
	return operation != "" && slices.Contains(operations, operation)
}

func authenticatedRunner(c echo.Context) (apiCredential, error) {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Runner.RunnerID == "" || credential.Runner.RunnerID != c.Param("runner") || string(credential.Runner.OrganizationID) != c.Param("organization") {
		return apiCredential{}, nativeNotFound()
	}
	return credential, nil
}

func readRunnerProjects(ctx context.Context, query nativeQueryer, tokenID string) ([]tracker.ProjectID, error) {
	rows, err := query.QueryContext(ctx, "SELECT project_id FROM token_grants WHERE token_id = ? ORDER BY project_id", tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []tracker.ProjectID{}
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		projects = append(projects, id)
	}
	return projects, rows.Err()
}

func (s *Service) getRunnerIdentity(c echo.Context) error {
	credential, err := authenticatedRunner(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential.Runner.ProjectIDs, err = readRunnerProjects(c.Request().Context(), s.database.db, credential.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, credential.Runner)
}

func (s *Service) renewRunnerIdentity(c echo.Context) error {
	var request struct{}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.changeRunnerCredential(c, "")
}

func (s *Service) rotateRunnerIdentity(c echo.Context) error {
	var request runnerauth.Rotation
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if !runnerauth.ValidCredential(request.Credential) {
		return s.nativeAPIError(c, nativeInvalid("A host-generated replacement credential is required"))
	}
	return s.changeRunnerCredential(c, request.Credential)
}

func (s *Service) changeRunnerCredential(c echo.Context, replacement string) error {
	credential, err := authenticatedRunner(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	token, err := apiBearerToken(c)
	if err != nil {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	if replacement == token {
		return s.nativeAPIError(c, nativeInvalid("Rotation requires a different credential"))
	}
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		var created, expires, hash string
		var revoked sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT created_at, expires_at, token_hash, revoked_at FROM api_tokens WHERE id = ?", credential.ID).Scan(&created, &expires, &hash, &revoked); err != nil {
			return nil, err
		}
		if revoked.Valid || hash != apikey.HashToken(token) || !runnerTimeValid(now, created, expires) {
			return nil, runnerUnauthorized()
		}
		kind := "renewed"
		if replacement != "" {
			kind = "rotated"
			hash = apikey.HashToken(replacement)
		}
		credential.Runner.ExpiresAt = now.Add(runnerauth.CredentialTTL)
		result, err := tx.ExecContext(ctx, "UPDATE api_tokens SET token_hash = ?, token_fingerprint = ?, expires_at = ?, updated_at = ?, rotated_at = CASE WHEN ? = 'rotated' THEN ? ELSE rotated_at END WHERE id = ?", hash, tokenFingerprint(hash), formatHubTime(credential.Runner.ExpiresAt), formatHubTime(now), kind, formatHubTime(now), credential.ID)
		if err := requireRunnerUpdate(result, err); err != nil {
			return nil, runnerCollision()
		}
		if err := recordRunnerEvent(ctx, tx, credential.Runner.RunnerID, credential.ID, kind, now); err != nil {
			return nil, err
		}
		credential.Runner.ProjectIDs, err = readRunnerProjects(ctx, tx, credential.ID)
		return credential.Runner, err
	})
}

func (s *Service) revokeRunnerIdentity(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	return s.runnerTransaction(c, http.StatusNoContent, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ?, updated_at = ? WHERE revoked_at IS NULL AND id IN
(SELECT token_id FROM runner_identities WHERE id = ? AND organization_id = ?)`, formatHubTime(now), formatHubTime(now), c.Param("runner"), c.Param("organization"))
		if err := requireRunnerUpdate(result, err); err != nil {
			return nil, err
		}
		return struct{}{}, recordRunnerEvent(ctx, tx, c.Param("runner"), credential.ID, "revoked", now)
	})
}

func recordRunnerEvent(ctx context.Context, tx *sql.Tx, runner, actor, kind string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO runner_identity_events (runner_id, actor_id, kind, occurred_at) VALUES (?, ?, ?, ?)", runner, actor, kind, formatHubTime(now))
	return err
}

func (s *Service) heartbeatNativeMachine(c echo.Context) error {
	var request struct {
		DisplayName string `json:"display_name"`
		Capacity    int    `json:"capacity"`
		Version     string `json:"version"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if scope.credential.Runner.RunnerID != "" && string(scope.credential.Runner.MachineID) != c.Param("machine") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if len(request.DisplayName) > 200 || request.Capacity < 0 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 {
		return s.nativeAPIError(c, nativeInvalid("Display name, version and nonnegative capacity are required"))
	}
	return s.runnerTransaction(c, http.StatusNoContent, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		result, err := tx.ExecContext(ctx, `UPDATE machines SET display_name = ?, capacity = ?, version = ?, last_heartbeat_at = ?, updated_at = ? WHERE id = ? AND organization_id = ? AND token_id = ?`, request.DisplayName, request.Capacity, request.Version, formatHubTime(now), formatHubTime(now), c.Param("machine"), scope.organization, scope.credential.ID)
		return struct{}{}, requireRunnerUpdate(result, err)
	})
}
