package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) projectOnboardingRecord(ctx context.Context, scope nativeScope) (onboarding.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	result := onboarding.Project{Runners: []runnerauth.Eligibility{}, Artifacts: []artifact.Binding{}}
	var project projectstore.Record
	var records []runnerstore.FleetRecord
	var leases []runnerstore.FleetLease
	var urgent runnerstore.UrgentUpdate
	var checks map[string]runnerauth.LocalChecks
	var repositories map[string]runnerauth.RepositoryList
	var projectCount int
	now := s.config.now().UTC()
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnerstore.OnboardingReader)
		if !ok {
			return errors.New("project onboarding reader unavailable")
		}
		var err error
		project, err = r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		result.Progress, err = r.ProjectOnboardingProgress(ctx, scope.project)
		if err != nil {
			return err
		}
		result.LatestRun, err = r.ProjectLatestAttemptStatus(ctx, scope.project)
		if err != nil {
			return err
		}
		projectCount, err = reader.FleetProjectCount(ctx, 10000)
		if err != nil {
			return err
		}
		if projectCount > 10000 {
			return runnerstore.ErrLimitExceeded
		}
		records, err = reader.FleetRecords(ctx, 10000)
		if err != nil {
			return err
		}
		leases, err = reader.FleetLeases(ctx, now, 10000)
		if err != nil {
			return err
		}
		urgent, err = r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		checks, err = reader.ProjectRunnerChecks(ctx, scope.project, 10000)
		if err != nil {
			return err
		}
		if scope.credential.Runner.RunnerID == "" {
			repositories, err = reader.RunnerRepositoryLists(ctx, 10000)
		}
		return err
	})
	if err != nil {
		return result, projectRecordError(err)
	}
	result.Repository = project.Settings.Repository
	if result.Repository == "" {
		result.Repository = project.Settings.CheckoutRepository
	}
	if result.Repository == "" {
		result.RepositoryBindingReason = &onboarding.RepositoryBindingReason{Code: "repository_binding_required", Message: "Choose and connect a repository in the Repository step before retrying checkout."}
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
	if err != nil {
		return result, err
	}
	if approval.Policy.ID != "" {
		result.Policy = &approval
	}
	policyView, err := s.tenantProjectPolicyRead(ctx, scope.organization, scope.project, approval, 1, "")
	if err != nil {
		return result, err
	}
	result.ObservedPolicies = policyView.ObservedPolicies
	manageable := map[tracker.ProjectID]bool{}
	allManageable := false
	if scope.credential.Hosted != nil {
		projects, err := s.hostedReadableProjects(ctx, scope.credential)
		if err != nil {
			return result, err
		}
		allManageable = len(projects) == projectCount && projectCount > 0
		for _, project := range projects {
			manageable[tracker.ProjectID(project.ID)] = project.CanManageRunners
			allManageable = allManageable && project.CanManageRunners
		}
	}
	canManage := scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly || scope.credential.Hosted != nil
	providerViews := tenantFleetProviderViews(records, leases, now)
	hostSlots := map[tracker.MachineID]map[string]bool{}
	runnerSlots := map[string]map[string]bool{}
	projectUsed := map[string]map[tracker.ProjectID]int{}
	runnerLeases := map[string][]runnerauth.RunnerLease{}
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
		if runnerSlots[lease.RunnerID] == nil {
			runnerSlots[lease.RunnerID] = map[string]bool{}
			projectUsed[lease.RunnerID] = map[tracker.ProjectID]int{}
		}
		if !runnerSlots[lease.RunnerID][key] {
			projectUsed[lease.RunnerID][lease.Lease.ProjectID]++
		}
		runnerSlots[lease.RunnerID][key] = true
		runnerLeases[lease.RunnerID] = append(runnerLeases[lease.RunnerID], lease.Lease)
	}
	for _, record := range records {
		runner := runnerFromFleetRecord(record, now, urgent)
		if !canManage && !runner.AllowsProject(scope.project) {
			continue
		}
		owned := record.OwnerPrincipalID == scope.credential.ID && scope.credential.ID != "" || scope.credential.Hosted != nil && record.OwnerUserID != "" && record.OwnerUserID == scope.credential.Hosted.Subject
		if runner.Scope == "projects" && !slices.Contains(runner.ProjectIDs, scope.project) && !owned {
			continue
		}
		runner.Leases = runnerLeases[runner.RunnerID]
		if scope.credential.Hosted != nil {
			allowed := allManageable
			if projects := runnerManagementProjects(runner); projects != nil {
				allowed = len(projects) > 0 && manageable[scope.project]
				for _, project := range projects {
					allowed = allowed && manageable[project]
				}
			}
			if !allowed {
				continue
			}
		}
		runner.HostUsed = len(hostSlots[runner.MachineID])
		runner.Used = len(runnerSlots[runner.RunnerID])
		runner.ProjectUsed = projectUsed[runner.RunnerID]
		runner.ProviderCapacity = providerViews[runner.RunnerID]
		exclusions := runner.Exclusions(scope.project, approval.Policy.Requirements, false)
		runner.Problems = slices.DeleteFunc(runner.Problems, func(problem runnerauth.Problem) bool {
			return problem.ProjectID != "" && problem.ProjectID != string(scope.project)
		})
		otherProjects := len(slices.DeleteFunc(slices.Clone(runner.ProjectIDs), func(project tracker.ProjectID) bool { return project == scope.project }))
		if runner.Scope == "organization" {
			otherProjects = max(0, projectCount-1)
		}
		runner.ProjectIDs = []tracker.ProjectID{scope.project}
		runner.ProjectRanks = map[tracker.ProjectID]int{scope.project: runner.ProjectRanks[scope.project]}
		if limit, ok := runner.ProjectCapacityLimits[scope.project]; ok {
			runner.ProjectCapacityLimits = map[tracker.ProjectID]int{scope.project: limit}
		} else {
			runner.ProjectCapacityLimits = nil
		}
		runner.ProjectUsed = map[tracker.ProjectID]int{scope.project: runner.ProjectUsed[scope.project]}
		runner.Leases = nil
		runner.ProviderCapacity = nil
		eligibility := runnerauth.Eligibility{Runner: runner, Exclusions: exclusions, OtherProjectCount: otherProjects}
		if check, ok := checks[runner.RunnerID]; ok {
			eligibility.LocalChecks = &check
		}
		if list, ok := repositories[runner.RunnerID]; ok && (scope.credential.Hosted != nil || scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly) {
			eligibility.RepositoryList = &list
		}
		result.Runners = append(result.Runners, eligibility)
	}
	slices.SortStableFunc(result.Runners, func(a, b runnerauth.Eligibility) int {
		if (a.Runner.ConnectionHealth == "online") == (b.Runner.ConnectionHealth == "online") {
			return 0
		}
		if a.Runner.ConnectionHealth == "online" {
			return -1
		}
		return 1
	})
	result.Evaluate()
	return result, nil
}
