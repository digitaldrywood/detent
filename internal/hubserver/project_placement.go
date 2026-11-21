package hubserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readPlacementPolicy(ctx context.Context, r tenantstore.Reader) (policy.Placement, int, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return policy.Placement{}, 0, hubapi.NativeWorkItemStorageUnavailable()
	}
	row, err := owner.SpritePool(ctx)
	if errors.Is(err, corestore.ErrNotFound) {
		return policy.Placement{Mode: "blended"}, 0, nil
	}
	if err != nil {
		return policy.Placement{}, 0, err
	}
	placement := policy.Placement{Mode: row.PlacementMode, TodoThreshold: int(row.TodoThreshold), OverflowSlots: int(row.OverflowSlots), MaxSpriteSlots: int(row.MaxSpriteSlots)}
	return placement.Resolved(), int(row.MaxRunners), placement.Validate()
}

func readPlacementSpriteUsage(ctx context.Context, r tenantstore.Reader, now time.Time) (int, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return 0, hubapi.NativeWorkItemStorageUnavailable()
	}
	return owner.SpriteLeaseUsage(ctx, now, "")
}

func readSpriteMembers(ctx context.Context, owner postgres.SpriteReader) ([]postgres.SpriteMember, error) {
	result := []postgres.SpriteMember{}
	after := ""
	for {
		page, err := owner.SpriteMembers(ctx, after, corestore.MaxPageSize)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < corestore.MaxPageSize {
			return result, nil
		}
		next := page[len(page)-1].Name
		if next <= after {
			return nil, corestore.ErrInvalidPage
		}
		after = next
	}
}

func readPlacementRunners(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, now time.Time) ([]hubrecords.PlacementRunner, error) {
	reader, ok := r.(runnerstore.RoutingPageReader)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	urgent, err := r.RunnerUrgentUpdate(ctx)
	if err != nil {
		return nil, err
	}
	result := []hubrecords.PlacementRunner{}
	for offset := 0; ; offset += corestore.MaxPageSize {
		var page []runnerstore.RoutingRecord
		var err error
		if scope.Project == "" {
			page, err = reader.RunnerRoutingRecords(ctx, corestore.MaxPageSize, offset, now)
		} else {
			page, err = reader.RunnerProjectRoutingRecords(ctx, scope.Project, corestore.MaxPageSize, offset, now)
		}
		if err != nil {
			return nil, err
		}
		for _, record := range page {
			if scope.Project != "" {
				allowed, err := spriteAllocationRunnerAllowedRecord(ctx, r, scope, record)
				if err != nil {
					return nil, err
				}
				if !allowed {
					continue
				}
			}
			runner := runnerFromRoutingRecord(record, now, urgent)
			current := hubrecords.PlacementRunner{Runner: runner, Enrollment: record.Record.Identity.EnrollmentID, Sprite: record.Record.Machine.SpriteName == runner.Hostname && validSpritesSlug(record.Record.Machine.SpriteName)}
			current.Paused = current.Sprite && now.Sub(runner.LastHeartbeatAt) >= spriteRunnerIdle
			woke := record.Record.Machine.SpriteWokenAt
			current.Pending = current.Paused && woke != nil && !now.Before(*woke) && now.Sub(*woke) < time.Minute
			if current.Pending && runner.Health != "revoked" && runner.Health != "expired" {
				current.Health, current.ConnectionHealth = "online", "online"
			}
			result = append(result, current)
		}
		if len(page) < corestore.MaxPageSize {
			return result, nil
		}
	}
}

type placementIssueSource struct {
	candidate claimstore.Candidate
	item      corestore.WorkItem
	version   eventstore.CollaborationVersion
	admitted  bool
}

type placementSource struct {
	project           projectstore.Metadata
	policy            policystore.Revision
	policyFound       bool
	models            policystore.OrganizationModels
	placement         policy.Placement
	ceiling           int
	runners           []hubrecords.PlacementRunner
	spriteUsed        int
	items             []placementIssueSource
	members           []postgres.SpriteMember
	allocationManaged bool
}

func (s *Service) readPlacementSource(ctx context.Context, scope hubscope.NativeScope, now time.Time) (placementSource, error) {
	var source placementSource
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return source, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		source.project, err = r.ProjectMetadata(ctx, scope.Project)
		if err != nil {
			return err
		}
		source.placement, source.ceiling, err = readPlacementPolicy(ctx, r)
		if err != nil {
			return err
		}
		source.runners, err = readPlacementRunners(ctx, r, scope, now)
		if err != nil {
			return err
		}
		source.spriteUsed, err = readPlacementSpriteUsage(ctx, r, now)
		if err != nil {
			return err
		}
		source.policy, err = r.ProjectPolicy(ctx, scope.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		source.policyFound = true
		source.models, err = r.OrganizationModels(ctx)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		source.members, err = readSpriteMembers(ctx, owner)
		if err != nil {
			return err
		}
		_, err = owner.SpriteAllocation(ctx, scope.Project)
		source.allocationManaged = err == nil
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		query := claimstore.CandidateQuery{ProjectID: scope.Project, Scope: string(scope.Project), AvailableAt: now, Limit: corestore.MaxPageSize}
		for {
			page, err := r.CandidatePage(ctx, query)
			if err != nil {
				return err
			}
			for _, candidate := range page.Items {
				item, err := r.WorkItem(ctx, scope.Project, candidate.WorkItemID)
				if err != nil {
					return err
				}
				version, err := r.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
				if err != nil {
					return err
				}
				_, err = r.BillingMonthlyAdmission(ctx, tracker.WorkItemID(candidate.ID))
				admitted := err == nil
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				source.items = append(source.items, placementIssueSource{candidate: candidate, item: item, version: version, admitted: admitted})
			}
			if len(page.Items) == 0 {
				return nil
			}
			if page.Next == query.After {
				return corestore.ErrInvalidCursor
			}
			query.After = page.Next
		}
	})
	return source, err
}

type placementCapacity struct {
	hosts    map[tracker.MachineID]int
	runners  map[string]int
	accounts []scopedProviderAccount
}

func placementRunnerCompatible(r hubrecords.PlacementRunner, project tracker.ProjectID, requirements policy.Requirements, now time.Time, paused bool) bool {
	if paused && r.Paused && !r.Pending && r.Health != "revoked" && r.Health != "expired" {
		r.Health, r.ConnectionHealth = "online", "online"
		r.Paused = false
	}
	blockingProblem := slices.ContainsFunc(r.Problems, func(p runnerauth.Problem) bool {
		return p.Code != "settings_invalid" || p.Check != "commit_signing"
	})
	if r.Paused && !r.Pending || blockingProblem || len(r.Exclusions(project, requirements, false)) != 0 {
		return false
	}
	availability, err := r.Availability.Evaluate(now)
	return err == nil && availability.Open
}

func newPlacementCapacity() *placementCapacity {
	return &placementCapacity{hosts: make(map[tracker.MachineID]int), runners: make(map[string]int)}
}

func (c *placementCapacity) available(r hubrecords.PlacementRunner, project tracker.ProjectID, requirements policy.Requirements, provider providercapacity.Requirement, now time.Time, consume bool) int {
	if !placementRunnerCompatible(r, project, requirements, now, false) {
		return 0
	}
	free := max(min(r.CapacityLimit, r.ReportedCapacity)-r.Used-c.runners[r.RunnerID], 0)
	free = min(free, max(r.HostCapacity-r.HostUsed-c.hosts[r.MachineID], 0))
	var selected providercapacity.Report
	providerFree := 0
	for _, view := range r.ProviderCapacity {
		if provider.Model != "" && !view.Supports(provider) {
			continue
		}
		remaining := free
		if view.MaxConcurrent > 0 {
			remaining = view.MaxConcurrent - view.Used
			for _, account := range c.accounts {
				if sharedProviderAccount(account, scopedProviderAccount{runnerID: r.RunnerID, Report: view.Report}) {
					remaining--
				}
			}
		}
		if remaining > providerFree {
			providerFree, selected = remaining, view.Report
		}
	}
	free = min(free, max(providerFree, 0))
	if consume && free > 0 {
		c.hosts[r.MachineID]++
		c.runners[r.RunnerID]++
		c.accounts = append(c.accounts, scopedProviderAccount{runnerID: r.RunnerID, Report: selected})
	}
	return free
}

func (s *Service) readPlacementSnapshot(ctx context.Context, scope hubscope.NativeScope, now time.Time, overrides []tracker.NativeCapacityCandidate) (hubrecords.PlacementSnapshot, error) {
	return s.readPlacementSnapshotWithCapacity(ctx, scope, now, overrides, newPlacementCapacity(), make(map[string]bool))
}

func (s *Service) readPlacementSnapshotWithCapacity(ctx context.Context, scope hubscope.NativeScope, now time.Time, overrides []tracker.NativeCapacityCandidate, localCapacity *placementCapacity, counted map[string]bool) (hubrecords.PlacementSnapshot, error) {
	var result hubrecords.PlacementSnapshot
	source, err := s.readPlacementSource(ctx, scope, now)
	if err != nil {
		return result, err
	}
	placement, ceiling := source.placement, source.ceiling
	result.Policy, result.Runners, result.SpriteUsed = placement, source.runners, source.spriteUsed
	result.Items = make(map[tracker.WorkItemID]providercapacity.Requirement)
	result.Local = make(map[tracker.WorkItemID]int)
	if !source.policyFound {
		result.Reason = "No approved project policy; compatible Todo demand is unavailable"
		return result, nil
	}
	descriptor, err := eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, source.policy)
	if err != nil {
		return result, err
	}
	workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(scope.Organization)+"/"+string(scope.Project), descriptor)
	if err != nil {
		return result, err
	}
	result.Requirements = descriptor.Requirements
	workflow.Config.Plan.Enabled = descriptor.Gates.PlanEnabled
	selection, err := s.projectRecordModel(ctx, source.project.Settings, source.models)
	if err != nil {
		return result, err
	}
	workflow.Config.Agents.ModelSelection = selection.Effective
	resolver, err := runner.NewBoardIdentityResolver(workflow.Config, selector.Context{})
	if err != nil {
		return result, err
	}
	result.Resolver = resolver
	exposure := budget.CostExposure{SpriteInfrastructure: true, RunnerAPI: workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered}
	decision, err := checkMonthlyBudget(ctx, nil, scope, exposure, false, now, s.config.TenantRepository)
	if err != nil {
		return result, err
	}
	continuation, err := checkMonthlyBudget(ctx, nil, scope, exposure, true, now, s.config.TenantRepository)
	if err != nil {
		return result, err
	}
	var ids []tracker.WorkItemID
	issues := map[tracker.WorkItemID]tracker.NativeIssue{}
	for _, row := range source.items {
		if decision.Allowed && !strings.EqualFold(row.candidate.State, "Todo") {
			continue
		}
		if !decision.Allowed && (!continuation.Allowed || !row.admitted || slices.Contains([]string{"blocked", "backlog", "cancelled"}, strings.ToLower(row.candidate.State))) {
			continue
		}
		issue, err := s.hydrateCanonicalWorkItemRecord(ctx, row.item, row.version)
		if err != nil {
			return result, err
		}
		id := tracker.WorkItemID(row.item.ID)
		ids = append(ids, id)
		issues[id] = issue
	}
	localAssigned := make(map[tracker.WorkItemID]bool)
	for _, id := range ids {
		issue := issues[id]
		identity, err := resolver.Identity(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels, State: issue.State})
		if err != nil {
			continue
		}
		requirement := providercapacity.Requirement{Role: identity.Role, Backend: identity.BackendKind, Model: identity.RequestedModel.Value}
		for _, candidate := range overrides {
			if candidate.WorkItemID == issue.WorkItemID && candidate.Revision == issue.Revision {
				requirement = candidate.Requirement
			}
		}
		if requirement.Validate() != nil {
			continue
		}
		compatible := ceiling > 0 && result.Requirements.RunnerID == "" && result.Requirements.MachineID == "" && len(result.Requirements.RequiredTags) == 0
		if slices.ContainsFunc(result.Runners, func(r hubrecords.PlacementRunner) bool { return r.Sprite && len(r.ProviderCapacity) > 0 }) {
			compatible = false
		}
		for _, r := range result.Runners {
			if !r.Sprite || r.State != "active" || r.Health == "revoked" || r.Health == "expired" || result.Requirements.Match(r.RunnerID, string(r.MachineID), r.Tags) != nil {
				continue
			}
			if slices.ContainsFunc(r.ProviderCapacity, func(view providercapacity.View) bool { return view.Supports(requirement) }) {
				compatible = true
			}
		}
		if !compatible {
			continue
		}
		result.Items[id] = requirement
		result.ReadyTodo++
		for _, r := range result.Runners {
			if r.Sprite {
				continue
			}
			result.Local[id] += newPlacementCapacity().available(r, scope.Project, result.Requirements, requirement, now, false)
		}
		if placement.Mode != "sprites_only" {
			for _, r := range result.Runners {
				if !r.Sprite && localCapacity.available(r, scope.Project, result.Requirements, requirement, now, true) > 0 {
					result.LocalFree++
					localAssigned[id] = true
					break
				}
			}
		}
	}
	spriteCapacity := localCapacity
	for _, id := range ids {
		if limit := placement.SpriteLimit(); limit > 0 && result.SpriteFree+result.Pending >= max(limit-result.SpriteUsed, 0) {
			break
		}
		requirement, compatible := result.Items[id]
		if !compatible || localAssigned[id] || placement.Mode == "local_first" && result.Local[id] > 0 {
			continue
		}
		for _, r := range result.Runners {
			if !r.Sprite || spriteCapacity.available(r, scope.Project, result.Requirements, requirement, now, true) == 0 {
				continue
			}
			counted[r.Enrollment] = true
			if r.Pending {
				result.Pending++
			} else {
				result.SpriteFree++
			}
			break
		}
	}
	bootstrapPending := 0
	for _, member := range source.members {
		owner := ""
		if source.allocationManaged {
			owner = string(scope.Project)
		}
		if member.AllocationProjectID != owner || member.State != "bootstrapping" && member.State != "enrolled" {
			continue
		}
		enrollment, state := member.EnrollmentID, member.State
		if state == "enrolled" && slices.ContainsFunc(result.Runners, func(r hubrecords.PlacementRunner) bool {
			return r.Enrollment == enrollment && len(r.ProviderCapacity) > 0
		}) {
			continue
		}
		if !counted[enrollment] && !slices.ContainsFunc(result.Runners, func(r hubrecords.PlacementRunner) bool { return r.Enrollment == enrollment && r.Used > 0 }) {
			counted[enrollment] = true
			result.Pending++
			bootstrapPending++
		}
	}
	result.SpriteTarget = max(result.ReadyTodo-result.LocalFree, 0)
	if limit := placement.SpriteLimit(); limit > 0 {
		result.SpriteTarget = min(result.SpriteTarget, max(limit-result.SpriteUsed, 0))
	}
	result.Reason = "Compatible ready Todo exceeds eligible local capacity"
	if placement.Mode == "local_first" {
		fullLocalDemand := 0
		for id := range result.Items {
			if result.Local[id] == 0 {
				fullLocalDemand++
			}
		}
		result.SpriteTarget = min(result.SpriteTarget, fullLocalDemand)
		if result.ReadyTodo < placement.TodoThreshold {
			result.SpriteTarget = 0
			result.Reason = "Compatible ready Todo is below the local-first threshold"
		} else {
			result.SpriteTarget = min(result.SpriteTarget, max(placement.OverflowSlots-result.SpriteUsed, 0))
			if fullLocalDemand == 0 && result.ReadyTodo > 0 {
				result.Reason = "Local-first placement has compatible free local capacity"
			}
		}
	}
	if result.LocalFree == result.ReadyTodo && placement.Mode != "sprites_only" {
		result.Reason = "Eligible local capacity can serve compatible ready Todo"
	}
	if limit := placement.SpriteLimit(); limit > 0 && result.SpriteUsed >= limit {
		result.Reason = "Configured maximum Sprite concurrency is full"
	}
	result.Provisionable = max(result.SpriteTarget-result.SpriteFree-result.Pending, 0)
	known := false
	providerCapacity := 0
	providerSlots := newPlacementCapacity()
	for _, r := range result.Runners {
		if !r.Sprite {
			continue
		}
		for _, view := range r.ProviderCapacity {
			supported := len(result.Items) == 0
			for _, requirement := range result.Items {
				if view.Supports(requirement) {
					supported = true
					break
				}
			}
			if !supported {
				continue
			}
			known = true
			remaining := result.SpriteTarget
			if view.MaxConcurrent > 0 {
				remaining = view.MaxConcurrent - view.Used
				for _, prior := range spriteCapacity.accounts {
					if sharedProviderAccount(prior, scopedProviderAccount{runnerID: r.RunnerID, Report: view.Report}) {
						remaining--
					}
				}
				for _, prior := range providerSlots.accounts {
					if sharedProviderAccount(prior, scopedProviderAccount{runnerID: r.RunnerID, Report: view.Report}) {
						remaining--
					}
				}
			}
			for range max(remaining, 0) {
				providerSlots.accounts = append(providerSlots.accounts, scopedProviderAccount{runnerID: r.RunnerID, Report: view.Report})
				providerCapacity++
			}
		}
	}
	if known {
		remaining := max(providerCapacity-bootstrapPending, 0)
		result.ProviderFree = &remaining
		result.Provisionable = min(result.Provisionable, remaining)
		if result.Provisionable == 0 && result.SpriteTarget > result.SpriteFree+result.Pending {
			result.Reason = "Reported compatible provider concurrency or budget is exhausted"
		}
	}
	return result, nil
}

var errClaimPlacementSnapshotRequired = errors.New("local-first claim placement requires a placement snapshot")

func claimPlacementAllowed(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, identity runnerstore.Identity, machine leasestore.Machine, plan hubrecords.NativeClaimPlan, overrides []tracker.NativeCapacityCandidate, workspace bool, now time.Time, snapshot *hubrecords.PlacementSnapshot) (bool, string, error) {
	allowed, err := spriteAllocationRunnerAllowedFacts(ctx, r, scope, identity, machine, true)
	if err != nil {
		return false, "", err
	}
	if !allowed {
		return false, "Project Sprite allocation excludes this runner", nil
	}
	if workspace {
		return true, "Project Sprite allocation permits this runner", nil
	}
	placement, _, err := readPlacementPolicy(ctx, r)
	if err != nil {
		return false, "", err
	}
	if machine.SpriteName != machine.Hostname || !validSpritesSlug(machine.SpriteName) {
		return placement.Mode != "sprites_only", "Resolved placement " + placement.Mode + " governs local eligibility", nil
	}
	used, err := readPlacementSpriteUsage(ctx, r, now)
	if err != nil {
		return false, "", err
	}
	if limit := placement.SpriteLimit(); limit > 0 && used >= limit {
		return false, "Configured maximum Sprite concurrency is full", nil
	}
	if placement.Mode != "local_first" {
		return true, "Resolved placement " + placement.Mode + " permits Sprite runners", nil
	}
	if snapshot == nil {
		return false, "", errClaimPlacementSnapshotRequired
	}
	id := tracker.WorkItemID(plan.Item.ID)
	if snapshot.Local[id] > 0 {
		return false, "Local-first placement has compatible free local capacity", nil
	}
	_, ready := snapshot.Items[id]
	if !ready && snapshot.Resolver != nil {
		issue := plan.Issue
		identity, err := snapshot.Resolver.Identity(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels, State: issue.State})
		if err != nil {
			return false, "Configured provider requirement is unavailable", nil
		}
		requirement := providercapacity.Requirement{Role: identity.Role, Backend: identity.BackendKind, Model: identity.RequestedModel.Value}
		for _, candidate := range overrides {
			if candidate.WorkItemID == plan.Item.NativeID && int64(candidate.Revision) == plan.Item.Revision {
				requirement = candidate.Requirement
			}
		}
		for _, runner := range snapshot.Runners {
			if !runner.Sprite && newPlacementCapacity().available(runner, scope.Project, snapshot.Requirements, requirement, now, false) > 0 {
				return false, "Local-first placement has compatible free local capacity", nil
			}
		}
		ready = !strings.EqualFold(issue.State, "Todo") && snapshot.ReadyTodo >= placement.TodoThreshold && used < placement.OverflowSlots
	}
	allowed = ready && snapshot.ReadyTodo >= placement.TodoThreshold && used < placement.SpriteLimit()
	reason := snapshot.Reason
	if allowed {
		reason = "Local-first overflow is eligible for this candidate"
	}
	return allowed, fmt.Sprintf("%s: ready Todo=%d, threshold=%d, local free=%d, Sprite used=%d, overflow slots=%d", reason, snapshot.ReadyTodo, placement.TodoThreshold, snapshot.LocalFree, used, placement.OverflowSlots), nil
}

func (s *Service) readOrganizationPlacementSnapshot(ctx context.Context, scope hubscope.NativeScope, now time.Time) (hubrecords.PlacementSnapshot, error) {
	result := hubrecords.PlacementSnapshot{Items: make(map[tracker.WorkItemID]providercapacity.Requirement), Local: make(map[tracker.WorkItemID]int)}
	var placement policy.Placement
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		var err error
		placement, _, err = readPlacementPolicy(ctx, r)
		return err
	})
	if err != nil {
		return result, err
	}
	result.Policy = placement
	var projects []corestore.Project
	managed := map[tracker.ProjectID]bool{}
	err = s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		allocationAfter := tracker.ProjectID("")
		for {
			page, err := owner.SpriteAllocations(ctx, allocationAfter, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, allocation := range page {
				managed[allocation.ProjectID] = true
			}
			if len(page) < corestore.MaxPageSize {
				break
			}
			next := page[len(page)-1].ProjectID
			if next <= allocationAfter {
				return corestore.ErrInvalidCursor
			}
			allocationAfter = next
		}
		after := tracker.ProjectID("")
		for {
			page, err := r.Projects(ctx, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, project := range page {
				if project.Profile == "native" && project.DeletedAt == nil {
					projects = append(projects, project)
				}
			}
			if len(page) < corestore.MaxPageSize {
				return nil
			}
			next := page[len(page)-1].ID
			if next <= after {
				return corestore.ErrInvalidCursor
			}
			after = next
		}
	})
	if err != nil {
		return result, err
	}
	slices.SortStableFunc(projects, func(a, b corestore.Project) int {
		if managed[a.ID] != managed[b.ID] {
			if managed[a.ID] {
				return -1
			}
			return 1
		}
		if a.SchedulingRank < b.SchedulingRank {
			return -1
		}
		if a.SchedulingRank > b.SchedulingRank {
			return 1
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})

	capacity := newPlacementCapacity()
	counted := make(map[string]bool)
	for _, project := range projects {
		projectScope := scope
		projectScope.Project = project.ID
		snapshot, err := s.readPlacementSnapshotWithCapacity(ctx, projectScope, now, nil, capacity, counted)
		if err != nil {
			return result, err
		}
		var allocation projectSpriteAllocation
		err = s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
			var err error
			allocation, _, err = projectSpriteAllocationRecord(ctx, r, projectScope)
			return err
		})
		if err != nil {
			return result, err
		}
		if !allocation.Managed {
			result.ReadyTodo += snapshot.ReadyTodo
			result.SpriteTarget += snapshot.SpriteTarget
			result.SpriteFree += snapshot.SpriteFree
			result.Pending += snapshot.Pending
			result.Provisionable += snapshot.Provisionable
			result.LocalFree += snapshot.LocalFree
		}
		if snapshot.ProviderFree != nil {
			if result.ProviderFree == nil {
				result.ProviderFree = new(*snapshot.ProviderFree)
			} else {
				*result.ProviderFree = max(*result.ProviderFree, *snapshot.ProviderFree)
			}
		}
	}
	err = s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		var err error
		result.SpriteUsed, err = readPlacementSpriteUsage(ctx, r, now)
		return err
	})
	if err != nil {
		return result, err
	}
	if limit := placement.SpriteLimit(); limit > 0 {
		remaining := max(limit-result.SpriteUsed, 0)
		result.SpriteTarget = min(result.SpriteTarget, remaining)
		result.SpriteFree = min(result.SpriteFree, remaining)
		result.Pending = min(result.Pending, max(remaining-result.SpriteFree, 0))
	}
	result.Provisionable = max(result.SpriteTarget-result.SpriteFree-result.Pending, 0)
	return result, nil
}
