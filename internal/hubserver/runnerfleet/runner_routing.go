package runnerfleet

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
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RoutingDeps struct {
	LandingCapacityKey  func(tracker.MachineID, string, string) string
	Repository          tenantstore.Repository
	Now                 func() time.Time
	HTTPError           func(echo.Context, error, ...any) error
	InvalidRequest      func(echo.Context, error) error
	Details             func() attachment.DetailStore
	NotifyDispatch      func(tracker.OrganizationID)
	AuthorizeAccess     func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	ManagementProjects  func(runnerauth.Runner) []tracker.ProjectID
	FleetRecord         func(runnerstore.FleetRecord, time.Time, runnerstore.UrgentUpdate) runnerauth.Runner
	AllRunnerGrants     func(context.Context, hubapi.APICredential, ...tracker.ProjectID) (bool, error)
	AllRunnerGrantsWith func(context.Context, tenantstore.Reader, hubapi.APICredential, []tracker.ProjectID) (bool, error)
	AuthorizeProject    func(context.Context, hubscope.NativeScope, bool) error
}

func (d RoutingDeps) GetRunnerRouting(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	r, err := d.ReadRunnerRouting(c.Request().Context(), hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("runner"))
	if err != nil {
		return d.HTTPError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, r)
}

type RunnerProjectRankReader interface {
	RunnerProjectSchedulingRanks(context.Context, string, int) (map[tracker.ProjectID]int, error)
}

type runnerProposedRankReader interface {
	RunnerProposedSchedulingRanks(context.Context, runnerstore.Identity, int) (map[tracker.ProjectID]int, error)
}

func (d RoutingDeps) ReadRunnerRouting(ctx context.Context, scope hubscope.NativeScope, runnerID string) (runnerauth.Runner, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	credential := scope.Credential
	if credential.Hosted == nil && (credential.Runner.RunnerID != "" && (credential.Runner.RunnerID != runnerID || credential.Runner.OrganizationID != scope.Organization) || credential.Runner.RunnerID == "" && (credential.Scope != hubapi.APIScopeAdmin || credential.NativeOnly)) {
		return runnerauth.Runner{}, hubapi.NativeNotFound()
	}

	repository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return runnerauth.Runner{}, hubapi.NativeRunEventStorageUnavailable()
	}
	var runner runnerauth.Runner
	var pinned map[RoutingPolicyKey]policystore.Revision
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		now := d.Now().UTC()
		if credential.Runner.RunnerID != "" {
			current, err := reader.AccessCredential(ctx, credential.ID)
			if err != nil {
				return err
			}
			identity, err := reader.RunnerIdentityByCredential(ctx, credential.ID)
			if err != nil {
				return err
			}
			if current.OrganizationID != scope.Organization || current.Scope != string(hubapi.APIScopeWorker) || current.Kind != "service" || !current.Valid(credential.Hash, now) || !RunnerIdentityMatchesHeldLease(identity, scope.Organization, runnerID, credential.ID, credential.Runner.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Read) {
				return accessstore.ErrDenied
			}
		} else {
			if err := d.AuthorizeAccess(ctx, reader, scope, false); err != nil {
				return err
			}
			current, err := reader.AccessCredential(ctx, credential.ID)
			if err != nil {
				return err
			}
			if credential.Hosted == nil {
				if current.Scope != string(hubapi.APIScopeAdmin) || current.NativeOnly {
					return accessstore.ErrDenied
				}
			} else {
				member, err := reader.AccessMember(ctx, credential.Hosted.Subject)
				if err != nil {
					return err
				}
				credential.HostedRole = hubapi.LesserHostedRole(member.Role, credential.HostedRole)
				credential.HostedKeyScope = apikey.Scope(current.KeyScope)
				credential.HostedProjectAccess = hubapi.HostedProjectAccess(current.ProjectAccess)
			}
		}
		routingReader, ok := reader.(runnerstore.RoutingReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		record, err := routingReader.RunnerRoutingRecord(ctx, runnerID, now)
		if err != nil {
			return err
		}
		urgent, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		runner = d.FromRoutingRecord(record, now, urgent)
		if pinned, err = ReadRoutingLeasePolicies(ctx, reader, runner); err != nil {
			return err
		}
		ranks, ok := reader.(RunnerProjectRankReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		runner.ProjectRanks, err = ranks.RunnerProjectSchedulingRanks(ctx, runnerID, runnerstore.MaxRunnerProjectList)
		return err
	})
	if err != nil {
		return runnerauth.Runner{}, hubapi.ProjectRecordError(err)
	}
	if err := d.PinRoutingLeasePolicies(ctx, pinned, runner); err != nil {
		return runnerauth.Runner{}, err
	}

	projects := d.ManagementProjects(runner)
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
	organizationAdmin := runner.Scope == "organization" && (credential.HostedRole == "owner" || credential.HostedRole == "admin") && (credential.HostedKeyScope == "" || credential.HostedProjectAccess == hubapi.HostedProjectsAll)
	if credential.Hosted != nil && !organizationAdmin {
		allowed, err := d.AllRunnerGrants(ctx, credential, projects...)
		if err != nil {
			return runnerauth.Runner{}, err
		}
		if !allowed {
			return runnerauth.Runner{}, hubapi.NativeNotFound()
		}
	}
	return runner, nil
}

type RunnerRoutingRequest struct {
	runnerauth.RoutingChange
	IdempotencyKey string                   `json:"idempotency_key,omitempty"`
	IsolationTier  *string                  `json:"isolation_tier"`
	HostServices   *[]string                `json:"host_services"`
	Availability   *runnerauth.Availability `json:"availability"`
}

// effective preserves optional settings exactly as the dashboard command does.

func (request RunnerRoutingRequest) Effective(current runnerauth.Routing) runnerauth.RoutingChange {
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

func (d RoutingDeps) UpdateRunnerRouting(c echo.Context) error {
	var request RunnerRoutingRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return d.InvalidRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return d.HTTPError(c, hubscope.RunnerUnauthorized())
	}
	value, err := d.UpdateRunnerRoutingCommand(c.Request().Context(), hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("runner"), request)
	if err != nil {
		return d.HTTPError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (d RoutingDeps) UpdateRunnerRoutingCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request RunnerRoutingRequest) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if len(request.IdempotencyKey) > 128 || scope.Credential.Runner.RunnerID != "" {
		return nil, hubapi.NativeInvalid("Invalid runner routing command")
	}
	current, err := d.ReadRunnerRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	operation := "runner_routing " + resource
	var identity runnerstore.Identity
	var receipt eventstore.Receipt
	projects := slices.Clone(d.ManagementProjects(current))
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
		return d.authorizeRunnerRoutingAccess(ctx, reader, scope, projects, authorizationScope)
	}
	err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := authorize(reader); err != nil {
			return err
		}
		var err error
		identity, err = reader.RunnerIdentity(ctx, resource)
		if err != nil || request.IdempotencyKey == "" {
			return err
		}
		receipt, err = reader.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	content := eventstore.NewContent(d.Details())
	if receipt.CommandKey != "" {
		if receipt.RequestHash != hash {
			return nil, hubapi.NativeConflict(0)
		}
		return d.readRunnerRoutingReceipt(ctx, scope, resource, receipt, content)
	}
	change := request.Effective(current.Routing)
	ranksUnchanged := request.ProjectRanks == nil || maps.Equal(request.ProjectRanks, current.ProjectRanks)
	if ranksUnchanged {
		change.ProjectRanks = current.ProjectRankOverrides
	}
	if err := change.Validate(); err != nil {
		return nil, hubapi.NativeInvalid(err.Error())
	}
	if !maps.Equal(change.ProjectCapacityLimits, current.ProjectCapacityLimits) && strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, hubapi.NativeInvalid("Project capacity updates require an idempotency key")
	}
	authorizedProjects := slices.Clone(projects)
	projects = append(projects, change.ProjectIDs...)
	for project := range change.ProjectCapacityLimits {
		if !change.AllowsProject(project) {
			return nil, hubapi.NativeInvalid("Project capacity limits require existing runner project access")
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
		return nil, hubapi.NativeConflict(tracker.Revision(identity.Revision))
	}
	if change.CapacityLimit != current.CapacityLimit {
		change.CapacityRequest = nil
	}
	if (change.CapacityLimit != current.CapacityLimit || current.CapacityRequiresApplication(change.CapacityLimit)) && change.CapacityLimit > 0 && FreshCapacityConfig(current, d.Now()) {
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
	err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		for _, project := range projects {
			if slices.Contains(authorizedProjects, project) {
				continue
			}
			row, err := reader.Project(ctx, project)
			if errors.Is(err, corestore.ErrNotFound) || err == nil && row.DeletedAt != nil {
				return hubapi.NativePublicInvalid("Runner routing names a project that does not exist")
			}
			if err != nil {
				return err
			}
		}
		if err := authorize(reader); err != nil {
			return err
		}

		ranks, ok := reader.(runnerProposedRankReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		responseRanks, err = ranks.RunnerProposedSchedulingRanks(ctx, identity, runnerstore.MaxRunnerProjectList)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
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
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: d.Now()}, raw)
		if err != nil {
			return nil, err
		}
	}
	wroteRouting := false
	err = d.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		wroteRouting = false
		if err := authorize(writer); err != nil {
			return err
		}
		if request.IdempotencyKey != "" {
			previous, err := writer.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return hubapi.NativeConflict(0)
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
			owned := enrollment.CreatedBy == scope.Credential.ID
			if !owned && scope.Credential.Hosted != nil {
				creator, err := writer.AccessCredential(ctx, enrollment.CreatedBy)
				if err != nil {
					return err
				}
				owned = creator.UserID == scope.Credential.Hosted.Subject
			}
			if !owned && previousRouting.Scope == "projects" && change.Scope == "organization" && hubscope.CanManageProjectSecrets(scope.Credential) {
				administrator := scope
				administrator.RequireHostedAdmin = true
				owned = d.AuthorizeAccess(ctx, writer, administrator, true) == nil
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
				if err := writer.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: scope.Organization, TokenID: identity.TokenID, ProjectID: project}); err != nil {
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
		return nil, hubapi.ProjectRecordError(err)
	}
	if wroteRouting && dispatchChanged {
		d.NotifyDispatch(scope.Organization)
	}
	if request.IdempotencyKey != "" {
		return d.readRunnerRoutingReceipt(ctx, scope, resource, receipt, content)
	}
	return json.RawMessage(raw), nil
}

func (d RoutingDeps) UpdateRunnerHost(c echo.Context) error {
	var change runnerauth.HostChange
	if err := hubapi.DecodeAPIJSON(c, &change); err != nil {
		return d.InvalidRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return d.HTTPError(c, hubscope.RunnerUnauthorized())
	}
	value, err := d.UpdateRunnerHostCommand(c.Request().Context(), hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("machine"), change)
	if err != nil {
		return d.HTTPError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (d RoutingDeps) UpdateRunnerHostCommand(ctx context.Context, scope hubscope.NativeScope, resource string, change runnerauth.HostChange) (any, error) {
	change.DisplayName = strings.TrimSpace(change.DisplayName)
	if change.DisplayName == "" || len(change.DisplayName) > 200 || strings.ContainsAny(change.DisplayName, "\r\n\x00") || change.Capacity < 0 || change.Capacity > 10000 {
		return nil, hubapi.NativePublicInvalid("Host name and a capacity between 0 and 10000 are required")
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := d.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := d.RequireRunnerAdministrationTenant(ctx, w, scope); err != nil {
			return err
		}
		revision, err := w.UpdateRunnerHost(ctx, resource, change)
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return hubapi.NativeConflict(tracker.Revision(revision))
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
	if d.NotifyDispatch != nil {
		d.NotifyDispatch(scope.Organization)
	}
	return change, nil
}

func (d RoutingDeps) RequireRunnerAdministrationTenant(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope) error {
	credential := scope.Credential
	if credential.Runner.RunnerID != "" || credential.Hosted == nil && (credential.NativeOnly || credential.Scope != hubapi.APIScopeAdmin) {
		return accessstore.ErrDenied
	}
	if err := d.AuthorizeAccess(ctx, reader, scope, true); err != nil {
		return err
	}
	if credential.Hosted == nil {
		return nil
	}
	if !credential.Hosted.ExpiresAt.IsZero() && !credential.Hosted.ExpiresAt.After(d.Now()) || credential.Hosted.SupportActor != "" {
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
		credential.HostedRole = hubapi.LesserHostedRole(member.Role, credential.HostedRole)
	}
	allowed, err := d.AllRunnerGrantsWith(ctx, reader, credential, nil)
	if err != nil {
		return err
	}
	if !allowed {
		return accessstore.ErrDenied
	}
	return nil
}

func (d RoutingDeps) FromRoutingRecord(record runnerstore.RoutingRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	runner := d.FleetRecord(record.Record, now, urgent)
	runner.ProviderCapacity = record.Providers
	runner.ProjectUsed = map[tracker.ProjectID]int{}
	runner.Leases = []runnerauth.RunnerLease{}
	hostSlots, runnerSlots := map[string]bool{}, map[string]bool{}
	for _, lease := range record.Leases {
		if lease.MachineID != runner.MachineID {
			continue
		}
		key, project := string(lease.Lease.ID), lease.CapacityProject
		if lease.LandingCapacity > 0 {
			project = string(lease.Lease.ProjectID)
		}
		if group := d.LandingCapacityKey(lease.MachineID, project, lease.SessionID); group != "" {
			key = group
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
		AppendRuntimeProblems(&runner, lease)
	}
	runner.HostUsed, runner.Used = len(hostSlots), len(runnerSlots)
	return runner
}

func (d RoutingDeps) ListRunnerRouting(c echo.Context) error {
	limit, err := hubapi.ParsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return d.HTTPError(c, hubapi.NativePublicInvalid("Page limit is invalid"))
	}
	offset := 0
	if value := c.QueryParam("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 || offset > 100000 {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Page offset is invalid"))
		}
	}
	result, err := d.ListRunnerRoutingData(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), limit+1, offset)
	if err != nil {
		return d.HTTPError(c, err)
	}
	if len(result) > limit {
		if c.QueryParam("limit") == "" {
			return d.HTTPError(c, hubapi.NativePublicInvalid("Fleet requires pagination; specify limit and follow X-Next-Offset"))
		}
		result = result[:limit]
		c.Response().Header().Set("X-Next-Offset", strconv.Itoa(offset+limit))
	}
	return c.JSON(http.StatusOK, result)
}

func (d RoutingDeps) ListRunnerRoutingData(ctx context.Context, organization tracker.OrganizationID, limit, offset int) ([]runnerauth.Runner, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	repository, ok := d.Repository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	result := []runnerauth.Runner{}
	var pinned map[RoutingPolicyKey]policystore.Revision
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		fleet, ok := reader.(runnerstore.RoutingPageReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		now := d.Now()
		records, err := fleet.RunnerRoutingRecords(ctx, limit, offset, now)
		if err != nil {
			return err
		}
		urgent, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		ranks, ok := reader.(RunnerProjectRankReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		for _, record := range records {
			runner := d.FromRoutingRecord(record, now, urgent)
			if runner.ProjectRanks, err = ranks.RunnerProjectSchedulingRanks(ctx, runner.RunnerID, runnerstore.MaxRunnerProjectList); err != nil {
				return err
			}
			result = append(result, runner)
		}
		pinned, err = ReadRoutingLeasePolicies(ctx, reader, result...)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	return result, d.PinRoutingLeasePolicies(ctx, pinned, result...)
}

type RoutingPolicyKey struct {
	project tracker.ProjectID
	id      string
}

func ReadRoutingLeasePolicies(ctx context.Context, reader policystore.Reader, runners ...runnerauth.Runner) (map[RoutingPolicyKey]policystore.Revision, error) {
	rows := map[RoutingPolicyKey]policystore.Revision{}
	for _, runner := range runners {
		for _, lease := range runner.Leases {
			key := RoutingPolicyKey{project: lease.ProjectID, id: lease.Policy.ID}
			if key.id == "" {
				continue
			}
			if _, found := rows[key]; found {
				continue
			}
			row, err := reader.ProjectPolicyRevision(ctx, key.project, key.id)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			rows[key] = row
		}
	}
	return rows, nil
}

func (d RoutingDeps) PinRoutingLeasePolicies(ctx context.Context, rows map[RoutingPolicyKey]policystore.Revision, runners ...runnerauth.Runner) error {
	if len(rows) == 0 {
		return nil
	}
	content := eventstore.NewContent(d.Details())
	descriptors := make(map[RoutingPolicyKey]policy.Descriptor, len(rows))
	for key, row := range rows {
		descriptor, err := content.ProjectPolicy(ctx, row)
		if err != nil {
			return err
		}
		if descriptor.ID != key.id {
			return artifact.ErrIntegrity
		}
		descriptors[key] = descriptor
	}
	for _, runner := range runners {
		for i, lease := range runner.Leases {
			if descriptor, found := descriptors[RoutingPolicyKey{project: lease.ProjectID, id: lease.Policy.ID}]; found {
				runner.Leases[i].Policy = descriptor
			}
		}
	}
	return nil
}

func RunnerExcluded(exclusions []runnerauth.Exclusion) error {
	if len(exclusions) == 0 {
		return nil
	}
	return &hubapi.NativeError{Code: exclusions[0].Code, Message: exclusions[0].Message, Status: http.StatusConflict}
}

func (d RoutingDeps) ValidateRunnerLease(c echo.Context) error {
	var request tracker.NativeLeaseMutation
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return d.InvalidRequest(c, err)
	}
	runner, err := d.ValidateRunnerLeaseTenant(c.Request().Context(), hubscope.NativeRequestScope(c), tracker.LeaseID(c.Param("lease")), request.FencingToken)
	if err != nil {
		return d.HTTPError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, runner)
}
