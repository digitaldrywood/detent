package hubserver

import (
	"context"
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
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	dispatchChanged := previousRouting.Scope != change.Scope || !slices.Equal(previousRouting.ProjectIDs, change.ProjectIDs) || previousRouting.State != change.State || !slices.Equal(previousRouting.Tags, change.Tags) || previousRouting.CapacityLimit != change.CapacityLimit || !maps.Equal(previousRouting.ProjectCapacityLimits, change.ProjectCapacityLimits) || !maps.Equal(current.ProjectRankOverrides, change.ProjectRanks) || previousRouting.MachineUse != change.MachineUse || previousRouting.IsolationTier != change.IsolationTier || !slices.Equal(previousRouting.HostServices, change.HostServices) || previousRouting.Availability.Timezone != change.Availability.Timezone || !slices.Equal(previousRouting.Availability.Windows, change.Availability.Windows) || !maps.Equal(previousRouting.Availability.WindowSlots, change.Availability.WindowSlots) || previousRouting.Availability.HardDeadline != change.Availability.HardDeadline
	if previousRouting.CapacityRequest == nil || change.CapacityRequest == nil {
		dispatchChanged = dispatchChanged || (previousRouting.CapacityRequest == nil) != (change.CapacityRequest == nil)
	} else {
		dispatchChanged = dispatchChanged || *previousRouting.CapacityRequest != *change.CapacityRequest
	}
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
	wroteRouting := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		wroteRouting = false
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
			granted := slices.Clone(change.ProjectIDs)
			slices.Sort(granted)
			for _, project := range slices.Compact(granted) {
				if err := writer.LockWorkItemProject(ctx, project); err != nil {
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
		wroteRouting = true
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
	if wroteRouting && dispatchChanged {
		s.notifications.notify(dispatchNotificationKey(scope.organization))
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

const releasedExecutionsPageSize = 100

type releasedExecutionCursorKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	runner       string
}
