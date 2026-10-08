package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type placementDecision struct {
	Policy        policy.Placement `json:"policy"`
	ReadyTodo     int              `json:"ready_todo"`
	LocalFree     int              `json:"local_free"`
	SpriteFree    int              `json:"sprite_free"`
	SpriteUsed    int              `json:"sprite_used"`
	Pending       int              `json:"pending"`
	SpriteTarget  int              `json:"sprite_target"`
	Provisionable int              `json:"provisionable"`
	ProviderFree  *int             `json:"provider_free,omitempty"`
	Reason        string           `json:"reason"`
}

type placementRunner struct {
	runnerauth.Runner
	Sprite, Pending, Paused bool
	Enrollment              string
}

type placementSnapshot struct {
	placementDecision
	Runners      []placementRunner
	Requirements policy.Requirements
	Items        map[tracker.WorkItemID]providercapacity.Requirement
	Local        map[tracker.WorkItemID]int
	resolver     *runner.BoardIdentityResolver
}

func readPlacementPolicy(ctx context.Context, q nativeQueryer, scope nativeScope) (policy.Placement, int, error) {
	var raw string
	var ceiling int
	err := q.QueryRowContext(ctx, `SELECT placement_json,max_runners FROM project_sprite_pools WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&raw, &ceiling)
	if errors.Is(err, sql.ErrNoRows) {
		return policy.Placement{Mode: "blended"}, 0, nil
	}
	if err != nil {
		return policy.Placement{}, 0, err
	}
	var placement policy.Placement
	if err := json.Unmarshal([]byte(raw), &placement); err != nil {
		return placement, 0, err
	}
	return placement.Resolved(), ceiling, placement.Validate()
}

func readPlacementSpriteUsage(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) (int, error) {
	var used int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM leases l JOIN issues i ON i.id=l.issue_id JOIN machines m ON m.id=l.machine_id WHERE i.organization_id=? AND i.project_id=? AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?) AND json_extract(m.capabilities_json,'$.sprite_name')=m.hostname`, scope.organization, scope.project, formatHubTime(now)).Scan(&used)
	return used, err
}

func readPlacementRunners(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) ([]placementRunner, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.enrollment_id, COALESCE(json_extract(m.capabilities_json,'$.sprite_name'),''), COALESCE(json_extract(m.capabilities_json,'$.sprite_woken_at'),'') FROM runner_identities r JOIN machines m ON m.id=r.machine_id JOIN token_grants g ON g.token_id=r.token_id WHERE r.organization_id=? AND r.removed_at IS NULL AND g.organization_id=? AND g.project_id=? ORDER BY r.id`, scope.organization, scope.organization, scope.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type identity struct{ id, enrollment, name, woke string }
	var identities []identity
	for rows.Next() {
		var id identity
		if err := rows.Scan(&id.id, &id.enrollment, &id.name, &id.woke); err != nil {
			return nil, err
		}
		identities = append(identities, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var runners []placementRunner
	for _, id := range identities {
		r, err := readRunner(ctx, q, scope.organization, id.id, now)
		if err != nil {
			return nil, err
		}
		current := placementRunner{Runner: r, Enrollment: id.enrollment, Sprite: id.name == r.Hostname && validSpritesSlug(id.name)}
		current.Paused = current.Sprite && now.Sub(r.LastHeartbeatAt) >= spriteRunnerIdle
		var at time.Time
		if id.woke != "" {
			at, err = parseTimeValue(id.woke)
			if err != nil {
				return nil, err
			}
		}
		current.Pending = current.Paused && !at.IsZero() && !now.Before(at) && now.Sub(at) < time.Minute
		if current.Pending && r.Health != "revoked" && r.Health != "expired" {
			current.Health, current.ConnectionHealth = "online", "online"
		}
		runners = append(runners, current)
	}
	return runners, nil
}

type placementCapacity struct {
	hosts    map[tracker.MachineID]int
	runners  map[string]int
	accounts []providercapacity.Report
}

func placementRunnerCompatible(r placementRunner, project tracker.ProjectID, requirements policy.Requirements, now time.Time, paused bool) bool {
	if paused && r.Paused && !r.Pending && r.Health != "revoked" && r.Health != "expired" {
		r.Health, r.ConnectionHealth = "online", "online"
		r.Paused = false
	}
	if r.Paused && !r.Pending || len(r.Problems) != 0 || len(r.Exclusions(project, requirements, false)) != 0 {
		return false
	}
	availability, err := r.Availability.Evaluate(now)
	return err == nil && availability.Open
}

func newPlacementCapacity() *placementCapacity {
	return &placementCapacity{hosts: make(map[tracker.MachineID]int), runners: make(map[string]int)}
}

func (c *placementCapacity) available(r placementRunner, project tracker.ProjectID, requirements policy.Requirements, provider providercapacity.Requirement, now time.Time, consume bool) int {
	if !placementRunnerCompatible(r, project, requirements, now, false) {
		return 0
	}
	free := max(min(r.CapacityLimit, r.ReportedCapacity)-r.Used-c.runners[r.RunnerID], 0)
	free = min(free, max(r.HostCapacity-r.HostUsed-c.hosts[r.MachineID], 0))
	var selected providercapacity.Report
	providerFree := 0
	for _, view := range r.ProviderCapacity {
		if view.State == "exhausted" || provider.Model != "" && !view.Supports(provider) {
			continue
		}
		remaining := view.MaxConcurrent - view.Used
		for _, account := range c.accounts {
			if sharedProviderAccount(account, view.Report) {
				remaining--
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
		c.accounts = append(c.accounts, selected)
	}
	return free
}

func readPlacementSnapshot(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time, overrides []tracker.NativeCapacityCandidate) (placementSnapshot, error) {
	var result placementSnapshot
	placement, ceiling, err := readPlacementPolicy(ctx, q, scope)
	if err != nil {
		return result, err
	}
	result.Policy = placement
	result.Items = make(map[tracker.WorkItemID]providercapacity.Requirement)
	result.Local = make(map[tracker.WorkItemID]int)
	result.Runners, err = readPlacementRunners(ctx, q, scope, now)
	if err != nil {
		return result, err
	}
	result.SpriteUsed, err = readPlacementSpriteUsage(ctx, q, scope, now)
	if err != nil {
		return result, err
	}
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		var failure *nativeError
		if errors.As(err, &failure) && failure.Code == "policy_mismatch" {
			result.Reason = "No approved project policy; compatible Todo demand is unavailable"
			return result, nil
		}
		return result, err
	}
	result.Requirements = approval.Policy.Requirements
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, approval.Policy)
	if err != nil {
		return result, err
	}
	selection, err := readCloudModelSelection(ctx, q, scope)
	if err != nil {
		return result, err
	}
	workflow.Config.Agents.ModelSelection = selection.Effective
	workflow.Config.Plan.Enabled = approval.Policy.Gates.PlanEnabled
	resolver, err := runner.NewBoardIdentityResolver(workflow.Config, selector.Context{})
	if err != nil {
		return result, err
	}
	result.resolver = resolver
	query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), AvailableAt: now, Limit: 100}
	var ids []tracker.WorkItemID
	for {
		page, err := nativeCandidateIDs(ctx, q, query, nil, nil, []string{"todo"}, nil, nil, nil, nil)
		if err != nil {
			return result, err
		}
		ids = append(ids, page...)
		if len(page) < query.Limit {
			break
		}
		query.After = page[len(page)-1]
	}
	exposure := budget.CostExposure{SpriteInfrastructure: true, RunnerAPI: workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered}
	budgetDecision, err := checkMonthlyBudget(ctx, q, scope, exposure, false, now)
	if err != nil {
		return result, err
	}
	if !budgetDecision.Allowed {
		query.After = 0
		var continuations []tracker.WorkItemID
		for {
			page, err := nativeCandidateIDs(ctx, q, query, nil, nil, nil, nil, nil, nil, nil)
			if err != nil {
				return result, err
			}
			for _, id := range page {
				admitted, err := monthlyIssueAdmitted(ctx, q, id)
				if err != nil {
					return result, err
				}
				if !admitted {
					continue
				}
				decision, err := checkMonthlyBudget(ctx, q, scope, exposure, true, now)
				if err != nil {
					return result, err
				}
				if decision.Allowed {
					continuations = append(continuations, id)
				}
			}
			if len(page) < query.Limit {
				break
			}
			query.After = page[len(page)-1]
		}
		ids = continuations
	}
	localCapacity := newPlacementCapacity()
	localAssigned := make(map[tracker.WorkItemID]bool)
	for _, id := range ids {
		var native tracker.NativeWorkItemID
		if err := q.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE id=?`, id).Scan(&native); err != nil {
			return result, err
		}
		issue, _, err := readNativeIssue(ctx, q, scope, string(native))
		if err != nil {
			return result, err
		}
		identity, err := resolver.Identity(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels, State: issue.State})
		if err != nil {
			continue
		}
		requirement := providercapacity.Requirement{Role: identity.Role, Backend: identity.BackendKind, Model: identity.RequestedModel.Value}
		for _, candidate := range overrides {
			if candidate.WorkItemID == native && candidate.Revision == issue.Revision {
				requirement = candidate.Requirement
			}
		}
		if requirement.Validate() != nil {
			continue
		}
		compatible := ceiling > 0 && result.Requirements.RunnerID == "" && result.Requirements.MachineID == "" && len(result.Requirements.RequiredTags) == 0
		if slices.ContainsFunc(result.Runners, func(r placementRunner) bool { return r.Sprite && len(r.ProviderCapacity) > 0 }) {
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
			result.Local[id] += newPlacementCapacity().available(r, scope.project, result.Requirements, requirement, now, false)
		}
		if placement.Mode != "sprites_only" {
			for _, r := range result.Runners {
				if !r.Sprite && localCapacity.available(r, scope.project, result.Requirements, requirement, now, true) > 0 {
					result.LocalFree++
					localAssigned[id] = true
					break
				}
			}
		}
	}
	spriteCapacity := localCapacity
	counted := make(map[string]bool)
	for _, id := range ids {
		if limit := placement.SpriteLimit(); limit > 0 && result.SpriteFree+result.Pending >= max(limit-result.SpriteUsed, 0) {
			break
		}
		requirement, compatible := result.Items[id]
		if !compatible || localAssigned[id] || placement.Mode == "local_first" && result.Local[id] > 0 {
			continue
		}
		for _, r := range result.Runners {
			if !r.Sprite || spriteCapacity.available(r, scope.project, result.Requirements, requirement, now, true) == 0 {
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
	rows, err := q.QueryContext(ctx, `SELECT enrollment_id,state FROM project_sprite_members WHERE organization_id=? AND project_id=? AND state IN ('bootstrapping','enrolled')`, scope.organization, scope.project)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var enrollment, state string
		if err := rows.Scan(&enrollment, &state); err != nil {
			return result, err
		}
		if state == "enrolled" && slices.ContainsFunc(result.Runners, func(r placementRunner) bool { return r.Enrollment == enrollment && len(r.ProviderCapacity) > 0 }) {
			continue
		}
		if !slices.ContainsFunc(result.Runners, func(r placementRunner) bool {
			return r.Enrollment == enrollment && (r.Used > 0 || counted[enrollment])
		}) {
			result.Pending++
			bootstrapPending++
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
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
			if view.State == "exhausted" {
				continue
			}
			remaining := view.MaxConcurrent - view.Used
			for _, prior := range spriteCapacity.accounts {
				if sharedProviderAccount(prior, view.Report) {
					remaining--
				}
			}
			for _, prior := range providerSlots.accounts {
				if sharedProviderAccount(prior, view.Report) {
					remaining--
				}
			}
			for range max(remaining, 0) {
				providerSlots.accounts = append(providerSlots.accounts, view.Report)
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

func placementClaimAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID, now time.Time, overrides []tracker.NativeCapacityCandidate) (bool, string, error) {
	if allowed, reason, err := nativeSourceClaimAllowed(ctx, q, scope, machine, id); err != nil || !allowed {
		return allowed, reason, err
	}
	var destination, version string
	if err := q.QueryRowContext(ctx, "SELECT recovery_runner_id,recovery_version_id FROM issues WHERE id=? AND organization_id=? AND project_id=?", id, scope.organization, scope.project).Scan(&destination, &version); err != nil {
		return false, "", err
	}
	if destination != "" {
		var current string
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(json_extract(c.record_json,'$.current_version_id'),'') FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id JOIN issues i ON i.native_id=l.work_item_id AND i.project_id=l.project_id AND i.organization_id=l.organization_id WHERE i.id=? ORDER BY c.rowid DESC LIMIT 1`, id).Scan(&current); err != nil {
			return false, "", err
		}
		if current == version && scope.credential.Runner.RunnerID != destination {
			return false, "The operator selected recovery runner " + destination + " for version " + version, nil
		}
	}
	placement, _, err := readPlacementPolicy(ctx, q, scope)
	if err != nil {
		return false, "", err
	}
	var name, hostname string
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(json_extract(capabilities_json,'$.sprite_name'),''),hostname FROM machines WHERE id=? AND organization_id=?`, machine, scope.organization).Scan(&name, &hostname); err != nil {
		return false, "", err
	}
	sprite := name == hostname && validSpritesSlug(name)
	if !sprite {
		return placement.Mode != "sprites_only", "Resolved placement " + placement.Mode + " governs local eligibility", nil
	}
	if limit := placement.SpriteLimit(); limit > 0 {
		used, err := readPlacementSpriteUsage(ctx, q, scope, now)
		if err != nil {
			return false, "", err
		}
		if used >= limit {
			return false, "Configured maximum Sprite concurrency is full", nil
		}
	}
	if placement.Mode != "local_first" {
		return true, "Resolved placement " + placement.Mode + " permits Sprite runners", nil
	}
	snapshot, err := readPlacementSnapshot(ctx, q, scope, now, overrides)
	if err != nil {
		return false, "", err
	}
	if snapshot.Local[id] > 0 {
		return false, "Local-first placement has compatible free local capacity", nil
	}
	_, ready := snapshot.Items[id]
	if !ready && snapshot.resolver != nil {
		var issue connector.Issue
		var native tracker.NativeWorkItemID
		var revision tracker.Revision
		var labels string
		if err := q.QueryRowContext(ctx, `SELECT i.native_id,i.revision,i.title,i.body,i.labels_json,w.detent_state FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id WHERE i.id=? AND i.organization_id=? AND i.project_id=?`, id, scope.organization, scope.project).Scan(&native, &revision, &issue.Title, &issue.Description, &labels, &issue.State); err != nil {
			return false, "", err
		}
		if err := json.Unmarshal([]byte(labels), &issue.Labels); err != nil {
			return false, "", err
		}
		identity, err := snapshot.resolver.Identity(issue)
		if err != nil {
			return false, "Configured provider requirement is unavailable", nil
		}
		requirement := providercapacity.Requirement{Role: identity.Role, Backend: identity.BackendKind, Model: identity.RequestedModel.Value}
		for _, candidate := range overrides {
			if candidate.WorkItemID == native && candidate.Revision == revision {
				requirement = candidate.Requirement
			}
		}
		for _, r := range snapshot.Runners {
			if !r.Sprite && newPlacementCapacity().available(r, scope.project, snapshot.Requirements, requirement, now, false) > 0 {
				return false, "Local-first placement has compatible free local capacity", nil
			}
		}
		ready = !strings.EqualFold(issue.State, "Todo") && snapshot.ReadyTodo >= placement.TodoThreshold && snapshot.SpriteUsed < placement.OverflowSlots
	}
	allowed := ready && snapshot.ReadyTodo >= placement.TodoThreshold && snapshot.SpriteUsed < placement.SpriteLimit()
	reason := snapshot.Reason
	if allowed {
		reason = "Local-first overflow is eligible for this candidate"
	}
	return allowed, fmt.Sprintf("%s: ready Todo=%d, threshold=%d, local free=%d, Sprite used=%d, overflow slots=%d", reason, snapshot.ReadyTodo, placement.TodoThreshold, snapshot.LocalFree, snapshot.SpriteUsed, placement.OverflowSlots), nil
}
