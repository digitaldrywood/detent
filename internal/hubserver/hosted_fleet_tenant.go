package hubserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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

func (s *Service) readHostedFleetTenant(ctx context.Context, credential apiCredential) (hostedFleetResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	projects, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	visible := map[tracker.ProjectID]bool{}
	manageable := map[tracker.ProjectID]bool{}
	for _, project := range projects {
		visible[tracker.ProjectID(project.ID)] = true
		manageable[tracker.ProjectID(project.ID)] = project.CanManageRunners
	}
	now := s.config.now().UTC()
	result := hostedFleetResponse{Runners: []hostedFleetRunner{}, RunnerNames: map[string]hostedRunnerName{}, Current: detentVersion(s.config.Version), MinimumRunnerVersion: minimumRunnerVersion(s.config.Version)}
	read := func(r tenantstore.Reader) error {
		reader, ok := r.(runnerstore.FleetReader)
		if !ok {
			return errors.New("runner fleet reader unavailable")
		}
		names, err := reader.FleetNames(ctx, 10000)
		if err != nil {
			return err
		}
		for _, name := range names {
			if name.Removed {
				result.RunnerNames[name.RunnerID] = hostedRunnerName{DisplayName: name.DisplayName, Hostname: name.Hostname}
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
			principal = (nativeScope{credential: credential}).actor().PrincipalID
		}
		leases = slices.DeleteFunc(leases, func(lease runnerstore.FleetLease) bool {
			return !visible[lease.Lease.ProjectID] || credential.HostedRole == "reporter" && (principal == "" || lease.ActorPrincipalID != principal)
		})
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

			view := hostedFleetRunnerView(runner, machine.Version, visible, now)
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

			owned := record.OwnerPrincipalID == credential.ID && credential.ID != "" || credential.Hosted != nil && record.OwnerUserID != "" && record.OwnerUserID == credential.Hosted.Subject
			view.CanEditProjects = view.Editable && owned
			view.CanConvertScope = view.Editable && (owned || canManageProjectSecrets(credential))
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
		}
		return nil
	}
	organization := tracker.OrganizationID(s.appOrganizationID())
	if repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository); ok {
		err = repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, read)
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, read)
	}
	if err != nil {
		return hostedFleetResponse{}, fmt.Errorf("read tenant fleet: %w", err)
	}
	slices.SortFunc(result.Runners, func(a, b hostedFleetRunner) int {
		return cmp.Or(cmp.Compare(a.DisplayName, b.DisplayName), cmp.Compare(a.ID, b.ID))
	})
	if s.database.plans() != nil {
		entitlement, readErr := s.database.hostedEntitlement(ctx, nil, now)
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

func runnerFromFleetRecord(record runnerstore.FleetRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	identity, machine := record.Identity, record.Machine
	runner := runnerauth.Runner{
		Binding:          runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID},
		OrganizationID:   identity.OrganizationID,
		Revision:         identity.Revision,
		LastHeartbeatAt:  identity.LastHeartbeatAt,
		Hostname:         machine.Hostname,
		HostDisplayName:  machine.DisplayName,
		HostCapacity:     int(machine.Capacity),
		HostRevision:     record.HostRevision,
		ReportedCapacity: int(identity.ReportedCapacity),
		CapacityConfig:   identity.CapacityConfig,
		OS:               identity.OS,
		Architecture:     identity.Architecture,
		BackendIsolation: record.Isolation,
		Update:           record.Update.Observation,
		Problems:         slices.Clone(record.Problems),
		Operations:       slices.Clone(identity.Operations),
		Routing: runnerauth.Routing{
			ProjectRankOverrides:  maps.Clone(identity.ProjectRanks),
			UpdateRequest:         record.Update.Desired,
			CapacityRequest:       identity.CapacityRequest,
			Scope:                 identity.Scope,
			State:                 identity.State,
			DisplayName:           identity.DisplayName,
			ProjectIDs:            slices.Clone(identity.ProjectIDs),
			Tags:                  slices.Clone(identity.Tags),
			CapacityLimit:         int(identity.CapacityLimit),
			ProjectCapacityLimits: maps.Clone(identity.ProjectCapacityLimits),
			ProjectRanks:          maps.Clone(identity.ProjectRanks),
			MachineUse:            identity.MachineUse,
			IsolationTier:         identity.IsolationTier,
			HostServices:          slices.Clone(identity.HostServices),
			Availability: runnerauth.Availability{
				Timezone:     identity.Availability.Timezone,
				Windows:      slices.Clone(identity.Availability.Windows),
				WindowSlots:  maps.Clone(identity.Availability.WindowSlots),
				HardDeadline: identity.Availability.HardDeadline,
			},
		},
	}
	runner.Health = "online"
	if record.CredentialRevoked {
		runner.Health = "revoked"
	} else if !identity.ExpiresAt.After(now) || now.Before(record.CredentialCreatedAt) || record.CredentialExpiresAt != nil && !record.CredentialExpiresAt.After(now) {
		runner.Health = "expired"
	} else if now.Before(identity.LastHeartbeatAt) || !now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) {
		runner.Health = "offline"
	}
	runner.ConnectionHealth = runner.Health
	if runner.Health == "online" && len(runner.Problems) > 0 {
		runner.Health = "needs_attention"
	}
	applyUrgentRunnerRouting(&runner, urgentRunnerUpdate{Revision: urgent.Revision, Request: urgent.Request}, machine.Version)
	return runner
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

func (s *Service) readHostedRunnerHostHistoryTenant(ctx context.Context, credential apiCredential, runnerID string, from, to time.Time) ([]runnerHostHour, error) {
	if runnerID == "" {
		return nil, nativePublicInvalid("Runner ID is required for host history")
	}
	projects, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return nil, err
	}
	if credential.HostedRole == "reporter" {
		return nil, nativeNotFound()
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
						return nativeNotFound()
					}
				}
			}
			if len(all) < corestore.MaxPageSize {
				break
			}
			after = all[len(all)-1].ID
		}
		if total != len(projects) {
			return nativeNotFound()
		}
		identity, err := r.RunnerIdentity(ctx, runnerID)
		if errors.Is(err, corestore.ErrNotFound) || identity.RemovedAt != nil {
			return nativeNotFound()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.readRunnerHostHistory(ctx, tracker.OrganizationID(s.appOrganizationID()), runnerID, from, to)
}
