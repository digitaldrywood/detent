package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func runnerOperationAllowed(c echo.Context, operations []string) bool {
	path := c.Path()
	if (path == runnerBase+"/:runner" || path == runnerBase+"/:runner/routing") && c.Request().Method == http.MethodGet || path == runnerBase+"/:runner/renew" || path == runnerBase+"/:runner/rotate" {
		return true
	}
	operation := ""
	switch {
	case path == "/api/v2/capabilities":
		operation = runnerauth.Read
	case !strings.HasPrefix(path, nativeBase):
		return false
	case path == nativeBase+"/claims", path == nativeBase+"/claims/preview", path == nativeBase+"/claims/wait", path == nativeBase+"/leases/:lease/renew", path == nativeBase+"/leases/:lease/release", path == nativeBase+"/leases/:lease/validate", path == nativeBase+"/landing-barrier" && c.Request().Method == http.MethodPost:
		operation = runnerauth.Claim
	case c.Request().Method == http.MethodGet:
		operation = runnerauth.Read
	case path == nativeBase+"/attempts/:attempt/diff", path == nativeBase+"/attempts/:attempt/diff/check":
		// The diff is written by the runner holding the attempt's lease;
		// postAttemptDiff re-checks that lease and its fencing token.
		operation = runnerauth.Claim
	case path == nativeBase+"/onboarding/issue-intake/result":
		operation = runnerauth.Heartbeat
	case path == nativeBase+"/work-items/:item/source-intake":
		operation = runnerauth.Claim
	case path == nativeBase+"/machines/register", path == nativeBase+"/machines/:machine/heartbeat", path == nativeBase+"/policy/observed":
		operation = runnerauth.Heartbeat
	case strings.HasPrefix(path, nativeBase+"/workspaces/:workspace/worker/"):
		// Binding, heartbeating and serving a workspace session is the same
		// authority as claiming work: the runner is taking and holding a
		// dispatched item, and the item happens to be a worktree rather than
		// a turn (decisions section 18.1).
		operation = runnerauth.Claim
	case path == nativeBase+"/work-items/:item/events", path == nativeBase+"/conversations/:conversation/turn-events" && c.Request().Method == http.MethodPost:
		operation = runnerauth.Events
	case path == nativeBase+"/attempts/:attempt/evidence", path == nativeBase+"/attempts/:attempt/evidence/check":
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
	if apikey.HashToken(replacement) == credential.Hash {
		return s.nativeAPIError(c, nativeInvalid("Rotation requires a different credential"))
	}
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		// runnerTransaction rechecks the current hash, revocation and time
		// policy before this mutation; renewal alone permits elapsed expiry.
		hash := credential.Hash
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
	_, err := s.revokeRunnerIdentityCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) revokeRunnerIdentityCommand(ctx context.Context, scope nativeScope, resource string) (any, error) {
	return s.runnerAdminTransaction(ctx, scope, true, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		var token string
		var removed bool
		if err := tx.QueryRowContext(ctx, "SELECT token_id, removed_at IS NOT NULL FROM runner_identities WHERE id = ? AND organization_id = ?", resource, scope.organization).Scan(&token, &removed); err != nil {
			return nil, err
		}
		if removed {
			return struct{}{}, nil
		}
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM leases l JOIN lease_runners lr ON lr.lease_id = l.lease_id
LEFT JOIN native_attempts a ON a.lease_id = l.lease_id
WHERE lr.runner_id = ? AND (a.status = 'running' OR (l.released_at IS NULL AND julianday(l.expires_at) > julianday(?))))`, resource, formatHubTime(now)).Scan(&busy); err != nil {
			return nil, err
		}
		if busy {
			return nil, nativeInvalid("This runner has active work. Drain it and wait for its attempts to finish before removing it.")
		}
		var revoked bool
		if err := tx.QueryRowContext(ctx, "SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = ?", token).Scan(&revoked); err != nil {
			return nil, err
		}
		if !revoked {
			if err := revokeRunnerIdentityInTx(ctx, tx, scope, resource, now); err != nil {
				return nil, err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE runner_identities SET removed_at = ?, revision = revision + 1 WHERE id = ? AND organization_id = ?", formatHubTime(now), resource, scope.organization)
		return struct{}{}, err
	})
}

func revokeRunnerIdentityInTx(ctx context.Context, tx *sql.Tx, scope nativeScope, resource string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ?, updated_at = ? WHERE revoked_at IS NULL AND id IN
(SELECT token_id FROM runner_identities WHERE id = ? AND organization_id = ?)`, formatHubTime(now), formatHubTime(now), resource, string(scope.organization))
	if err := requireRunnerUpdate(result, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET released_at = ?, updated_at = ? WHERE released_at IS NULL AND lease_id IN
(SELECT lr.lease_id FROM lease_runners lr JOIN runner_identities r ON r.id = lr.runner_id WHERE r.id = ? AND r.organization_id = ?)`, formatHubTime(now), formatHubTime(now), resource, scope.organization); err != nil {
		return err
	}
	return recordRunnerEvent(ctx, tx, resource, scope.credential.ID, "revoked", now)
}

func recordRunnerEvent(ctx context.Context, tx *sql.Tx, runner, actor, kind string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO runner_identity_events (runner_id, actor_id, kind, occurred_at) VALUES (?, ?, ?, ?)", runner, actor, kind, formatHubTime(now))
	return err
}

func (s *Service) heartbeatNativeMachine(c echo.Context) error {
	var request struct {
		Admission            *tracker.NativeAdmissionObservation `json:"admission,omitempty"`
		SpriteName           string                              `json:"sprite_name,omitempty"`
		Update               *runnerauth.UpdateObservation       `json:"update,omitempty"`
		CapacityConfig       *runnerauth.CapacityConfig          `json:"capacity_configuration,omitempty"`
		ProjectConfiguration *runnerauth.ProjectConfiguration    `json:"project_configuration,omitempty"`
		LocalChecks          *runnerauth.LocalChecks             `json:"local_checks,omitempty"`
		Problems             []runnerauth.Problem                `json:"problems"`
		ProtocolMajor        int                                 `json:"protocol_major,omitempty"`
		SettingsRejected     bool                                `json:"settings_rejected,omitempty"`
		BackendIsolation     isolation.Report                    `json:"backend_isolation,omitempty"`
		ProviderReports      []providercapacity.Report           `json:"provider_reports,omitempty"`
		DisplayName          string                              `json:"display_name"`
		Capacity             int                                 `json:"capacity"`
		Version              string                              `json:"version"`
		OS                   string                              `json:"os,omitempty"`
		Architecture         string                              `json:"architecture,omitempty"`
		// WorkspaceCapabilities and WorkspaceIsolation are what this runner
		// can serve for a workspace session (decisions section 18.10). They
		// ride the heartbeat beside the provider reports because the claim
		// gate asks one question -- can this runner serve this workspace, and
		// was it saying so recently -- and freshness means nothing unless the
		// answer and the heartbeat are the same row.
		WorkspaceCapabilities *workspacesession.Capabilities `json:"workspace_capabilities,omitempty"`
		WorkspaceIsolation    string                         `json:"workspace_isolation,omitempty"`
		CheckoutRepository    *string                        `json:"checkout_repository,omitempty"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if !workspacesession.ValidIsolation(request.WorkspaceIsolation) {
		return s.nativeAPIError(c, nativeInvalid("Workspace isolation must be sandbox, container or user"))
	}
	scope := nativeRequestScope(c)
	if scope.credential.Runner.RunnerID != "" && string(scope.credential.Runner.MachineID) != c.Param("machine") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if len(request.DisplayName) > 200 || request.Capacity < 0 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || !validRunnerPlatform(request.OS, request.Architecture) {
		return s.nativeAPIError(c, nativeInvalid("Display name, version and nonnegative capacity are required"))
	}
	target := runnerUpdateTarget(s.config.Version)
	published := false
	if scope.credential.Runner.RunnerID != "" {
		published = s.runnerReleasePublished(c.Request().Context(), target, request.OS, request.Architecture)
	}
	status := http.StatusNoContent
	if scope.credential.Runner.RunnerID != "" {
		status = http.StatusOK
	}
	return s.runnerTransaction(c, status, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		if scope.credential.Runner.RunnerID != "" {
			if request.SpriteName != "" {
				if !validSpritesSlug(request.SpriteName) {
					return nil, nativeInvalid("Sprite name is invalid")
				}
				result, err := tx.ExecContext(ctx, "UPDATE machines SET capabilities_json=json_set(capabilities_json, '$.sprite_name', ?) WHERE id=? AND organization_id=? AND hostname=?", request.SpriteName, scope.credential.Runner.MachineID, scope.organization, request.SpriteName)
				if err != nil {
					return nil, err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return nil, err
				}
				if count != 1 {
					return nil, nativeInvalid("Sprite name must match the runner hostname")
				}
			}
			if err := storeRunnerAdmissionObservation(ctx, tx, scope, request.Admission, now); err != nil {
				return nil, err
			}
			if err := s.storeRunnerUpdateObservation(ctx, tx, scope, request.Update, request.ProtocolMajor, request.Version, request.OS, request.Architecture, now); err != nil {
				return nil, err
			}
			if request.CapacityConfig != nil {
				if err := request.CapacityConfig.Validate(); err != nil {
					return nil, nativeInvalid(err.Error())
				}
				request.CapacityConfig.ObservedAt = now
				raw, err := json.Marshal(request.CapacityConfig)
				if err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET capacity_configuration_json = ? WHERE organization_id = ? AND id = ?", string(raw), scope.organization, scope.credential.Runner.RunnerID); err != nil {
					return nil, err
				}
			}
			if err := updateRunnerIsolationReport(ctx, tx, scope, request.BackendIsolation); err != nil {
				return nil, err
			}
			if err := updateRunnerHeartbeat(ctx, tx, scope, request.Capacity, request.Version, request.OS, request.Architecture, now); err != nil {
				return nil, err
			}
			if err := updateRunnerWorkspaceReport(ctx, tx, scope, request.WorkspaceCapabilities, request.WorkspaceIsolation); err != nil {
				return nil, err
			}
			if err := updateRunnerCheckoutReport(ctx, tx, scope, request.CheckoutRepository, now); err != nil {
				return nil, err
			}
			if err := updateProviderReports(ctx, tx, scope, request.ProviderReports, now); err != nil {
				return nil, err
			}
			if request.LocalChecks != nil {
				if err := request.LocalChecks.Validate(); err != nil {
					return nil, nativeInvalid(err.Error())
				}
				request.LocalChecks.ObservedAt = now
				raw, err := json.Marshal(request.LocalChecks)
				if err != nil {
					return nil, err
				}
				_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET local_checks_json = json_set(local_checks_json, ?, json(?)) WHERE id = ? AND organization_id = ?", "$."+string(scope.project), string(raw), scope.credential.Runner.RunnerID, scope.organization)
				if err != nil {
					return nil, err
				}
			}
			snapshot, err := readRunnerRoutingSnapshot(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
			if err != nil {
				return nil, err
			}
			if err := readRunnerClaimState(ctx, tx, scope, &snapshot, now); err != nil {
				return nil, err
			}
			configurationRequest, err := s.runnerProjectConfiguration(ctx, tx, scope, request.ProjectConfiguration, now)
			if err != nil {
				return nil, err
			}
			if err := updateRunnerProblems(ctx, tx, scope, request.Problems, request.ProtocolMajor, request.SettingsRejected, now); err != nil {
				return nil, err
			}
			if configurationRequest.RequestID != "" {
				snapshot.ProjectConfigurationRequest = &configurationRequest
			}
			if published {
				snapshot.TargetRunnerVersion = strings.TrimPrefix(target, "v")
			}
			snapshot.GitHubIntake, err = readGitHubBatchTask(ctx, tx, scope)
			return snapshot, err
		}
		if request.Admission != nil {
			return nil, nativeInvalid("Admission observations require an enrolled runner")
		}
		if request.Update != nil {
			return nil, nativeInvalid("Update evidence requires an enrolled runner")
		}
		if request.ProjectConfiguration != nil {
			return nil, nativeInvalid("Project configuration requires an enrolled runner")
		}
		if request.LocalChecks != nil {
			return nil, nativeInvalid("Local checks require an enrolled runner")
		}
		if len(request.ProviderReports) != 0 {
			return nil, nativeInvalid("Provider reports require an enrolled runner")
		}
		if request.WorkspaceCapabilities != nil {
			return nil, nativeInvalid("Workspace capabilities require an enrolled runner")
		}
		if request.CheckoutRepository != nil && *request.CheckoutRepository != "" {
			return nil, nativeInvalid("Checkout reports require an enrolled runner")
		}
		result, err := tx.ExecContext(ctx, `UPDATE machines SET display_name = ?, capacity = ?, version = ?, last_heartbeat_at = ?, updated_at = ? WHERE id = ? AND organization_id = ? AND token_id = ?`, request.DisplayName, request.Capacity, request.Version, formatHubTime(now), formatHubTime(now), c.Param("machine"), scope.organization, scope.credential.ID)
		return struct{}{}, requireRunnerUpdate(result, err)
	})
}

func validRunnerPlatform(os, architecture string) bool {
	return (os == "" || policy.ValidToken(os)) && (architecture == "" || policy.ValidToken(architecture))
}

func updateRunnerHeartbeat(ctx context.Context, tx *sql.Tx, scope nativeScope, capacity int, version, os, architecture string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE runner_identities SET reported_capacity = ?, os = ?, architecture = ?, last_heartbeat_at = ? WHERE id = ? AND token_id = ? AND organization_id = ?`, capacity, os, architecture, formatHubTime(now), scope.credential.Runner.RunnerID, scope.credential.ID, scope.organization)
	if err != nil {
		return err
	}
	if err := requireRunnerUpdate(result, nil); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE machines SET version = ?, last_heartbeat_at = ?, updated_at = ? WHERE id = ?", version, formatHubTime(now), formatHubTime(now), scope.credential.Runner.MachineID)
	return err
}
