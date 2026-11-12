package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type monthlyBudgetSettings struct {
	Revision tracker.Revision     `json:"revision,string"`
	Policy   budget.MonthlyPolicy `json:"policy"`
}

var ErrBillingBudgetUnavailable = errors.New("monthly budget enforcement requires the selected billing transaction")

func readBillingMonthlySettings(ctx context.Context, repository tenantstore.Repository, scope nativeScope) (monthlyBudgetSettings, error) {
	var settings monthlyBudgetSettings
	err := repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		row, err := reader.BillingMonthlyPolicy(ctx, scope.project)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		settings = monthlyBudgetSettings{Revision: row.Revision, Policy: row.Policy}
		return nil
	})
	return settings, err
}

func selectedBillingBudgetState(ctx context.Context, repository tenantstore.Repository, scope nativeScope) (monthlyBudgetSettings, monthlyBudgetSettings, error) {
	orgScope := scope
	orgScope.project = ""
	organization, err := readBillingMonthlySettings(ctx, repository, orgScope)
	if err != nil {
		return monthlyBudgetSettings{}, monthlyBudgetSettings{}, err
	}
	project, err := readBillingMonthlySettings(ctx, repository, scope)
	return organization, project, err
}

func readMonthlyBudgetSettings(ctx context.Context, q nativeQueryer, scope nativeScope) (monthlyBudgetSettings, error) {
	var settings monthlyBudgetSettings
	var raw string
	err := q.QueryRowContext(ctx, `SELECT revision,policy_json FROM monthly_budget_policies WHERE organization_id=? AND project_id=? ORDER BY revision DESC LIMIT 1`, scope.organization, scope.project).Scan(&settings.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	return settings, json.Unmarshal([]byte(raw), &settings.Policy)
}

func monthlyBudgetSpend(totals []monthlyCostTotal) budget.MonthlySpend {
	var spend budget.MonthlySpend
	for _, total := range totals {
		if total.Currency != "USD" || total.KnownMicros == nil {
			continue
		}
		switch total.Bucket {
		case lunaCostBucket:
			spend.LunaMicros = max(*total.KnownMicros, 0)
		case runnerCostBucket:
			spend.RunnerMicros = max(*total.KnownMicros, 0)
		case spriteCostBucket:
			spend.SpriteMicros = max(*total.KnownMicros, 0)
		}
	}
	return spend
}

func readBillingMonthlyBudgetInput(ctx context.Context, reader billingstore.Reader, scope nativeScope, now time.Time) (billingstore.MonthlyBudgetInput, error) {
	var input billingstore.MonthlyBudgetInput
	for i, target := range []struct {
		project tracker.ProjectID
		policy  *billingstore.MonthlyPolicy
		spend   *budget.MonthlySpend
	}{{"", &input.OrganizationPolicy, &input.OrganizationSpend}, {scope.project, &input.ProjectPolicy, &input.ProjectSpend}} {
		if i == 1 && scope.project == "" {
			continue
		}
		row, err := reader.BillingMonthlyPolicy(ctx, target.project)
		if errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return input, err
		}
		*target.policy = row
		if row.Policy.Enabled {
			*target.spend, err = reader.BillingMonthlySpend(ctx, target.project, now)
			if err != nil {
				return input, err
			}
		}
	}
	return input, nil
}

func checkMonthlyBudget(ctx context.Context, q nativeQueryer, scope nativeScope, exposure budget.CostExposure, admitted bool, now time.Time, selected ...tenantstore.Repository) (budget.MonthlyDecision, error) {
	if len(selected) > 0 && selected[0] != nil {
		var decision budget.MonthlyDecision
		err := selected[0].ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			input, err := readBillingMonthlyBudgetInput(ctx, reader, scope, now)
			if err != nil {
				return err
			}
			input.Exposure, input.Admitted = exposure, admitted
			decision, err = billingstore.EvaluateMonthlyBudget(input)
			return err
		})
		return decision, err
	}
	orgScope := scope
	orgScope.project = ""
	organization, err := readMonthlyBudgetSettings(ctx, q, orgScope)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	project, err := readMonthlyBudgetSettings(ctx, q, scope)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	if scope.project == "" {
		project = monthlyBudgetSettings{}
	}
	if !organization.Policy.Enabled && !project.Policy.Enabled {
		return budget.MonthlyDecision{Allowed: true}, nil
	}
	window, err := costMonth("", now)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	observations, err := readCostObservations(ctx, q, string(scope.organization), nil, window)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	luna, err := readLunaCostObservations(ctx, q, string(scope.organization), nil, window)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	report, err := buildMonthlyCostReport(string(scope.organization), "organization", window, now, append(observations, luna...), nil)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	orgSpend := monthlyBudgetSpend(report.Totals)
	var projectSpend budget.MonthlySpend
	for _, entry := range report.ByProject {
		if entry.ProjectID == string(scope.project) {
			projectSpend = monthlyBudgetSpend(entry.Totals)
		}
	}
	return budget.CheckMonthly(budget.MonthlyConstraint{Scope: "organization", Policy: organization.Policy, Spend: orgSpend}, budget.MonthlyConstraint{Scope: "project", Policy: project.Policy, Spend: projectSpend}, exposure, admitted)
}

func monthlyBudgetRefusal(decision budget.MonthlyDecision) string {
	for _, exhausted := range decision.Exhausted {
		return fmt.Sprintf("Monthly %s %s budget exhausted: detected %d / %d USD micros (%s); costs may be delayed or incomplete", exhausted.Scope, exhausted.Bucket, exhausted.CurrentMicros, exhausted.CapMicros, exhausted.Mode)
	}
	return "Monthly budget admission requires ready Todo or a durably admitted issue"
}

func monthlyClaimExposure(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, workspace bool) (budget.CostExposure, error) {
	var exposure budget.CostExposure
	facts, err := readMachineSpriteFacts(ctx, q, scope.organization, machine)
	if err != nil {
		return exposure, err
	}
	var hostname string
	err = q.QueryRowContext(ctx, `SELECT hostname FROM machines WHERE id=? AND organization_id=?`, machine, scope.organization).Scan(&hostname)
	exposure.SpriteInfrastructure = facts.Name != nil && *facts.Name == hostname && hostname != ""
	if err != nil {
		return exposure, err
	}
	if workspace {
		return exposure, nil
	}
	approval, err := requireProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return exposure, err
	}
	if approval.Policy.Configuration == nil {
		exposure.RunnerAPI = true
		return exposure, nil
	}
	workflow, err := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), approval.Policy)
	if err != nil {
		return exposure, err
	}
	exposure.RunnerAPI = workflow.Config.Budget.EffectiveBillingMode() == config.BillingModeMetered
	return exposure, nil
}

func monthlyIssueAdmitted(ctx context.Context, q nativeQueryer, id tracker.WorkItemID) (bool, error) {
	var admitted bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM monthly_budget_admissions a JOIN issues i ON i.id=a.issue_id JOIN workflow_states w ON w.id=i.workflow_state_id WHERE a.issue_id=? AND i.archived=0 AND w.terminal=0 AND lower(w.detent_state) NOT IN ('blocked','backlog','cancelled'))`, id).Scan(&admitted)
	return admitted, err
}

func checkMonthlyClaim(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID, workspace bool, now time.Time, selected ...tenantstore.Repository) (budget.MonthlyDecision, budget.CostExposure, error) {
	if len(selected) > 0 && selected[0] != nil {
		organization, project, err := selectedBillingBudgetState(ctx, selected[0], scope)
		if err != nil {
			return budget.MonthlyDecision{}, budget.CostExposure{}, err
		}
		if organization.Policy.Enabled || project.Policy.Enabled {
			return budget.MonthlyDecision{}, budget.CostExposure{}, ErrBillingBudgetUnavailable
		}
		return budget.MonthlyDecision{Allowed: true}, budget.CostExposure{}, nil
	}
	exposure, err := monthlyClaimExposure(ctx, q, scope, machine, workspace)
	if err != nil {
		return budget.MonthlyDecision{}, exposure, err
	}
	admitted, err := monthlyIssueAdmitted(ctx, q, id)
	if err != nil {
		return budget.MonthlyDecision{}, exposure, err
	}
	decision, err := checkMonthlyIssueBudget(ctx, q, scope, id, exposure, admitted, now)
	return decision, exposure, err
}

func recordMonthlyAdmission(ctx context.Context, tx *sql.Tx, id tracker.WorkItemID, lease tracker.LeaseID, exposure budget.CostExposure, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO monthly_budget_admissions(issue_id,admitted_at) SELECT i.id,? FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id WHERE i.id=? AND i.archived=0 AND w.terminal=0 AND lower(w.detent_state) NOT IN ('blocked','backlog','cancelled') ON CONFLICT(issue_id) DO NOTHING`, formatHubTime(now), id); err != nil {
		return err
	}
	raw, err := json.Marshal(exposure)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO monthly_budget_leases(lease_id,exposure_json) VALUES(?,?)`, lease, string(raw))
	return err
}

func monthlySpriteAllowed(ctx context.Context, _ nativeQueryer, scope nativeScope, now time.Time, selected ...tenantstore.Repository) (bool, error) {
	if len(selected) == 0 || selected[0] == nil {
		return false, ErrBillingBudgetUnavailable
	}
	return monthlySpriteAllowedPG(ctx, selected[0], scope, now)
}

func (s *Service) stopMonthlyBudgetTurns(ctx context.Context, organization tracker.OrganizationID) {
	if s.conversations == nil {
		return
	}
	coordinator, ok := s.conversations.coordinator.(*conversationTurnCoordinator)
	if !ok {
		return
	}
	coordinator.mu.Lock()
	ids := make([]string, 0, len(coordinator.turns))
	for id := range coordinator.turns {
		ids = append(ids, id)
	}
	coordinator.mu.Unlock()
	for _, id := range ids {
		var project tracker.ProjectID
		err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
			record, err := reader.ConversationByID(ctx, id)
			project = record.ProjectID
			return err
		})
		if err != nil {
			continue
		}
		decision, err := checkMonthlyBudget(ctx, nil, nativeScope{organization: organization, project: project}, budget.CostExposure{LunaAPI: true}, true, s.config.now(), s.database.billingRepository)
		if err != nil {
			s.config.Logger.Warn("Check active conversation monthly budget", "error", err)
			continue
		}
		if !decision.Allowed {
			coordinator.cancelTurn(id, true)
		}
	}
}

func (s *Service) resumeMonthlyBudgetWork(ctx context.Context, scope nativeScope) {
	if s.conversations != nil {
		if err := s.conversations.wakePending(ctx); err != nil {
			s.config.Logger.Warn("Resume budget-eligible conversations", "error", err)
		}
	}
	s.startSpritePoolForQueue(scope)
}

func (s *Service) stopMonthlyBudgetSpritePasses(ctx context.Context, organization tracker.OrganizationID) {
	s.spriteWakeMu.Lock()
	keys := make([]spriteWakeKey, 0, len(s.spriteWakes))
	for key := range s.spriteWakes {
		if key.organization == organization {
			keys = append(keys, key)
		}
	}
	s.spriteWakeMu.Unlock()
	for _, key := range keys {
		decision, err := checkMonthlyBudget(ctx, nil, nativeScope{organization: key.organization}, budget.CostExposure{SpriteInfrastructure: true}, true, s.config.now(), s.database.billingRepository)
		if err != nil {
			s.config.Logger.Warn("Check active Sprite lifecycle monthly budget", "error", err)
			continue
		}
		if decision.Allowed {
			continue
		}
		s.spriteWakeMu.Lock()
		if pass := s.spriteWakes[key]; pass != nil && pass.cancel != nil {
			pass.pendingState = ""
			pass.cancel()
		}
		s.spriteWakeMu.Unlock()
	}
}

func checkMonthlyIssueBudget(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID, exposure budget.CostExposure, admitted bool, now time.Time, selected ...tenantstore.Repository) (budget.MonthlyDecision, error) {
	decision, err := checkMonthlyBudget(ctx, q, scope, exposure, admitted, now, selected...)
	if len(selected) > 0 && selected[0] != nil {
		return decision, err
	}
	if err != nil || !decision.Allowed || admitted {
		return decision, err
	}
	orgScope := scope
	orgScope.project = ""
	orgPolicy, err := readMonthlyBudgetSettings(ctx, q, orgScope)
	if err != nil {
		return decision, err
	}
	projectPolicy, err := readMonthlyBudgetSettings(ctx, q, scope)
	if err != nil {
		return decision, err
	}
	if orgPolicy.Policy.Enabled || projectPolicy.Policy.Enabled {
		var state string
		if err := q.QueryRowContext(ctx, `SELECT lower(w.detent_state) FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id WHERE i.id=?`, id).Scan(&state); err != nil {
			return decision, err
		}
		if state != "todo" {
			decision.Allowed = false
		}
	}
	return decision, nil
}

func monthlyBudgetRetainsRunner(ctx context.Context, _ nativeQueryer, scope nativeScope, machine tracker.MachineID, selected ...tenantstore.Repository) (bool, error) {
	if len(selected) > 0 && selected[0] != nil {
		var retained bool
		err := selected[0].ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			enabled := false
			for _, project := range []tracker.ProjectID{"", scope.project} {
				row, err := r.BillingMonthlyPolicy(ctx, project)
				if errors.Is(err, corestore.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				enabled = enabled || row.Policy.Enabled
			}
			if !enabled {
				return nil
			}
			var err error
			retained, err = r.BillingMonthlyRetainsMachine(ctx, scope.project, machine)
			return err
		})
		return retained, err
	}
	return false, ErrBillingBudgetUnavailable
}
