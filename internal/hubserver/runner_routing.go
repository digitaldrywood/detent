package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerIdentitySelect = `SELECT r.id, r.organization_id, r.machine_id, r.token_id, r.display_name, r.tags_json, r.state, r.capacity_limit,
r.reported_capacity, r.os, r.architecture, r.last_heartbeat_at, r.revision, r.operations_json, r.routing_settings_json,
m.hostname, m.display_name, m.capacity, m.routing_revision, t.created_at, t.expires_at, t.revoked_at,
r.problems_json, r.backend_isolation_json, r.reported_protocol_major, r.settings_rejected, r.capacity_configuration_json, r.provider_reports_json, r.update_observation_json, r.scope,
m.version, o.urgent_runner_update_json,
COALESCE(m.sprite_name,''),CASE WHEN m.machine_facts_ready IS NULL THEN m.capabilities_json END
FROM runner_identities r JOIN machines m ON m.id = r.machine_id JOIN api_tokens t ON t.id = r.token_id JOIN organizations o ON o.id = r.organization_id`

func scanRunnerIdentity(row interface{ Scan(...any) error }, clock func() time.Time, extra ...any) (runnerauth.Runner, []providercapacity.Report, time.Time, error) {
	var r runnerauth.Runner
	var tags, operations, heartbeat, created, expires, token, settings string
	var problems, isolationRaw, capacityRaw, providerRaw, updateRaw string
	var version, urgentRaw, spriteName string
	var protocol int
	var rejected bool
	var revoked, machineLegacy sql.NullString
	destinations := []any{&r.RunnerID, &r.OrganizationID, &r.MachineID, &token, &r.DisplayName, &tags, &r.State, &r.CapacityLimit,
		&r.ReportedCapacity, &r.OS, &r.Architecture, &heartbeat, &r.Revision, &operations, &settings,
		&r.Hostname, &r.HostDisplayName, &r.HostCapacity, &r.HostRevision, &created, &expires, &revoked, &problems, &isolationRaw, &protocol, &rejected, &capacityRaw, &providerRaw, &updateRaw, &r.Scope, &version, &urgentRaw, &spriteName, &machineLegacy}
	err := row.Scan(append(destinations, extra...)...)
	if err != nil {
		return r, nil, time.Time{}, err
	}
	now := clock()
	if err := json.Unmarshal([]byte(tags), &r.Tags); err != nil {
		return r, nil, time.Time{}, err
	}
	if err := unmarshalRunnerSettings(settings, &r.Routing); err != nil {
		return r, nil, time.Time{}, err
	}
	if err := json.Unmarshal([]byte(operations), &r.Operations); err != nil {
		return r, nil, time.Time{}, err
	}
	r.LastHeartbeatAt, err = parseTimeValue(heartbeat)
	if err != nil {
		return r, nil, time.Time{}, err
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
	r.Routing = r.Normalized()
	if r.MachineUse == "" {
		r.MachineUse = "shared"
	}
	if err := json.Unmarshal([]byte(updateRaw), &r.Update); err != nil {
		return r, nil, time.Time{}, err
	}
	if err := applyRunnerProblems(&r, problems, isolationRaw, protocol, rejected, now); err != nil {
		return r, nil, time.Time{}, err
	}
	if machineLegacy.Valid {
		facts, err := resolveMachineSpriteFacts(machineSpriteFacts{}, machineLegacy)
		if err != nil {
			return r, nil, time.Time{}, err
		}
		spriteName = spriteFactValue(facts.Name)
	}
	if spriteName != "" && spriteName == r.Hostname && r.IsolationTier == "sandbox" {
		for i := range r.Problems {
			if r.Problems[i].Code == "tier_unavailable" {
				r.Problems[i].Message = spriteSandboxUnavailableReason
				r.Problems[i].FixHint = "Ask an organization owner or admin to approve switching this runner to Full access. Projects requiring sandbox cannot use it with Full access."
			}
		}
	}
	var urgent urgentRunnerUpdate
	if err := json.Unmarshal([]byte(urgentRaw), &urgent); err != nil {
		return r, nil, time.Time{}, err
	}
	applyUrgentRunnerRouting(&r, urgent, version)
	var reports []providercapacity.Report
	if err := json.Unmarshal([]byte(capacityRaw), &r.CapacityConfig); err != nil {
		return r, nil, time.Time{}, err
	}
	if err := json.Unmarshal([]byte(providerRaw), &reports); err != nil {
		return r, nil, time.Time{}, err
	}

	return r, reports, now, nil
}

func readRunner(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, now time.Time, joining ...string) (runnerauth.Runner, error) {
	return readRunnerWithClock(ctx, db, organization, id, func() time.Time { return now }, joining...)
}

func readRunnerWithClock(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, clock func() time.Time, joining ...string) (runnerauth.Runner, error) {
	return readRunnerFactsWithClock(ctx, db, organization, id, clock, true, joining...)
}

func readRunnerFactsWithClock(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, clock func() time.Time, display bool, joining ...string) (runnerauth.Runner, error) {
	r, reports, now, err := scanRunnerIdentity(db.QueryRowContext(ctx, runnerIdentitySelect+" WHERE r.organization_id = ? AND r.id = ?", organization, id), clock)
	if err != nil {
		return r, err
	}
	if err := readRunnerIdentityFacts(ctx, db, &r, reports); err != nil {
		return r, err
	}
	r.ProjectRanks, err = readRunnerProjectRanks(ctx, db, r.RunnerID, r.OrganizationID)
	if err != nil {
		return r, err
	}
	group := ""
	if len(joining) > 0 {
		group = joining[0]
	}
	r.HostUsed, r.Used, err = machineClaimUsage(ctx, db, r.MachineID, id, now, group)
	if err != nil {
		return r, err
	}
	if display {
		r.Leases = []runnerauth.RunnerLease{}
	}
	r.ProjectUsed = make(map[tracker.ProjectID]int)
	runnerSlots := make(map[string]bool)
	leaseQuery := `SELECT l.expires_at, coalesce(lr.runner_id, ''), l.lease_id, coalesce(i.project_id, ''), l.session_id, ` + nativeLandingCapacityProject + `
FROM leases l JOIN issues i ON i.id = l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id ` + nativeLandingCapacityJoins + ` WHERE l.machine_id = ? AND l.released_at IS NULL AND julianday(l.renewed_at)<=julianday(?) AND julianday(l.expires_at)>julianday(?) ORDER BY l.fencing_token`
	if display {
		leaseQuery = `SELECT l.expires_at, coalesce(lr.runner_id, ''), l.lease_id, l.fencing_token, coalesce(i.native_id, ''), i.title, coalesce(i.project_id, ''), coalesce(p.metadata_json, ''), coalesce(pp.policy_id, ''), l.session_id, ` + nativeLandingCapacityProject + `
FROM leases l JOIN issues i ON i.id = l.issue_id LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id
LEFT JOIN lease_policies lp ON lp.lease_id = l.lease_id LEFT JOIN policy_revisions p ON p.scope = lp.scope AND p.policy_id = lp.policy_id
LEFT JOIN project_policies pp ON pp.scope = lp.scope ` + nativeLandingCapacityJoins + ` WHERE l.machine_id = ? AND l.released_at IS NULL AND julianday(l.renewed_at)<=julianday(?) AND julianday(l.expires_at)>julianday(?) ORDER BY l.fencing_token`
	}
	rows, err := db.QueryContext(ctx, leaseQuery, r.MachineID, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return r, err
	}
	defer rows.Close()

	type landingUsageRow struct {
		expiry, runner, raw, approved, session, batchProject string
		lease                                                runnerauth.RunnerLease
	}
	var landingRows []landingUsageRow
	for rows.Next() {
		var value landingUsageRow
		destinations := []any{&value.expiry, &value.runner, &value.lease.ID, &value.lease.ProjectID, &value.session, &value.batchProject}
		if display {
			destinations = []any{&value.expiry, &value.runner, &value.lease.ID, &value.lease.FencingToken, &value.lease.WorkItemID, &value.lease.Title, &value.lease.ProjectID, &value.raw, &value.approved, &value.session, &value.batchProject}
		}
		if err := rows.Scan(destinations...); err != nil {
			return r, err
		}
		landingRows = append(landingRows, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return r, err
	}
	if err := rows.Close(); err != nil {
		return r, err
	}
	for _, value := range landingRows {

		expiry, runner, lease, raw, approved, session, batchProject := value.expiry, value.runner, value.lease, value.raw, value.approved, value.session, value.batchProject

		end, err := parseTimeValue(expiry)
		if err != nil {
			return r, err
		}
		batchProject, err = resolveLandingProject(ctx, db, batchProject, false)
		if err != nil {
			return r, err
		}
		if end.After(now) {
			if runner == id {
				key := landingCapacityKey(r.MachineID, batchProject, session)
				if key == "" {
					key = string(lease.ID)
				}
				if !runnerSlots[key] && (group == "" || key != group) {
					r.ProjectUsed[lease.ProjectID]++
					runnerSlots[key] = true
				}
				if !display {
					continue
				}
				lease.ExpiresAt = end
				if raw != "" {
					if err := decodeStorageJSON(ctx, []byte(raw), &lease.Policy); err != nil {
						return r, err
					}
				}
				lease.Exclusions = r.Exclusions(lease.ProjectID, lease.Policy.Requirements, true)
				if lease.Policy.ID == "" || approved == "" {
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
	if len(reports) > 0 {
		snapshot, err := readProviderCapacitySnapshot(ctx, db, organization, now)
		if err != nil {
			return r, err
		}
		for _, report := range reports {
			r.ProviderCapacity = append(r.ProviderCapacity, snapshot.view(id, report, now, group))
		}
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
	MachineUse                  string                                  `json:"machine_use,omitempty"`
	ProjectRanks                map[tracker.ProjectID]int               `json:"project_ranks,omitempty"`
	ProjectConfigurationCommand *runnerauth.ProjectConfigurationCommand `json:"project_configuration_request,omitempty"`
	UpdateRequest               *runnerauth.UpdateRequest               `json:"update_request,omitempty"`
	CapacityRequest             *runnerauth.CapacityRequest             `json:"capacity_request,omitempty"`
	IsolationTier               string                                  `json:"isolation_tier"`
	HostServices                []string                                `json:"host_services"`
	Availability                runnerauth.Availability                 `json:"availability"`
}

func settingsFromRouting(r runnerauth.Routing) runnerSettings {
	return runnerSettings{MachineUse: r.MachineUse, ProjectRanks: r.ProjectRankOverrides, ProjectConfigurationCommand: r.ProjectConfigurationCommand, UpdateRequest: r.UpdateRequest, CapacityRequest: r.CapacityRequest, IsolationTier: r.IsolationTier, HostServices: r.HostServices, Availability: r.Availability}
}

func unmarshalRunnerSettings(raw string, routing *runnerauth.Routing) error {
	var settings runnerSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return err
	}
	routing.ProjectRanks = settings.ProjectRanks
	routing.MachineUse = settings.MachineUse
	routing.ProjectRankOverrides = settings.ProjectRanks
	routing.UpdateRequest = settings.UpdateRequest
	routing.ProjectConfigurationCommand = settings.ProjectConfigurationCommand
	routing.CapacityRequest = settings.CapacityRequest
	routing.IsolationTier = settings.IsolationTier
	routing.HostServices = settings.HostServices
	routing.Availability = settings.Availability
	return nil
}

func readRunnerRoutingSnapshot(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, id string, now time.Time) (runnerauth.RoutingSnapshot, error) {
	runner, _, _, err := scanRunnerIdentity(db.QueryRowContext(ctx, runnerIdentitySelect+" WHERE r.organization_id = ? AND r.id = ?", organization, id), func() time.Time { return now })
	if err == nil {
		err = readRunnerIdentityFacts(ctx, db, &runner, nil)
	}
	if err == nil {
		runner.ProjectRanks, err = readRunnerProjectRanks(ctx, db, runner.RunnerID, runner.OrganizationID)
	}
	snapshot := runnerauth.RoutingSnapshot{RunnerID: id, Revision: runner.Revision, Routing: runner.Routing}
	if err == nil {
		err = db.QueryRowContext(ctx, `SELECT request_id FROM runner_repository_lists WHERE runner_id=? AND pending=1`, id).Scan(&snapshot.RepositoryListRequest)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	}
	return snapshot, err
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

type runnerProjectRankReader interface {
	RunnerProjectSchedulingRanks(context.Context, string, int) (map[tracker.ProjectID]int, error)
}

type runnerProposedRankReader interface {
	RunnerProposedSchedulingRanks(context.Context, runnerstore.Identity, int) (map[tracker.ProjectID]int, error)
}

func (s *Service) readRunnerRouting(ctx context.Context, scope nativeScope, runnerID string) (runnerauth.Runner, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	credential := scope.credential
	if credential.Hosted == nil && (credential.Runner.RunnerID != "" && (credential.Runner.RunnerID != runnerID || credential.Runner.OrganizationID != scope.organization) || credential.Runner.RunnerID == "" && (credential.Scope != apiScopeAdmin || credential.NativeOnly)) {
		return runnerauth.Runner{}, nativeNotFound()
	}

	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return runnerauth.Runner{}, nativeRunEventStorageUnavailable()
	}
	var runner runnerauth.Runner
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		now := s.config.now().UTC()
		if credential.Runner.RunnerID != "" {
			current, err := reader.AccessCredential(ctx, credential.ID)
			if err != nil {
				return err
			}
			identity, err := reader.RunnerIdentityByCredential(ctx, credential.ID)
			if err != nil {
				return err
			}
			if current.OrganizationID != scope.organization || current.Scope != string(apiScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.organization, runnerID, credential.ID, credential.Runner.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Read) {
				return accessstore.ErrDenied
			}
		} else {
			if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
				return err
			}
			current, err := reader.AccessCredential(ctx, credential.ID)
			if err != nil {
				return err
			}
			if credential.Hosted == nil {
				if current.Scope != string(apiScopeAdmin) || current.NativeOnly {
					return accessstore.ErrDenied
				}
			} else {
				member, err := reader.AccessMember(ctx, credential.Hosted.Subject)
				if err != nil {
					return err
				}
				credential.HostedRole = lesserHostedRole(member.Role, credential.HostedRole)
				credential.HostedKeyScope = apikey.Scope(current.KeyScope)
				credential.HostedProjectAccess = hostedProjectAccess(current.ProjectAccess)
			}
		}
		routingReader, ok := reader.(runnerstore.RoutingReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		record, err := routingReader.RunnerRoutingRecord(ctx, runnerID, now)
		if err != nil {
			return err
		}
		urgent, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		runner = runnerFromRoutingRecord(record, now, urgent)
		ranks, ok := reader.(runnerProjectRankReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		runner.ProjectRanks, err = ranks.RunnerProjectSchedulingRanks(ctx, runnerID, runnerstore.MaxRunnerProjectList)
		return err
	})
	if err != nil {
		return runnerauth.Runner{}, projectRecordError(err)
	}

	projects := runnerManagementProjects(runner)
	if credential.Hosted != nil && projects != nil {
		for project := range runner.ProjectRankOverrides {
			if !slices.Contains(projects, project) {
				projects = append(projects, project)
			}
		}
		for project := range runner.ProjectCapacityLimits {
			if !slices.Contains(projects, project) {
				projects = append(projects, project)
			}
		}
		for _, problem := range runner.Problems {
			project := tracker.ProjectID(problem.ProjectID)
			if project != "" && !slices.Contains(projects, project) {
				projects = append(projects, project)
			}
		}
	}
	organizationAdmin := runner.Scope == "organization" && (credential.HostedRole == "owner" || credential.HostedRole == "admin") && (credential.HostedKeyScope == "" || credential.HostedProjectAccess == hostedProjectsAll)
	if credential.Hosted != nil && !organizationAdmin {
		allowed, err := s.appAllRunnerGrants(ctx, credential, projects...)
		if err != nil {
			return runnerauth.Runner{}, err
		}
		if !allowed {
			return runnerauth.Runner{}, nativeNotFound()
		}
	}
	return runner, nil
}

type runnerRoutingRequest struct {
	runnerauth.RoutingChange
	IdempotencyKey string                   `json:"idempotency_key,omitempty"`
	IsolationTier  *string                  `json:"isolation_tier"`
	HostServices   *[]string                `json:"host_services"`
	Availability   *runnerauth.Availability `json:"availability"`
}

// effective preserves optional settings exactly as the dashboard command does.
func (request runnerRoutingRequest) effective(current runnerauth.Routing) runnerauth.RoutingChange {
	change := request.RoutingChange
	if change.MachineUse == "" {
		change.MachineUse = current.MachineUse
	}
	if change.Scope == "" {
		change.Scope = current.Scope
	}
	if change.ProjectCapacityLimits == nil {
		change.ProjectCapacityLimits = current.ProjectCapacityLimits
	}
	change.UpdateRequest = current.UpdateRequest
	change.CapacityRequest = current.CapacityRequest
	change.ProjectConfigurationCommand = current.ProjectConfigurationCommand
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
	if request.Availability == nil || request.Availability.Timezone == "" && request.Availability.Windows == nil && request.Availability.WindowSlots == nil && request.Availability.HardDeadline == "" {
		change.Availability = current.Availability
	} else {
		change.Availability = *request.Availability
	}
	change.Routing = change.Normalized()
	return change
}

func (s *Service) updateRunnerRouting(c echo.Context) error {
	var request runnerRoutingRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if len(request.IdempotencyKey) > 128 || scope.credential.Runner.RunnerID != "" {
		return nil, nativeInvalid("Invalid runner routing command")
	}
	current, err := s.readRunnerRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	operation := "runner_routing " + resource
	var identity runnerstore.Identity
	var receipt eventstore.Receipt
	projects := slices.Clone(runnerManagementProjects(current))
	for project := range current.ProjectCapacityLimits {
		projects = append(projects, project)
	}
	for project := range current.ProjectRankOverrides {
		projects = append(projects, project)
	}
	slices.Sort(projects)
	projects = slices.Compact(projects)
	if len(projects) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	authorizationScope := current.Scope
	authorize := func(reader tenantstore.Reader) error {
		return s.authorizeRunnerRoutingAccess(ctx, reader, scope, projects, authorizationScope)
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := authorize(reader); err != nil {
			return err
		}
		var err error
		identity, err = reader.RunnerIdentity(ctx, resource)
		if err != nil || request.IdempotencyKey == "" {
			return err
		}
		receipt, err = reader.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	if receipt.CommandKey != "" {
		if receipt.RequestHash != hash {
			return nil, nativeConflict(0)
		}
		return s.readRunnerRoutingReceipt(ctx, scope, resource, receipt, content)
	}
	change := request.effective(current.Routing)
	ranksUnchanged := request.ProjectRanks == nil || maps.Equal(request.ProjectRanks, current.ProjectRanks)
	if ranksUnchanged {
		change.ProjectRanks = current.ProjectRankOverrides
	}
	if err := change.Validate(); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	if !maps.Equal(change.ProjectCapacityLimits, current.ProjectCapacityLimits) && strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, nativeInvalid("Project capacity updates require an idempotency key")
	}
	projects = append(projects, change.ProjectIDs...)
	for project := range change.ProjectCapacityLimits {
		if !change.AllowsProject(project) {
			return nil, nativeInvalid("Project capacity limits require existing runner project access")
		}
		projects = append(projects, project)
	}
	for project := range change.ProjectRanks {
		projects = append(projects, project)
	}
	slices.Sort(projects)
	projects = slices.Compact(projects)
	if len(projects) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	if identity.Revision != change.ExpectedRevision || current.Revision != identity.Revision {
		return nil, nativeConflict(tracker.Revision(identity.Revision))
	}
	if change.CapacityLimit != current.CapacityLimit {
		change.CapacityRequest = nil
	}
	if (change.CapacityLimit != current.CapacityLimit || current.CapacityRequiresApplication(change.CapacityLimit)) && change.CapacityLimit > 0 && freshCapacityConfig(current, s.config.now()) {
		change.CapacityRequest = &runnerauth.CapacityRequest{ExpectedConfigRevision: current.CapacityConfig.Revision, Capacity: change.CapacityLimit}
	}
	identity.CapacityRequest = change.CapacityRequest
	identity.Scope, identity.ProjectIDs = change.Scope, change.ProjectIDs
	authorizationScope = identity.Scope
	identity.DisplayName, identity.State, identity.Tags = change.DisplayName, change.State, change.Tags
	identity.CapacityLimit, identity.MachineUse = int64(change.CapacityLimit), change.MachineUse
	identity.IsolationTier, identity.HostServices = change.IsolationTier, change.HostServices
	identity.Availability = runnerstore.Availability(change.Availability)
	identity.ProjectCapacityLimits, identity.ProjectRanks = change.ProjectCapacityLimits, change.ProjectRanks
	var responseRanks map[tracker.ProjectID]int
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := authorize(reader); err != nil {
			return err
		}

		ranks, ok := reader.(runnerProposedRankReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		responseRanks, err = ranks.RunnerProposedSchedulingRanks(ctx, identity, runnerstore.MaxRunnerProjectList)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	previousRouting := current.Routing
	current.Routing, current.Revision = change.Routing, identity.Revision+1
	current.ProjectRanks = responseRanks
	raw, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	if request.IdempotencyKey != "" {
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
		if err != nil {
			return nil, err
		}
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := authorize(writer); err != nil {
			return err
		}
		if request.IdempotencyKey != "" {
			previous, err := writer.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				receipt = previous
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if change.Scope != previousRouting.Scope || !slices.Equal(change.ProjectIDs, previousRouting.ProjectIDs) {
			enrollment, err := writer.RunnerEnrollment(ctx, identity.EnrollmentID)
			if err != nil {
				return err
			}
			owned := enrollment.CreatedBy == scope.credential.ID
			if !owned && scope.credential.Hosted != nil {
				creator, err := writer.AccessCredential(ctx, enrollment.CreatedBy)
				if err != nil {
					return err
				}
				owned = creator.UserID == scope.credential.Hosted.Subject
			}
			if !owned && previousRouting.Scope == "projects" && change.Scope == "organization" && canManageProjectSecrets(scope.credential) {
				administrator := scope
				administrator.requireHostedAdmin = true
				owned = s.authorizeAccessScope(ctx, writer, administrator, true) == nil
			}
			if !owned {
				return accessstore.ErrDenied
			}
			for _, project := range previousRouting.ProjectIDs {
				if err := writer.DeleteAccessTokenGrant(ctx, identity.TokenID, project); err != nil {
					return err
				}
			}
			for _, project := range change.ProjectIDs {
				if err := writer.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: identity.TokenID, ProjectID: project}); err != nil {
					return err
				}
			}
		}
		if err := writer.UpdateRunnerRouting(ctx, change.ExpectedRevision, identity); err != nil {
			return err
		}
		if request.IdempotencyKey != "" {
			var err error
			receipt, err = writer.PutReceipt(ctx, receipt)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if request.IdempotencyKey != "" {
		return s.readRunnerRoutingReceipt(ctx, scope, resource, receipt, content)
	}
	return json.RawMessage(raw), nil
}

func (s *Service) updateRunnerHost(c echo.Context) error {
	var change runnerauth.HostChange
	if err := decodeAPIJSON(c, &change); err != nil {
		return s.invalidAPIRequest(c, err)
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
		return nil, nativePublicInvalid("Host name and a capacity between 0 and 10000 are required")
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.requireRunnerAdministrationTenant(ctx, w, scope); err != nil {
			return err
		}
		revision, err := w.UpdateRunnerHost(ctx, resource, change)
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nativeConflict(tracker.Revision(revision))
		}
		if err != nil {
			return err
		}
		change.ExpectedRevision = revision
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.notifications != nil {
		s.notifications.notify(dispatchNotificationKey(scope.organization))
	}
	return change, nil
}

func (s *Service) requireRunnerAdministrationTenant(ctx context.Context, reader tenantstore.Reader, scope nativeScope) error {
	credential := scope.credential
	if credential.Runner.RunnerID != "" || credential.Hosted == nil && (credential.NativeOnly || credential.Scope != apiScopeAdmin) {
		return accessstore.ErrDenied
	}
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
		return err
	}
	if credential.Hosted == nil {
		return nil
	}
	if !credential.Hosted.ExpiresAt.IsZero() && !credential.Hosted.ExpiresAt.After(s.config.now()) || credential.Hosted.SupportActor != "" {
		return accessstore.ErrDenied
	}
	if credential.EntryKey != nil && !credential.EntryKey.Valid() {
		return accessstore.ErrDenied
	}
	if credential.EntryKey == nil || credential.EntryKey.Kind != "service" {
		member, err := reader.AccessMember(ctx, credential.Hosted.Subject)
		if err != nil {
			return err
		}
		credential.HostedRole = lesserHostedRole(member.Role, credential.HostedRole)
	}
	allowed, err := s.appAllRunnerGrantsWith(ctx, reader, credential, nil)
	if err != nil {
		return err
	}
	if !allowed {
		return accessstore.ErrDenied
	}
	return nil
}

func runnerFromRoutingRecord(record runnerstore.RoutingRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	runner := runnerFromFleetRecord(record.Record, now, urgent)
	runner.ProviderCapacity = record.Providers
	runner.ProjectUsed = map[tracker.ProjectID]int{}
	runner.Leases = []runnerauth.RunnerLease{}
	hostSlots, runnerSlots := map[string]bool{}, map[string]bool{}
	for _, lease := range record.Leases {
		if lease.MachineID != runner.MachineID {
			continue
		}
		key := string(lease.Lease.ID)
		if lease.LandingCapacity > 0 {
			if group := landingCapacityKey(lease.MachineID, string(lease.Lease.ProjectID), lease.SessionID); group != "" {
				key = group
			}
		}
		hostSlots[key] = true
		if lease.RunnerID != runner.RunnerID {
			continue
		}
		if !runnerSlots[key] {
			runner.ProjectUsed[lease.Lease.ProjectID]++
		}
		runnerSlots[key] = true
		lease.Lease.Policy.ID = lease.PolicyID
		if lease.PolicyID == "" || lease.CurrentPolicyID == "" {
			lease.Lease.Exclusions = append(lease.Lease.Exclusions, runnerauth.Exclusion{Code: "policy_mismatch", Message: "This run's pinned policy is missing or revoked"})
		}
		lease.Lease.ProviderReservation = lease.Reservation
		runner.Leases = append(runner.Leases, lease.Lease)
		appendRunnerRuntimeProblems(&runner, lease, now)
	}
	runner.HostUsed, runner.Used = len(hostSlots), len(runnerSlots)
	return runner
}

func (s *Service) listRunnerRouting(c echo.Context) error {
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Page limit is invalid"))
	}
	offset := 0
	if value := c.QueryParam("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 100000 {
			return s.nativeAPIError(c, nativePublicInvalid("Page offset is invalid"))
		}
	}
	result, err := s.listRunnerRoutingData(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), limit+1, offset)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(result) > limit {
		if c.QueryParam("limit") == "" {
			return s.nativeAPIError(c, nativePublicInvalid("Fleet requires pagination; specify limit and follow X-Next-Offset"))
		}
		result = result[:limit]
		c.Response().Header().Set("X-Next-Offset", strconv.Itoa(offset+limit))
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) listRunnerRoutingData(ctx context.Context, organization tracker.OrganizationID, limit, offset int) ([]runnerauth.Runner, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, nativeRunEventStorageUnavailable()
	}
	result := []runnerauth.Runner{}
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		fleet, ok := reader.(runnerstore.RoutingPageReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		now := s.config.now()
		records, err := fleet.RunnerRoutingRecords(ctx, limit, offset, now)
		if err != nil {
			return err
		}
		urgent, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		for _, record := range records {
			result = append(result, runnerFromRoutingRecord(record, now, urgent))
		}
		return nil
	})
	return result, projectRecordError(err)
}

func runnerExcluded(exclusions []runnerauth.Exclusion) error {
	if len(exclusions) == 0 {
		return nil
	}
	return &nativeError{Code: exclusions[0].Code, Message: exclusions[0].Message, status: http.StatusConflict}
}

const runnerAuthoritySelect = `SELECT t.token_hash,t.created_at,t.expires_at,t.revoked_at,r.state,
 r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.project_id=?)
 FROM runner_identities r JOIN api_tokens t ON t.id=r.token_id
 WHERE r.organization_id=? AND r.id=? AND r.token_id=?`

func requireRunnerAuthority(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	if scope.credential.Runner.RunnerID == "" {
		return requireCredentialAuthority(ctx, tx, scope.credential, now)
	}
	if err := readRunnerAuthority(ctx, tx, scope, now); err != nil {
		return err
	}
	return authorizeNativeProject(ctx, tx, scope)
}

func readRunnerAuthority(ctx context.Context, query nativeQueryer, scope nativeScope, now time.Time) error {
	if scope.credential.EntryKey != nil {
		return runnerUnauthorized()
	}
	var hash, created, state string
	var expires, revoked sql.NullString
	var allowed bool
	err := query.QueryRowContext(ctx, runnerAuthoritySelect, scope.project, scope.organization, scope.credential.Runner.RunnerID, scope.credential.ID).Scan(&hash, &created, &expires, &revoked, &state, &allowed)
	if err != nil {
		return err
	}
	if hash != scope.credential.Hash || revoked.Valid || !expires.Valid || !runnerTimeValid(now, created, expires.String) {
		return runnerUnauthorized()
	}
	if scope.project == "" || !allowed {
		return nativeNotFound()
	}
	if state == "disabled" {
		return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
	}
	return nil
}

func requireCredentialAuthority(ctx context.Context, tx *sql.Tx, credential apiCredential, now time.Time) error {
	if credential.EntryKey != nil {
		if !credential.EntryKey.Valid() || credential.Runner.RunnerID != "" || credential.Hosted == nil || !credential.Hosted.ExpiresAt.After(now) {
			return runnerUnauthorized()
		}
		return nil
	}
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
		return nativeNotFoundAt("lease_runner_credential")
	}
	return nil
}

func validateRunnerDispatch(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time, joining ...string) error {
	r, err := readRunnerForDispatch(ctx, tx, scope, now, joining...)
	if err != nil {
		return err
	}
	return runnerExcluded(r.Exclusions(scope.project, policy.Requirements{}, false))
}

func readRunnerForDispatch(ctx context.Context, tx nativeQueryer, scope nativeScope, now time.Time, joining ...string) (runnerauth.Runner, error) {
	return readRunnerFactsWithClock(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, func() time.Time { return now }, false, joining...)
}

func (s *Service) validateRunnerLease(c echo.Context) error {
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	runner, err := s.validateRunnerLeaseTenant(c.Request().Context(), nativeRequestScope(c), tracker.LeaseID(c.Param("lease")), request.FencingToken)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, runner)
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
	if err := requireLeaseRouting(ctx, tx, id); err != nil {
		return runnerauth.Runner{}, err
	}
	if scope.credential.Runner.RunnerID == "" {
		return runnerauth.Runner{Binding: runnerauth.Binding{MachineID: lease.session.Machine.ID}}, nil
	}
	r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if err := runnerExcluded(r.Exclusions(scope.project, policy.Requirements{}, true)); err != nil {
		return runnerauth.Runner{}, err
	}
	return r, nil
}

func readRunnerProjectRanks(ctx context.Context, db nativeQueryer, runnerID string, organization tracker.OrganizationID) (map[tracker.ProjectID]int, error) {
	var legacy sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT CASE WHEN routing_facts_ready IS NULL THEN routing_settings_json END FROM runner_identities WHERE id=? AND organization_id=?`, runnerID, organization).Scan(&legacy); err != nil {
		return nil, err
	}
	var settings runnerSettings
	if legacy.Valid {
		if err := decodeStorageJSON(ctx, []byte(legacy.String), &settings); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT p.id, COALESCE(rank.rank, p.scheduling_rank) FROM projects p JOIN runner_identities r ON r.organization_id=p.organization_id LEFT JOIN runner_project_ranks rank ON rank.runner_id=r.id AND rank.project_id=p.id AND r.routing_facts_ready IS NOT NULL WHERE r.id=? AND p.organization_id=? AND (r.scope='organization' AND p.deleted_at IS NULL OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.project_id=p.id AND g.organization_id=p.organization_id))", runnerID, organization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ranks := map[tracker.ProjectID]int{}
	for rows.Next() {
		var id tracker.ProjectID
		var rank int
		if err := rows.Scan(&id, &rank); err != nil {
			return nil, err
		}
		if legacy.Valid {
			if override, ok := settings.ProjectRanks[id]; ok {
				rank = override
			}
		}
		ranks[id] = rank
	}
	return ranks, rows.Err()
}

func runnerOwnedBy(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID, runner string, credential apiCredential) (bool, error) {
	user := ""
	if credential.Hosted != nil {
		user = credential.Hosted.Subject
	}
	var owned bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runner_identities r
 JOIN runner_enrollments e ON e.id = r.enrollment_id JOIN api_tokens t ON t.id = e.created_by
 LEFT JOIN hosted_members m ON m.principal_id = e.created_by
 WHERE r.id = ? AND r.organization_id = ? AND (e.created_by = ? OR (? <> '' AND (t.hosted_user_id = ? OR m.user_id = ?))))`, runner, organization, credential.ID, user, user, user).Scan(&owned)
	return owned, err
}

const releasedExecutionsPageSize = 100

type releasedExecutionCursorKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	runner       string
}

func (s *Service) readRunnerClaimState(ctx context.Context, db nativeQueryer, scope nativeScope, snapshot *runnerauth.RoutingSnapshot, now time.Time) error {
	var state runnerauth.ClaimState
	if err := db.QueryRowContext(ctx, `SELECT m.capacity, min(r.capacity_limit, r.reported_capacity) FROM runner_identities r JOIN machines m ON m.id = r.machine_id WHERE r.id = ? AND r.organization_id = ?`, snapshot.RunnerID, scope.organization).Scan(&state.HostCapacity, &state.RunnerCapacity); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT l.lease_id, coalesce(lr.runner_id, ''), l.expires_at FROM leases l LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id WHERE l.machine_id = ? AND l.released_at IS NULL ORDER BY l.lease_id`, scope.credential.Runner.MachineID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slot runnerauth.ClaimSlot
		var expiry string
		if err := rows.Scan(&slot.ID, &slot.RunnerID, &expiry); err != nil {
			return errors.Join(err, rows.Close())
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return errors.Join(err, rows.Close())
		}
		if end.After(now) {
			state.Slots = append(state.Slots, slot)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var approved string
	err = db.QueryRowContext(ctx, "SELECT policy_id FROM project_policies WHERE scope = ?", string(scope.organization)+"/"+string(scope.project)).Scan(&approved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	state.PolicyIDs = map[tracker.ProjectID]string{scope.project: approved}

	key := releasedExecutionCursorKey{organization: scope.organization, project: scope.project, runner: snapshot.RunnerID}
	position, _ := s.cleanupPollCursors.Load(key)
	after, valid := position.(string)
	if !valid {
		after = ""
	}
	released, err := db.QueryContext(ctx, `SELECT a.work_item_id,a.id FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id
 WHERE a.organization_id=? AND a.project_id=? AND a.runner_id=? AND l.machine_id=? AND l.released_at IS NOT NULL
 AND a.worker_cleanup_required=1 AND a.cleanup_acknowledged_at IS NULL AND a.status IN ('running','interrupted','cancelled') AND a.execution_role<>''
 AND a.id>? ORDER BY a.id LIMIT ?`, scope.organization, scope.project, snapshot.RunnerID, scope.credential.Runner.MachineID, after, releasedExecutionsPageSize)
	if err != nil {
		return err
	}
	defer released.Close()
	for released.Next() {
		reference := runnerauth.ReleasedExecution{ProjectID: scope.project}
		if err := released.Scan(&reference.WorkItemID, &reference.AttemptID); err != nil {
			return errors.Join(err, released.Close())
		}
		state.ReleasedExecutions = append(state.ReleasedExecutions, reference)
	}
	if err := errors.Join(released.Err(), released.Close()); err != nil {
		return err
	}
	if len(state.ReleasedExecutions) == releasedExecutionsPageSize {
		s.cleanupPollCursors.Store(key, state.ReleasedExecutions[len(state.ReleasedExecutions)-1].AttemptID)
	} else {
		s.cleanupPollCursors.Delete(key)
	}

	snapshot.ClaimState = &state
	return nil
}
