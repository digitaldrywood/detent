package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
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
	case path == runnerBase+"/:runner/update/receipt":
		operation = runnerauth.Heartbeat
	case path == "/api/v2/capabilities":
		operation = runnerauth.Read
	case !strings.HasPrefix(path, nativeBase):
		return false
	case path == nativeBase+"/claims", path == nativeBase+"/claims/preview", path == nativeBase+"/claims/wait", path == nativeBase+"/leases/:lease/renew", path == nativeBase+"/leases/:lease/release", path == nativeBase+"/leases/:lease/validate":
		operation = runnerauth.Claim
	case c.Request().Method == http.MethodGet:
		operation = runnerauth.Read
	case path == nativeBase+"/runner-sessions/find":
		operation = runnerauth.Read
	case path == nativeBase+"/runner-sessions", path == nativeBase+"/security-audits/dispositions":
		operation = runnerauth.Events
	case path == nativeBase+"/attempts/:attempt/diff", path == nativeBase+"/attempts/:attempt/diff/check":
		// The diff is written by the runner holding the attempt's lease;
		// postAttemptDiff re-checks that lease and its fencing token.
		operation = runnerauth.Claim
	case path == nativeBase+"/onboarding/issue-intake/result", path == nativeBase+"/onboarding/repositories/report":
		operation = runnerauth.Heartbeat
	case path == nativeBase+"/work-items/:item/source-intake":
		operation = runnerauth.Claim
	case path == nativeBase+"/machines/register", path == nativeBase+"/machines/:machine/heartbeat", path == nativeBase+"/policy/observed":
		operation = runnerauth.Heartbeat
	case strings.HasPrefix(path, nativeBase+"/workspaces/:workspace/worker/"):
		// Binding, heartbeating and serving a workspace session is the same
		// authority as claiming work: the runner is taking and holding a
		// dispatched item, and the item happens to be a worktree rather than
		// a turn.
		operation = runnerauth.Claim
	case path == nativeBase+"/work-items/:item/events", path == nativeBase+"/conversations/:conversation/turn-events" && c.Request().Method == http.MethodPost:
		operation = runnerauth.Events
	case path == nativeBase+"/attempts/:attempt/evidence", path == nativeBase+"/attempts/:attempt/evidence/check":
		operation = runnerauth.Events
	case path == nativeBase+"/go-test-receipts", path == nativeBase+"/go-test-receipts/attestations":
		operation = runnerauth.Events
	case strings.HasPrefix(path, attachmentMetadataBase):
		operation = runnerauth.Collaborate
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

func runnerProjectAccessSQL(organization, project string) string {
	return "r.organization_id=" + organization + " AND r.removed_at IS NULL AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.organization_id=" + organization + " AND g.project_id=" + project + "))"
}

func readRunnerProjects(ctx context.Context, query nativeQueryer, tokenID string, limits ...int) ([]tracker.ProjectID, error) {
	statement := "SELECT p.id FROM projects p JOIN runner_identities r ON r.organization_id=p.organization_id WHERE r.token_id=? AND (r.scope='organization' AND p.deleted_at IS NULL OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.project_id=p.id AND g.organization_id=p.organization_id)) ORDER BY p.id"
	args := []any{tokenID}
	if len(limits) > 0 {
		statement += " LIMIT ?"
		args = append(args, limits[0])
	}
	rows, err := query.QueryContext(ctx, statement, args...)
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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	credential, err := authenticatedRunner(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	identity, err := s.readRunnerIdentityTenant(ctx, tracker.OrganizationID(c.Param("organization")), c.Param("runner"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if identity.Binding != credential.Runner.Binding {
		return s.nativeAPIError(c, nativeNotFound())
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, identity)
}

func (s *Service) renewRunnerIdentity(c echo.Context) error {
	var request struct{}
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.changeRunnerCredential(c, "")
}

func (s *Service) rotateRunnerIdentity(c echo.Context) error {
	var request runnerauth.Rotation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if !runnerauth.ValidCredential(request.Credential) {
		return s.nativeAPIError(c, nativePublicInvalid("A host-generated replacement credential is required"))
	}
	return s.changeRunnerCredential(c, request.Credential)
}

func (s *Service) changeRunnerCredential(c echo.Context, replacement string) error {
	credential, err := authenticatedRunner(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if apikey.HashToken(replacement) == credential.Hash {
		return s.nativeAPIError(c, nativePublicInvalid("Rotation requires a different credential"))
	}
	identity, err := s.changeRunnerCredentialTenant(c.Request().Context(), credential, replacement)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, identity)
}

func (s *Service) changeRunnerCredentialTenant(ctx context.Context, credential apiCredential, replacement string) (runnerauth.Identity, error) {
	organization := credential.Runner.OrganizationID
	if s.config.TenantRepository == nil || organization == "" || credential.ID == "" {
		return runnerauth.Identity{}, runnerUnauthorized()
	}
	updated := runnerauth.Identity{}
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.RunnerIdentity(ctx, credential.Runner.RunnerID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return runnerUnauthorized()
			}
			return err
		}
		if record.RemovedAt != nil || record.TokenID != credential.ID || record.OrganizationID != organization || record.RunnerID != credential.Runner.RunnerID {
			return runnerUnauthorized()
		}
		current, err := w.CredentialByHash(ctx, credential.Hash)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return runnerUnauthorized()
			}
			return err
		}
		now := s.config.now()
		if current.ID != credential.ID || current.Scope != string(apiScopeWorker) || current.Kind != "service" || !validRunnerCredentialForRenewal(current, credential.Hash, now, replacement == "") || current.UserID != "" || current.MembershipID != "" {
			return runnerUnauthorized()
		}
		if replacement != "" && !record.ExpiresAt.After(now) {
			return runnerUnauthorized()
		}
		if record.MachineID != credential.Runner.MachineID || record.Scope != credential.Runner.Scope || !slices.Equal(record.Operations, credential.Runner.Operations) {
			return runnerUnauthorized()
		}
		newHash := current.Hash
		kind := "renewed"
		if replacement != "" {
			newHash, kind = apikey.HashToken(replacement), "rotated"
		}
		expires := now.Add(runnerauth.CredentialTTL)
		if err := w.RenewAccessCredential(ctx, current.ID, newHash, tokenFingerprint(newHash), expires, now); err != nil {
			return err
		}
		record.ExpiresAt = expires
		if err := w.UpdateRunnerIdentity(ctx, record); err != nil {
			return err
		}
		if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: organization, RunnerID: record.RunnerID, ActorID: current.ID, Kind: kind, OccurredAt: now}); err != nil {
			return err
		}
		updated, err = runnerIdentityRecord(ctx, w, record)
		return err
	})
	if err != nil {
		return runnerauth.Identity{}, err
	}
	return updated, nil
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.requireRunnerAdministrationTenant(ctx, w, scope); err != nil {
			return err
		}
		identity, err := w.RunnerIdentity(ctx, resource)
		if err != nil {
			return err
		}
		if identity.RemovedAt != nil {
			return nil
		}
		now := s.config.now().UTC()
		busy, err := w.RunnerHasActiveLease(ctx, resource, now)
		if err != nil {
			return err
		}
		if busy {
			return nativePublicInvalid("This runner has active work. Drain it and wait for its leases to finish or expire before removing it.")
		}
		credential, err := w.AccessCredential(ctx, identity.TokenID)
		if err != nil {
			return err
		}
		if credential.RevokedAt == nil {
			if err := w.RevokeAccessCredential(ctx, identity.TokenID, now); err != nil {
				return err
			}
			if err := w.RecordRunnerIdentityEvent(ctx, runnerstore.Event{OrganizationID: scope.organization, RunnerID: resource, ActorID: scope.credential.ID, Kind: "revoked", OccurredAt: now}); err != nil {
				return err
			}
		}
		if err := w.RemoveRunnerIdentity(ctx, resource, now); err != nil {
			return err
		}
		return s.applyRunnerTenantUsage(ctx, w, now, resource, true, false)
	})
	if err != nil {
		return nil, err
	}
	if s.notifications != nil {
		s.notifications.notify(dispatchNotificationKey(scope.organization))
	}
	return struct{}{}, nil
}

func revokeRunnerIdentityInTx(ctx context.Context, tx *sql.Tx, scope nativeScope, resource string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ?, updated_at = ? WHERE revoked_at IS NULL AND id IN
(SELECT token_id FROM runner_identities WHERE id = ? AND organization_id = ?)`, formatHubTime(now), formatHubTime(now), resource, string(scope.organization))
	if err := requireRunnerUpdate(result, err); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.lease_id FROM leases l JOIN lease_runners lr ON lr.lease_id=l.lease_id JOIN runner_identities r ON r.id=lr.runner_id
WHERE r.id=? AND r.organization_id=? AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)`, resource, scope.organization, formatHubTime(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []tracker.LeaseID
	for rows.Next() {
		var id tracker.LeaseID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range ids {
		lease, found, err := readLeaseByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if found {
			if err := releaseLeaseRow(ctx, tx, lease, "cancelled", now); err != nil {
				return err
			}
		}
	}
	return recordRunnerEvent(ctx, tx, resource, scope.credential.ID, "revoked", now)
}

func recordRunnerEvent(ctx context.Context, tx *sql.Tx, runner, actor, kind string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO runner_identity_events (runner_id, actor_id, kind, occurred_at) VALUES (?, ?, ?, ?)", runner, actor, kind, formatHubTime(now))
	return err
}

type nativeMachineHeartbeatRequest struct {
	HostMetrics           json.RawMessage                     `json:"host_metrics,omitempty"`
	ChangeCursor          *string                             `json:"change_cursor,omitempty"`
	Admission             *tracker.NativeAdmissionObservation `json:"admission,omitempty"`
	SpriteName            string                              `json:"sprite_name,omitempty"`
	Update                *runnerauth.UpdateObservation       `json:"update,omitempty"`
	CapacityConfig        *runnerauth.CapacityConfig          `json:"capacity_configuration,omitempty"`
	ProjectConfiguration  *runnerauth.ProjectConfiguration    `json:"project_configuration,omitempty"`
	LocalChecks           *runnerauth.LocalChecks             `json:"local_checks,omitempty"`
	Problems              []runnerauth.Problem                `json:"problems"`
	ProtocolMajor         int                                 `json:"protocol_major,omitempty"`
	SettingsRejected      bool                                `json:"settings_rejected,omitempty"`
	BackendIsolation      isolation.Report                    `json:"backend_isolation,omitempty"`
	ProviderReports       []providercapacity.Report           `json:"provider_reports,omitempty"`
	DisplayName           string                              `json:"display_name"`
	Capacity              int                                 `json:"capacity"`
	Version               string                              `json:"version"`
	OS                    string                              `json:"os,omitempty"`
	Architecture          string                              `json:"architecture,omitempty"`
	WorkspaceCapabilities *workspacesession.Capabilities      `json:"workspace_capabilities,omitempty"`
	WorkspaceIsolation    string                              `json:"workspace_isolation,omitempty"`
	Skills                *[]skills.ProviderSkill             `json:"skills,omitempty"`
	CheckoutRepository    *string                             `json:"checkout_repository,omitempty"`
}

func (s *Service) heartbeatNativeMachine(c echo.Context) error {
	var request nativeMachineHeartbeatRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.heartbeatNativeMachineTenant(c, request)
}

func validRunnerPlatform(os, architecture string) bool {
	return (os == "" || policy.ValidToken(os)) && (architecture == "" || policy.ValidToken(architecture))
}
