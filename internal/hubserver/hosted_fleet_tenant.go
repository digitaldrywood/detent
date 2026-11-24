package hubserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHostedRunnerNamesTenant(ctx context.Context, includeActive bool) (map[string]hostedRunnerName, error) {
	names := map[string]hostedRunnerName{}
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(runnerstore.FleetReader)
		if !ok {
			return errors.New("runner fleet reader unavailable")
		}
		rows, err := reader.FleetNames(ctx, 10000)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if includeActive || row.Removed {
				names[row.RunnerID] = hostedRunnerName{DisplayName: row.DisplayName, Hostname: row.Hostname}
			}
		}
		return nil
	})
	return names, err
}

func (s *Service) readHostedFleetTenant(ctx context.Context, credential hubapi.APICredential) (hostedFleetResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	projects, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	now := s.config.Clock().UTC()
	result, _, err := s.readHostedFleetSnapshotTenant(ctx, credential, projects, "", now)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	if s.database.plansService().Plans() != nil {
		entitlement, readErr := hubapi.FeatureResult(s.database.plansService().Entitlement(ctx, nil, now))
		if readErr != nil {
			return hostedFleetResponse{}, readErr
		}
		if !slices.Contains(entitlement.Features, "native_execution") {
			for index := range result.Runners {
				if capacity := result.Runners[index].Capacity; capacity != nil {
					zero := 0
					capacity.Limits = append(capacity.Limits, runnerauth.CapacityLimit{Owner: "plan", Limit: &zero, Constraint: "The current plan does not permit native execution.", ObservedAt: now})
					capacity.Effective = &zero
					capacity.Status = "partially_applied"
					for project, projectCapacity := range capacity.Projects {
						projectCapacity.Effective = &zero
						capacity.Projects[project] = projectCapacity
					}
				}
			}
		}
	}
	result.Usage, err = s.hostedFleetUsage(ctx)
	return result, err
}

func (s *Service) readHostedFleetSnapshotTenant(ctx context.Context, credential hubapi.APICredential, projects []appBootstrapProject, selectedProject tracker.ProjectID, now time.Time) (hostedFleetResponse, []scopedProviderAccount, error) {
	visible := map[tracker.ProjectID]bool{}
	manageable := map[tracker.ProjectID]bool{}
	names := map[tracker.ProjectID]string{}
	for _, project := range projects {
		names[tracker.ProjectID(project.ID)] = project.Name
		if selectedProject == "" || tracker.ProjectID(project.ID) == selectedProject {
			visible[tracker.ProjectID(project.ID)] = true
		}
		manageable[tracker.ProjectID(project.ID)] = project.CanManageRunners
	}
	if selectedProject != "" && !visible[selectedProject] {
		return hostedFleetResponse{}, nil, operatortool.ErrAccessDenied
	}
	reservations := []scopedProviderAccount{}
	type fleetSpriteSource struct {
		runner runnerauth.Runner
		facts  machineSpriteFacts
	}
	sprites := []fleetSpriteSource{}
	result := hostedFleetResponse{Runners: []hostedFleetRunner{}, RunnerNames: map[string]hostedRunnerName{}, Current: detentVersion(s.config.Version), MinimumRunnerVersion: minimumRunnerVersion(s.config.Version)}
	read := func(r tenantstore.Reader) error {
		if selectedProject != "" {
			scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID()), Project: selectedProject, Credential: credential}
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
		}
		reader, ok := r.(runnerstore.FleetReader)
		if !ok {
			return errors.New("runner fleet reader unavailable")
		}
		if selectedProject == "" {
			names, err := reader.FleetNames(ctx, 10000)
			if err != nil {
				return err
			}
			for _, name := range names {
				if name.Removed {
					result.RunnerNames[name.RunnerID] = hostedRunnerName{DisplayName: name.DisplayName, Hostname: name.Hostname}
				}
			}
		}
		records, err := reader.FleetRecords(ctx, 10000)
		if err != nil {
			return err
		}
		leases, err := reader.FleetLeases(ctx, now, 10000)
		if err != nil {
			return err
		}
		allManageable := false
		if credential.HostedRole == "member" {
			count, err := reader.FleetProjectCount(ctx, 10000)
			if err != nil {
				return err
			}
			allManageable = count > 0 && count == len(projects)
			for _, allowed := range manageable {
				allManageable = allManageable && allowed
			}
		}
		result.Editable = credential.HostedRole == "owner" || credential.HostedRole == "admin" || credential.HostedRole == "member" && allManageable
		managementProjects := map[string][]tracker.ProjectID{}
		for _, lease := range leases {
			managementProjects[lease.RunnerID] = append(managementProjects[lease.RunnerID], lease.Lease.ProjectID)
		}

		urgent, err := r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		principal := ""
		if credential.HostedRole == "reporter" {
			principal = (hubscope.NativeScope{Credential: credential}).Actor().PrincipalID
		}
		leases = slices.DeleteFunc(leases, func(lease runnerstore.FleetLease) bool {
			return !visible[lease.Lease.ProjectID] || credential.HostedRole == "reporter" && (principal == "" || lease.ActorPrincipalID != principal)
		})
		for _, lease := range leases {
			if lease.Reservation != nil {
				reservations = append(reservations, scopedProviderAccount{runnerID: lease.RunnerID, Report: lease.Reservation.Report})
			}
		}
		runnerLeases := map[string][]runnerauth.RunnerLease{}
		hostSlots := map[tracker.MachineID]map[string]bool{}
		for _, lease := range leases {
			key := string(lease.Lease.ID)
			if lease.LandingCapacity > 0 {
				if group := landingCapacityKey(lease.MachineID, string(lease.Lease.ProjectID), lease.SessionID); group != "" {
					key = group
				}
			}
			if hostSlots[lease.MachineID] == nil {
				hostSlots[lease.MachineID] = map[string]bool{}
			}
			hostSlots[lease.MachineID][key] = true
			runnerLeases[lease.RunnerID] = append(runnerLeases[lease.RunnerID], lease.Lease)
		}
		providerViews := tenantFleetProviderViews(records, leases, now)
		for _, record := range records {
			identity, machine := record.Identity, record.Machine
			runner := runnerFromFleetRecord(record, now, urgent)
			if selectedProject != "" && !runner.AllowsProject(selectedProject) {
				continue
			}
			runner.HostUsed = len(hostSlots[identity.MachineID])
			runner.ProviderCapacity = providerViews[identity.RunnerID]
			runner.Leases = runnerLeases[identity.RunnerID]
			runner.Problems = slices.DeleteFunc(slices.Clone(runner.Problems), func(problem runnerauth.Problem) bool {
				return problem.ProjectID != "" && !visible[tracker.ProjectID(problem.ProjectID)]
			})
			if runner.ConnectionHealth == "online" {
				runner.Health = "online"
				if len(runner.Problems) > 0 {
					runner.Health = "needs_attention"
				}
			}

			version := machine.Version
			if runner.Lifecycle != nil {
				version = runner.Lifecycle.Version
			}
			view := hostedFleetRunnerView(runner, version, visible, now)
			view.HasHeartbeat = identity.LastHeartbeatAt.After(identity.CreatedAt)
			view.ClaimRefusalReason = runnerClaimRefusal(minimumRunnerVersion(s.config.Version), machine.Version)
			view.Revision = identity.Revision
			routing := runner.Routing
			routing.UpdateRequest = nil
			routing.ProjectIDs = slices.DeleteFunc(slices.Clone(routing.ProjectIDs), func(project tracker.ProjectID) bool { return !visible[project] })
			for project := range routing.ProjectRanks {
				if !visible[project] {
					delete(routing.ProjectRanks, project)
				}
			}
			for project := range routing.ProjectCapacityLimits {
				if !visible[project] {
					delete(routing.ProjectCapacityLimits, project)
				}
			}
			view.Routing = &routing
			view.Editable = credential.HostedRole == "owner" || credential.HostedRole == "admin"
			if credential.HostedRole == "member" {
				view.Editable = identity.Scope == "organization" && allManageable || identity.Scope != "organization" && len(identity.ProjectIDs) > 0
				for _, project := range identity.ProjectIDs {
					if !manageable[project] {
						view.Editable = false
					}
				}
				for _, project := range managementProjects[identity.RunnerID] {
					if !manageable[project] {
						view.Editable = false
					}
				}
			}

			if !view.Editable {
				view.EditRefusalReason = hostedRunnerEditRefusal(credential.HostedRole, identity, managementProjects[identity.RunnerID], projects, manageable, names)
			}

			owned := record.OwnerPrincipalID == credential.ID && credential.ID != "" || credential.Hosted != nil && record.OwnerUserID != "" && record.OwnerUserID == credential.Hosted.Subject
			view.CanEditProjects = view.Editable && owned
			view.CanConvertScope = view.Editable && (owned || hubscope.CanManageProjectSecrets(credential))
			capacity := runner.CapacityView("", now)
			capacity.Projects = map[tracker.ProjectID]runnerauth.ProjectCapacityView{}
			projectUsed := map[tracker.ProjectID]int{}
			for _, lease := range runner.Leases {
				projectUsed[lease.ProjectID]++
			}
			capacityProjects := map[tracker.ProjectID]bool{}
			for project := range identity.ProjectRanks {
				capacityProjects[project] = true
			}
			for project := range identity.ProjectCapacityLimits {
				capacityProjects[project] = true
			}
			for _, project := range identity.ProjectIDs {
				capacityProjects[project] = true
			}
			if selectedProject != "" {
				capacityProjects[selectedProject] = true
			}
			for project := range capacityProjects {
				if !visible[project] || !runner.AllowsProject(project) {
					continue
				}
				projectCapacity := runnerauth.ProjectCapacityView{Used: projectUsed[project]}
				if limit, ok := identity.ProjectCapacityLimits[project]; ok {
					projectCapacity.Desired = &limit
					projectCapacity.Applied = &limit
				}
				if capacity.Effective != nil {
					effective := *capacity.Effective
					if projectCapacity.Applied != nil {
						effective = min(effective, *projectCapacity.Applied)
					}
					projectCapacity.Effective = &effective
				}
				capacity.Projects[project] = projectCapacity
			}
			view.Capacity = &capacity
			view.ProjectCheckouts = map[tracker.ProjectID]hostedRunnerCheckout{}
			for project, checks := range record.Checks {
				if !visible[project] || identity.Scope != "organization" && !slices.Contains(identity.ProjectIDs, project) {
					continue
				}
				status := "preparing"
				if checks.Checkout == "failed" {
					status = "missing"
				} else if checks.Setup == "failed" {
					status = "setup_failed"
				} else if checks.Checkout == "passed" && checks.Setup == "passed" {
					status = "ready"
				}
				view.ProjectCheckouts[project] = hostedRunnerCheckout{Status: status, Message: checks.CheckoutMessage, FixCommand: checks.CheckoutFix}
			}
			result.Runners = append(result.Runners, view)
			sprites = append(sprites, fleetSpriteSource{runner: runner, facts: spriteFactsForMachine(machine)})
		}
		return nil
	}
	organization := tracker.OrganizationID(s.appOrganizationID())
	var err error
	if repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository); ok {
		err = repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, read)
	} else if selectedProject != "" {
		return hostedFleetResponse{}, nil, operatortool.ErrServiceUnavailable
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, read)
	}
	if err != nil {
		return hostedFleetResponse{}, nil, fmt.Errorf("read tenant fleet: %w", err)
	}
	spriteContext, cancelSprites := context.WithTimeout(ctx, 5*time.Second)
	defer cancelSprites()
	for index, source := range sprites {
		if err := s.hostedFleetSprite(ctx, spriteContext, credential, &result.Runners[index], source.runner, source.facts, visible); err != nil {
			return hostedFleetResponse{}, nil, fmt.Errorf("read runner sprite: %w", err)
		}
	}
	slices.SortFunc(result.Runners, func(a, b hostedFleetRunner) int {
		return cmp.Or(cmp.Compare(a.DisplayName, b.DisplayName), cmp.Compare(a.ID, b.ID))
	})
	return result, reservations, nil
}

func runnerFromFleetRecord(record runnerstore.FleetRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	return runnerfleet.FromRecord(record, now, urgent)
}

type tenantFleetAccount struct{ provider, shared, runner, alias string }

func tenantFleetAccountKey(runner string, report providercapacity.Report) tenantFleetAccount {
	if report.SharedAccountAlias != "" {
		return tenantFleetAccount{provider: report.Provider, shared: report.SharedAccountAlias}
	}
	return tenantFleetAccount{provider: report.Provider, runner: runner, alias: report.AccountAlias}
}

func tenantFleetProviderViews(records []runnerstore.FleetRecord, leases []runnerstore.FleetLease, now time.Time) map[string][]providercapacity.View {
	type account struct {
		limit, used int
		state       string
		reset       time.Time
	}
	accounts := map[tenantFleetAccount]account{}
	fresh := map[string]bool{}
	for _, record := range records {
		if record.CredentialRevoked || now.Before(record.CredentialCreatedAt) || record.CredentialExpiresAt != nil && !record.CredentialExpiresAt.After(now) {
			continue
		}
		for _, report := range record.Reports {
			key := tenantFleetAccountKey(record.Identity.RunnerID, report)
			a := accounts[key]
			a.limit = providerConcurrencyLimit(a.limit, report.MaxConcurrent)
			state := report.State(now)
			if a.state != "exhausted" && (state == "exhausted" || a.state != "unknown") {
				a.state = state
			}
			if report.ResetAt.After(a.reset) && state == "exhausted" {
				a.reset = report.ResetAt
			}
			accounts[key] = a
			if !now.Before(report.ObservedAt) && now.Before(report.ObservedAt.Add(providercapacity.MaxAge)) {
				for _, model := range report.Models {
					fresh[record.Identity.RunnerID+"\x00"+report.Provider+"\x00"+report.Backend+"\x00"+report.AccountAlias+"\x00"+report.SharedAccountAlias+"\x00"+model] = true
				}
			}
		}
	}
	groups := map[string]map[tenantFleetAccount]bool{}
	for _, lease := range leases {
		reservation := lease.Reservation
		if reservation == nil {
			continue
		}
		key := tenantFleetAccountKey(lease.RunnerID, reservation.Report)
		if reservation.Backend == "git" && reservation.Role == "merge" {
			group := landingCapacityKey(lease.MachineID, string(lease.Lease.ProjectID), lease.SessionID)
			if group != "" {
				group += "/" + lease.RunnerID
				if groups[group] == nil {
					groups[group] = map[tenantFleetAccount]bool{}
				}
				if groups[group][key] {
					continue
				}
				groups[group][key] = true
			}
		}
		a := accounts[key]
		a.used++
		if !fresh[lease.RunnerID+"\x00"+reservation.Report.Provider+"\x00"+reservation.Report.Backend+"\x00"+reservation.Report.AccountAlias+"\x00"+reservation.Report.SharedAccountAlias+"\x00"+reservation.Model] {
			a.limit = providerConcurrencyLimit(a.limit, reservation.Report.MaxConcurrent)
		}
		accounts[key] = a
	}
	views := map[string][]providercapacity.View{}
	for _, record := range records {
		for _, report := range record.Reports {
			a := accounts[tenantFleetAccountKey(record.Identity.RunnerID, report)]
			view := providercapacity.View{Report: report, Used: a.used, State: a.state, Reason: "Declared provider concurrency is available"}
			view.MaxConcurrent = providerConcurrencyLimit(report.MaxConcurrent, a.limit)
			ownState := report.State(now)
			if ownState == "exhausted" || ownState == "unknown" && view.State != "exhausted" {
				view.State = ownState
			}
			if a.state == "exhausted" && a.reset.After(view.ResetAt) {
				view.ResetAt = a.reset
			}
			if view.MaxConcurrent > 0 && a.used >= view.MaxConcurrent {
				view.Reason = "Shared provider concurrency is fully reserved; wait for lease release or expiry"
			} else if view.MaxConcurrent == 0 {
				view.Reason = "No provider account concurrency limit is configured; runner and host limits apply"
			}
			views[record.Identity.RunnerID] = append(views[record.Identity.RunnerID], view)
		}
	}
	return views
}

func (s *Service) readHostedRunnerHostHistoryTenant(ctx context.Context, credential hubapi.APICredential, runnerID string, from, to time.Time) ([]runnerHostHour, error) {
	if runnerID == "" {
		return nil, hubapi.NativePublicInvalid("Runner ID is required for host history")
	}
	projects, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return nil, err
	}
	if credential.HostedRole == "reporter" {
		return nil, hubapi.NativeNotFound()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(r tenantstore.Reader) error {
		total := 0
		after := tracker.ProjectID("")
		for {
			all, err := r.Projects(ctx, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, project := range all {
				if project.DeletedAt == nil {
					total++
					if total > len(projects) {
						return hubapi.NativeNotFound()
					}
				}
			}
			if len(all) < corestore.MaxPageSize {
				break
			}
			after = all[len(all)-1].ID
		}
		if total != len(projects) {
			return hubapi.NativeNotFound()
		}
		identity, err := r.RunnerIdentity(ctx, runnerID)
		if errors.Is(err, corestore.ErrNotFound) || identity.RemovedAt != nil {
			return hubapi.NativeNotFound()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.readRunnerHostHistory(ctx, tracker.OrganizationID(s.appOrganizationID()), runnerID, from, to)
}

func hostedRunnerEditRefusal(role string, identity runnerstore.Identity, running []tracker.ProjectID, projects []appBootstrapProject, manageable map[tracker.ProjectID]bool, names map[tracker.ProjectID]string) string {
	const refusal = "Runner changes need manage_runner on every allowed project and every project with running work; ask an owner or admin"
	if role != "member" {
		return refusal
	}
	required := slices.Concat(identity.ProjectIDs, running)
	if identity.Scope == "organization" {
		for _, project := range projects {
			required = append(required, tracker.ProjectID(project.ID))
		}
	} else if len(required) == 0 {
		return "An unassigned runner needs an owner or admin"
	}
	slices.Sort(required)
	for _, project := range slices.Compact(required) {
		if name, visible := names[project]; visible && !manageable[project] {
			return fmt.Sprintf("project %q (%s) lacks a manage_runner grant; ask an owner or admin", name, project)
		}
	}
	return refusal
}
