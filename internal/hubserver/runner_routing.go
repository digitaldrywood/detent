package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRunner(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, now time.Time) (runnerauth.Runner, error) {
	var r runnerauth.Runner
	var tags, operations, heartbeat, created, expires, token string
	var revoked sql.NullString
	err := db.QueryRowContext(ctx, `SELECT r.id, r.organization_id, r.machine_id, r.token_id, r.display_name, r.tags_json, r.state, r.capacity_limit,
r.reported_capacity, r.os, r.architecture, r.last_heartbeat_at, r.revision, r.operations_json,
m.hostname, m.display_name, m.capacity, m.routing_revision, t.created_at, t.expires_at, t.revoked_at
FROM runner_identities r JOIN machines m ON m.id = r.machine_id JOIN api_tokens t ON t.id = r.token_id
WHERE r.organization_id = ? AND r.id = ?`, organization, id).Scan(&r.RunnerID, &r.OrganizationID, &r.MachineID, &token, &r.DisplayName, &tags, &r.State, &r.CapacityLimit,
		&r.ReportedCapacity, &r.OS, &r.Architecture, &heartbeat, &r.Revision, &operations,
		&r.Hostname, &r.HostDisplayName, &r.HostCapacity, &r.HostRevision, &created, &expires, &revoked)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(tags), &r.Tags); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(operations), &r.Operations); err != nil {
		return r, err
	}
	r.LastHeartbeatAt, err = parseTimeValue(heartbeat)
	if err != nil {
		return r, err
	}
	r.Health = "online"
	switch {
	case revoked.Valid:
		r.Health = "revoked"
	case !runnerTimeValid(now, created, expires):
		r.Health = "expired"
	case now.Before(r.LastHeartbeatAt) || !now.Before(r.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)):
		r.Health = "offline"
	}
	r.ProjectIDs, err = readRunnerProjects(ctx, db, token)
	if err != nil {
		return r, err
	}
	r.Leases = []runnerauth.RunnerLease{}
	rows, err := db.QueryContext(ctx, `SELECT l.expires_at, coalesce(lr.runner_id, ''), l.lease_id, coalesce(i.native_id, ''), i.title, coalesce(i.project_id, ''), coalesce(p.metadata_json, ''), coalesce(pp.policy_id, '')
FROM leases l JOIN issues i ON i.id = l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id
LEFT JOIN lease_policies lp ON lp.lease_id = l.lease_id LEFT JOIN policy_revisions p ON p.scope = lp.scope AND p.policy_id = lp.policy_id
LEFT JOIN project_policies pp ON pp.scope = lp.scope WHERE l.machine_id = ? AND l.released_at IS NULL ORDER BY l.fencing_token`, r.MachineID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var expiry, runner, raw, approved string
		var lease runnerauth.RunnerLease
		if err := rows.Scan(&expiry, &runner, &lease.ID, &lease.WorkItemID, &lease.Title, &lease.ProjectID, &raw, &approved); err != nil {
			return r, err
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return r, err
		}
		if end.After(now) {
			r.HostUsed++
			if runner == id {
				r.Used++
				lease.ExpiresAt = end
				if raw != "" {
					if err := json.Unmarshal([]byte(raw), &lease.Policy); err != nil {
						return r, err
					}
				}
				lease.Exclusions = r.Exclusions(lease.ProjectID, lease.Policy.Requirements, true)
				if lease.Policy.ID == "" || lease.Policy.ID != approved {
					lease.Exclusions = append(lease.Exclusions, runnerauth.Exclusion{Code: "policy_mismatch", Message: "This run's pinned policy is missing or revoked"})
				}
				r.Leases = append(r.Leases, lease)
			}
		}
	}
	return r, rows.Err()
}

func (s *Service) getRunnerRouting(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Runner.RunnerID != "" && (credential.Runner.RunnerID != c.Param("runner") || string(credential.Runner.OrganizationID) != c.Param("organization")) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if credential.Runner.RunnerID == "" && (credential.Scope != apiScopeAdmin || credential.NativeOnly) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	r, err := readRunner(c.Request().Context(), s.database.db, tracker.OrganizationID(c.Param("organization")), c.Param("runner"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, r)
}

func (s *Service) updateRunnerRouting(c echo.Context) error {
	var change runnerauth.RoutingChange
	if err := decodeAPIJSON(c, &change); err != nil {
		return invalidAPIRequest(c, err)
	}
	change.Routing = change.Normalized()
	if err := change.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		organization := tracker.OrganizationID(c.Param("organization"))
		r, err := readRunner(ctx, tx, organization, c.Param("runner"), now)
		if err != nil {
			return nil, err
		}
		if r.Revision != change.ExpectedRevision {
			return nil, nativeConflict(tracker.Revision(r.Revision))
		}
		for _, project := range change.ProjectIDs {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?", organization, project).Scan(&count); err != nil {
				return nil, err
			}
			if count != 1 {
				return nil, nativeInvalid("Runner projects must belong to the organization")
			}
		}
		tags, err := marshalNative(change.Tags)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runner_identities SET display_name = ?, tags_json = ?, state = ?, capacity_limit = ?, revision = revision + 1 WHERE id = ?`, change.DisplayName, tags, change.State, change.CapacityLimit, r.RunnerID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM token_grants WHERE token_id = (SELECT token_id FROM runner_identities WHERE id = ?)", r.RunnerID); err != nil {
			return nil, err
		}
		for _, project := range change.ProjectIDs {
			if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants (token_id, organization_id, project_id) SELECT token_id, organization_id, ? FROM runner_identities WHERE id = ?", project, r.RunnerID); err != nil {
				return nil, err
			}
		}
		return readRunner(ctx, tx, organization, r.RunnerID, now)
	})
}

func (s *Service) updateRunnerHost(c echo.Context) error {
	var change runnerauth.HostChange
	if err := decodeAPIJSON(c, &change); err != nil {
		return invalidAPIRequest(c, err)
	}
	change.DisplayName = strings.TrimSpace(change.DisplayName)
	if change.DisplayName == "" || len(change.DisplayName) > 200 || strings.ContainsAny(change.DisplayName, "\r\n\x00") || change.Capacity < 0 || change.Capacity > 10000 {
		return s.nativeAPIError(c, nativeInvalid("Host name and a capacity between 0 and 10000 are required"))
	}
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, _ time.Time) (any, error) {
		var revision int64
		if err := tx.QueryRowContext(ctx, "SELECT routing_revision FROM machines WHERE organization_id = ? AND id = ?", c.Param("organization"), c.Param("machine")).Scan(&revision); err != nil {
			return nil, err
		}
		if revision != change.ExpectedRevision {
			return nil, nativeConflict(tracker.Revision(revision))
		}
		if _, err := tx.ExecContext(ctx, "UPDATE machines SET display_name = ?, capacity = ?, routing_revision = routing_revision + 1 WHERE id = ?", change.DisplayName, change.Capacity, c.Param("machine")); err != nil {
			return nil, err
		}
		change.ExpectedRevision++
		return change, nil
	})
}

func (s *Service) listRunnerRouting(c echo.Context) error {
	ctx := c.Request().Context()
	organization := tracker.OrganizationID(c.Param("organization"))
	rows, err := s.database.db.QueryContext(ctx, "SELECT id FROM runner_identities WHERE organization_id = ? ORDER BY display_name, id", organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return s.nativeAPIError(c, err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result := []runnerauth.Runner{}
	for _, id := range ids {
		r, err := readRunner(ctx, s.database.db, organization, id, s.config.now())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		result = append(result, r)
	}
	return c.JSON(http.StatusOK, result)
}

func runnerExcluded(exclusions []runnerauth.Exclusion) error {
	if len(exclusions) == 0 {
		return nil
	}
	return &nativeError{Code: exclusions[0].Code, Message: exclusions[0].Message, status: http.StatusConflict}
}

func requireRunnerAuthority(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	if err := requireCredentialAuthority(ctx, tx, scope.credential, now); err != nil {
		return err
	}
	if scope.credential.Runner.RunnerID == "" {
		return nil
	}
	r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens t JOIN token_grants g ON g.token_id = t.id
WHERE t.id = ? AND t.token_hash = ? AND g.organization_id = ? AND g.project_id = ?`, scope.credential.ID, scope.credential.Hash, scope.organization, scope.project).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return nativeNotFound()
	}
	if r.State == "disabled" {
		return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
	}
	if r.Health == "revoked" || r.Health == "expired" {
		return runnerUnauthorized()
	}
	return nil
}

func requireCredentialAuthority(ctx context.Context, tx *sql.Tx, credential apiCredential, now time.Time) error {
	var hash, created string
	var revoked, expires sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT token_hash, created_at, revoked_at, expires_at FROM api_tokens WHERE id = ?", credential.ID).Scan(&hash, &created, &revoked, &expires)
	if err != nil {
		return err
	}
	if hash != credential.Hash || revoked.Valid || expires.Valid && !runnerTimeValid(now, created, expires.String) {
		return runnerUnauthorized()
	}
	return nil
}

func requireLeaseRunner(ctx context.Context, tx nativeQueryer, lease tracker.LeaseID, scope nativeScope) error {
	var count int
	query := `SELECT count(*) FROM leases l JOIN issues i ON i.id = l.issue_id JOIN machines m ON m.id = l.machine_id
WHERE l.lease_id = ? AND i.organization_id = ? AND i.project_id = ? AND m.organization_id = ? AND m.token_id = ?`
	args := []any{lease, scope.organization, scope.project, scope.organization, scope.credential.ID}
	if scope.credential.Runner.RunnerID != "" {
		query = `SELECT count(*) FROM leases l JOIN issues i ON i.id = l.issue_id JOIN lease_runners lr ON lr.lease_id = l.lease_id
JOIN runner_identities r ON r.id = lr.runner_id WHERE l.lease_id = ? AND i.organization_id = ? AND i.project_id = ? AND r.organization_id = ? AND r.token_id = ?`
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return nativeNotFound()
	}
	return nil
}

func validateRunnerDispatch(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return err
	}
	return runnerExcluded(r.Exclusions(scope.project, policy.Requirements{}, false))
}

func (s *Service) validateRunnerLease(c echo.Context) error {
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	return s.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		scope := nativeRequestScope(c)
		id := tracker.LeaseID(c.Param("lease"))
		if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		if err := requireLeaseRunner(ctx, tx, id, scope); err != nil {
			return nil, err
		}
		lease, found, err := readLeaseByID(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, nativeNotFound()
		}
		if err := requireCurrentLease(lease, request.FencingToken, now); err != nil {
			return nil, err
		}
		if err := requireApprovedLeasePolicy(ctx, tx, id, true); err != nil {
			return nil, err
		}
		if scope.credential.Runner.RunnerID == "" {
			return runnerauth.Runner{Binding: runnerauth.Binding{MachineID: lease.session.Machine.ID}}, nil
		}
		approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil {
			return nil, err
		}
		r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
		if err != nil {
			return nil, err
		}
		if err := runnerExcluded(r.Exclusions(scope.project, approval.Policy.Requirements, true)); err != nil {
			return nil, err
		}
		return r, nil
	})
}
