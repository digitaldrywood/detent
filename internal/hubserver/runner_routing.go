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

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerIdentitySelect = `SELECT r.id, r.organization_id, r.machine_id, r.token_id, r.display_name, r.tags_json, r.state, r.capacity_limit,
r.reported_capacity, r.os, r.architecture, r.last_heartbeat_at, r.revision, r.operations_json, r.routing_settings_json, r.home_dry_since,
m.hostname, m.display_name, m.capacity, m.routing_revision, t.created_at, t.expires_at, t.revoked_at,
(SELECT json_group_array(project_id) FROM (SELECT project_id FROM token_grants WHERE token_id = r.token_id ORDER BY project_id)),
r.problems_json, r.backend_isolation_json, r.reported_protocol_major, r.settings_rejected, r.capacity_configuration_json, r.provider_reports_json, r.update_observation_json
FROM runner_identities r JOIN machines m ON m.id = r.machine_id JOIN api_tokens t ON t.id = r.token_id`

func scanRunnerIdentity(row interface{ Scan(...any) error }, now time.Time) (runnerauth.Runner, []providercapacity.Report, error) {
	var r runnerauth.Runner
	var tags, operations, heartbeat, created, expires, token, settings string
	var projects, problems, isolationRaw, capacityRaw, providerRaw, updateRaw string
	var protocol int
	var rejected bool
	var revoked, dry sql.NullString
	err := row.Scan(&r.RunnerID, &r.OrganizationID, &r.MachineID, &token, &r.DisplayName, &tags, &r.State, &r.CapacityLimit,
		&r.ReportedCapacity, &r.OS, &r.Architecture, &heartbeat, &r.Revision, &operations, &settings, &dry,
		&r.Hostname, &r.HostDisplayName, &r.HostCapacity, &r.HostRevision, &created, &expires, &revoked, &projects, &problems, &isolationRaw, &protocol, &rejected, &capacityRaw, &providerRaw, &updateRaw)
	if err != nil {
		return r, nil, err
	}
	if err := json.Unmarshal([]byte(tags), &r.Tags); err != nil {
		return r, nil, err
	}
	if err := unmarshalRunnerSettings(settings, &r.Routing); err != nil {
		return r, nil, err
	}
	if err := json.Unmarshal([]byte(operations), &r.Operations); err != nil {
		return r, nil, err
	}
	r.LastHeartbeatAt, err = parseTimeValue(heartbeat)
	if err != nil {
		return r, nil, err
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
	r.ConnectionHealth = r.Health
	if err := json.Unmarshal([]byte(projects), &r.ProjectIDs); err != nil {
		return r, nil, err
	}
	r.Routing = r.Normalized()
	if err := applyRunnerProblems(&r, problems, isolationRaw, protocol, rejected); err != nil {
		return r, nil, err
	}
	if dry.Valid {
		since, err := parseTimeValue(dry.String)
		if err != nil {
			return r, nil, err
		}
		r.HomeDrySince = &since
	}
	r.HomeStatus = r.HomeWorkStatus(now)
	if err := json.Unmarshal([]byte(updateRaw), &r.Update); err != nil {
		return r, nil, err
	}
	var reports []providercapacity.Report
	if err := json.Unmarshal([]byte(capacityRaw), &r.CapacityConfig); err != nil {
		return r, nil, err
	}
	if err := json.Unmarshal([]byte(providerRaw), &reports); err != nil {
		return r, nil, err
	}
	return r, reports, nil
}

func readRunner(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, now time.Time) (runnerauth.Runner, error) {
	r, reports, err := scanRunnerIdentity(db.QueryRowContext(ctx, runnerIdentitySelect+" WHERE r.organization_id = ? AND r.id = ?", organization, id), now)
	if err != nil {
		return r, err
	}
	if err := applyUrgentRunnerRouting(ctx, db, &r); err != nil {
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
	if err := rows.Err(); err != nil {
		return r, err
	}
	if err := rows.Close(); err != nil {
		return r, err
	}
	for _, report := range reports {
		view, err := providerView(ctx, db, organization, report, now)
		if err != nil {
			return r, err
		}
		r.ProviderCapacity = append(r.ProviderCapacity, view)
	}
	for i := range r.Leases {
		reservation, reserved, err := readProviderReservation(ctx, db, r.Leases[i].ID)
		if err != nil {
			return r, err
		}
		if reserved {
			r.Leases[i].ProviderReservation = &reservation
		}
	}
	return r, nil
}

type runnerSettings struct {
	UpdateRequest   *runnerauth.UpdateRequest   `json:"update_request,omitempty"`
	CapacityRequest *runnerauth.CapacityRequest `json:"capacity_request,omitempty"`
	HomeProjectIDs  []tracker.ProjectID         `json:"home_project_ids"`
	IsolationTier   string                      `json:"isolation_tier"`
	HostServices    []string                    `json:"host_services"`
	Availability    runnerauth.Availability     `json:"availability"`
	Spillover       runnerauth.Spillover        `json:"spillover"`
}

func settingsFromRouting(r runnerauth.Routing) runnerSettings {
	return runnerSettings{UpdateRequest: r.UpdateRequest, CapacityRequest: r.CapacityRequest, HomeProjectIDs: r.HomeProjectIDs, IsolationTier: r.IsolationTier, HostServices: r.HostServices, Availability: r.Availability, Spillover: r.Spillover}
}

func unmarshalRunnerSettings(raw string, routing *runnerauth.Routing) error {
	var settings runnerSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return err
	}
	routing.UpdateRequest = settings.UpdateRequest
	routing.CapacityRequest = settings.CapacityRequest
	routing.IsolationTier = settings.IsolationTier
	routing.HostServices = settings.HostServices
	routing.Availability = settings.Availability
	routing.Spillover = settings.Spillover
	routing.HomeProjectIDs = settings.HomeProjectIDs
	return nil
}

func readRunnerRoutingSnapshot(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, now time.Time) (runnerauth.RoutingSnapshot, error) {
	runner, _, err := scanRunnerIdentity(db.QueryRowContext(ctx, runnerIdentitySelect+" WHERE r.organization_id = ? AND r.id = ?", organization, id), now)
	if err == nil {
		err = applyUrgentRunnerRouting(ctx, db, &runner)
	}
	return runnerauth.RoutingSnapshot{RunnerID: id, Revision: runner.Revision, Routing: runner.Routing}, err
}

func (s *Service) getRunnerRouting(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	r, err := s.readRunnerRouting(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, r)
}

func (s *Service) readRunnerRouting(ctx context.Context, scope nativeScope, runnerID string) (runnerauth.Runner, error) {
	credential := scope.credential
	if credential.Runner.RunnerID != "" && (credential.Runner.RunnerID != runnerID || credential.Runner.OrganizationID != scope.organization) || credential.Runner.RunnerID == "" && (credential.Scope != apiScopeAdmin || credential.NativeOnly) {
		return runnerauth.Runner{}, nativeNotFound()
	}
	var visible bool
	if err := s.database.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM runner_identities WHERE organization_id = ? AND id = ? AND removed_at IS NULL)", scope.organization, runnerID).Scan(&visible); err != nil {
		return runnerauth.Runner{}, err
	}
	if !visible {
		return runnerauth.Runner{}, nativeNotFound()
	}
	return readRunner(ctx, s.database.db, scope.organization, runnerID, s.config.now())
}

type runnerRoutingRequest struct {
	runnerauth.RoutingChange
	IsolationTier  *string                  `json:"isolation_tier"`
	HostServices   *[]string                `json:"host_services"`
	Availability   *runnerauth.Availability `json:"availability"`
	Spillover      *runnerauth.Spillover    `json:"spillover"`
	HomeProjectIDs *[]tracker.ProjectID     `json:"home_project_ids"`
}

// effective preserves optional settings exactly as the dashboard command does.
func (request runnerRoutingRequest) effective(current runnerauth.Routing) runnerauth.RoutingChange {
	change := request.RoutingChange
	change.UpdateRequest = current.UpdateRequest
	change.CapacityRequest = current.CapacityRequest
	if request.IsolationTier == nil || *request.IsolationTier == "" {
		change.IsolationTier = current.IsolationTier
	} else {
		change.IsolationTier = *request.IsolationTier
	}
	if request.HostServices == nil || *request.HostServices == nil {
		change.HostServices = current.HostServices
	} else {
		change.HostServices = *request.HostServices
	}
	if request.Availability == nil || request.Availability.Timezone == "" && request.Availability.Windows == nil && request.Availability.HardDeadline == "" {
		change.Availability = current.Availability
	} else {
		change.Availability = *request.Availability
	}
	if request.Spillover == nil || request.Spillover.Mode == "" && request.Spillover.AfterMinutes == 0 {
		change.Spillover = current.Spillover
	} else {
		change.Spillover = *request.Spillover
	}
	if request.HomeProjectIDs == nil {
		change.HomeProjectIDs = current.HomeProjectIDs
	} else {
		change.HomeProjectIDs = *request.HomeProjectIDs
	}
	change.Routing = change.Normalized()
	return change
}

func (s *Service) updateRunnerRouting(c echo.Context) error {
	var request runnerRoutingRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	value, err := s.updateRunnerRoutingCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateRunnerRoutingCommand(ctx context.Context, scope nativeScope, resource string, request runnerRoutingRequest) (any, error) {
	change := request.RoutingChange
	return s.runnerAdminTransaction(ctx, scope, false, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		organization := tracker.OrganizationID(string(scope.organization))
		r, err := readRunner(ctx, tx, organization, resource, now)
		if err != nil {
			return nil, err
		}
		if r.Revision != change.ExpectedRevision {
			return nil, nativeConflict(tracker.Revision(r.Revision))
		}
		change = request.effective(r.Routing)
		if change.CapacityLimit != r.CapacityLimit {
			change.CapacityRequest = nil
		}
		if (change.CapacityLimit != r.CapacityLimit || r.CapacityRequiresApplication(change.CapacityLimit)) && change.CapacityLimit > 0 && freshCapacityConfig(r, now) {
			change.CapacityRequest = &runnerauth.CapacityRequest{ExpectedConfigRevision: r.CapacityConfig.Revision, Capacity: change.CapacityLimit}
		}
		if err := change.Validate(); err != nil {
			return nil, nativeInvalid(err.Error())
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
		settings, err := marshalNative(settingsFromRouting(change.Routing))
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runner_identities SET display_name = ?, tags_json = ?, state = ?, capacity_limit = ?, routing_settings_json = ?, home_dry_since = CASE WHEN ? THEN NULL ELSE home_dry_since END, revision = revision + 1 WHERE id = ?`, change.DisplayName, tags, change.State, change.CapacityLimit, settings, !slices.Equal(change.HomeProjectIDs, r.HomeProjectIDs) || change.Spillover != r.Spillover, r.RunnerID); err != nil {
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
		if err := refreshRunnerProblems(ctx, tx, organization, r.RunnerID, now); err != nil {
			return nil, err
		}
		return readRunner(ctx, tx, organization, r.RunnerID, now)
	})
}

func (s *Service) updateRunnerHost(c echo.Context) error {
	var change runnerauth.HostChange
	if err := decodeAPIJSON(c, &change); err != nil {
		return invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	value, err := s.updateRunnerHostCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("machine"), change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateRunnerHostCommand(ctx context.Context, scope nativeScope, resource string, change runnerauth.HostChange) (any, error) {
	change.DisplayName = strings.TrimSpace(change.DisplayName)
	if change.DisplayName == "" || len(change.DisplayName) > 200 || strings.ContainsAny(change.DisplayName, "\r\n\x00") || change.Capacity < 0 || change.Capacity > 10000 {
		return nil, nativeInvalid("Host name and a capacity between 0 and 10000 are required")
	}
	return s.runnerAdminTransaction(ctx, scope, false, func(ctx context.Context, tx *sql.Tx, _ time.Time) (any, error) {
		var revision int64
		if err := tx.QueryRowContext(ctx, "SELECT routing_revision FROM machines WHERE organization_id = ? AND id = ?", string(scope.organization), resource).Scan(&revision); err != nil {
			return nil, err
		}
		if revision != change.ExpectedRevision {
			return nil, nativeConflict(tracker.Revision(revision))
		}
		if _, err := tx.ExecContext(ctx, "UPDATE machines SET display_name = ?, capacity = ?, routing_revision = routing_revision + 1 WHERE id = ?", change.DisplayName, change.Capacity, resource); err != nil {
			return nil, err
		}
		change.ExpectedRevision++
		return change, nil
	})
}

func (s *Service) listRunnerRouting(c echo.Context) error {
	result, err := s.listRunnerRoutingData(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), 0, 0)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) listRunnerRoutingData(ctx context.Context, organization tracker.OrganizationID, limit, offset int) ([]runnerauth.Runner, error) {
	query := "SELECT id FROM runner_identities WHERE organization_id = ? AND removed_at IS NULL ORDER BY display_name, id"
	args := []any{organization}
	if limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	rows, err := s.database.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []runnerauth.Runner{}
	for _, id := range ids {
		r, err := readRunner(ctx, s.database.db, organization, id, s.config.now())
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
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
	if hash != credential.Hash || revoked.Valid || !credential.timeValid(now, created, expires) {
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
		return validateRunnerLeaseTx(ctx, tx, nativeRequestScope(c), tracker.LeaseID(c.Param("lease")), request.FencingToken, now)
	})
}

func validateRunnerLeaseTx(ctx context.Context, tx *sql.Tx, scope nativeScope, id tracker.LeaseID, fencing tracker.FencingToken, now time.Time) (runnerauth.Runner, error) {
	if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
		return runnerauth.Runner{}, err
	}
	if err := requireLeaseRunner(ctx, tx, id, scope); err != nil {
		return runnerauth.Runner{}, err
	}
	lease, found, err := readLeaseByID(ctx, tx, id)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if !found {
		return runnerauth.Runner{}, nativeNotFound()
	}
	if err := requireCurrentLease(lease, fencing, now); err != nil {
		return runnerauth.Runner{}, err
	}
	if err := requireApprovedLeasePolicy(ctx, tx, id, true); err != nil {
		return runnerauth.Runner{}, err
	}
	if scope.credential.Runner.RunnerID == "" {
		return runnerauth.Runner{Binding: runnerauth.Binding{MachineID: lease.session.Machine.ID}}, nil
	}
	approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return runnerauth.Runner{}, err
	}
	r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if err := runnerExcluded(r.Exclusions(scope.project, approval.Policy.Requirements, true)); err != nil {
		return runnerauth.Runner{}, err
	}
	return r, nil
}
