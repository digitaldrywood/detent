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
	"github.com/digitaldrywood/detent/internal/tracker"
)

type monthlyBudgetSettings struct {
	Revision tracker.Revision     `json:"revision,string"`
	Policy   budget.MonthlyPolicy `json:"policy"`
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

func checkMonthlyBudget(ctx context.Context, q nativeQueryer, scope nativeScope, exposure budget.CostExposure, admitted bool, now time.Time) (budget.MonthlyDecision, error) {
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
	err := q.QueryRowContext(ctx, `SELECT COALESCE(json_extract(capabilities_json,'$.sprite_name')=hostname AND length(hostname)>0,0) FROM machines WHERE id=? AND organization_id=?`, machine, scope.organization).Scan(&exposure.SpriteInfrastructure)
	if err != nil {
		return exposure, err
	}
	if workspace {
		return exposure, nil
	}
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return exposure, err
	}
	if approval.Policy.Configuration == nil {
		exposure.RunnerAPI = true
		return exposure, nil
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, approval.Policy)
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

func checkMonthlyClaim(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID, id tracker.WorkItemID, workspace bool, now time.Time) (budget.MonthlyDecision, budget.CostExposure, error) {
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

func checkMonthlyLease(ctx context.Context, q nativeQueryer, record leaseRecord, now time.Time) error {
	var scope nativeScope
	var raw sql.NullString
	var profile string
	err := q.QueryRowContext(ctx, `SELECT i.organization_id,i.project_id,p.profile,b.exposure_json FROM leases l JOIN issues i ON i.id=l.issue_id JOIN projects p ON p.id=i.project_id LEFT JOIN monthly_budget_leases b ON b.lease_id=l.lease_id WHERE l.lease_id=? AND i.organization_id IS NOT NULL`, record.session.ID).Scan(&scope.organization, &scope.project, &profile, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if profile != "native" {
		return nil
	}
	var exposure budget.CostExposure
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &exposure); err != nil {
			return err
		}
	} else {
		exposure, err = monthlyClaimExposure(ctx, q, scope, record.session.Machine.ID, false)
		if err != nil {
			orgScope := scope
			orgScope.project = ""
			orgPolicy, readErr := readMonthlyBudgetSettings(ctx, q, orgScope)
			if readErr != nil {
				return readErr
			}
			projectPolicy, readErr := readMonthlyBudgetSettings(ctx, q, scope)
			if readErr != nil {
				return readErr
			}
			if !orgPolicy.Policy.Enabled && !projectPolicy.Policy.Enabled {
				return nil
			}
			return err
		}
	}
	decision, err := checkMonthlyBudget(ctx, q, scope, exposure, true, now)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fmt.Errorf("%w: %s", tracker.ErrLeaseConflict, monthlyBudgetRefusal(decision))
	}
	return nil
}

func monthlySpriteAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) (bool, error) {
	exposure := budget.CostExposure{SpriteInfrastructure: true}
	decision, err := checkMonthlyBudget(ctx, q, scope, exposure, false, now)
	if err != nil || decision.Allowed {
		return decision.Allowed, err
	}
	decision, err = checkMonthlyBudget(ctx, q, scope, exposure, true, now)
	if err != nil || !decision.Allowed {
		return false, err
	}
	query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), AvailableAt: now, Limit: 100}
	for {
		ids, err := nativeCandidateIDs(ctx, q, query, nil, nil, nil, nil, nil, nil, nil)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			admitted, err := monthlyIssueAdmitted(ctx, q, id)
			if err != nil {
				return false, err
			}
			if admitted {
				return true, nil
			}
		}
		if len(ids) < query.Limit {
			return false, nil
		}
		query.After = ids[len(ids)-1]
	}
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
		record, err := coordinator.readConversation(ctx, s.database.db, id)
		if err != nil || record.OrganizationID != organization {
			continue
		}
		decision, err := checkMonthlyBudget(ctx, s.database.db, nativeScope{organization: organization, project: record.ProjectID}, budget.CostExposure{LunaAPI: true}, true, s.config.now())
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
		decision, err := checkMonthlyBudget(ctx, s.database.db, nativeScope{organization: key.organization}, budget.CostExposure{SpriteInfrastructure: true}, true, s.config.now())
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

func checkMonthlyIssueBudget(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.WorkItemID, exposure budget.CostExposure, admitted bool, now time.Time) (budget.MonthlyDecision, error) {
	decision, err := checkMonthlyBudget(ctx, q, scope, exposure, admitted, now)
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

func monthlyBudgetRetainsRunner(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID) (bool, error) {
	orgScope := scope
	orgScope.project = ""
	orgPolicy, err := readMonthlyBudgetSettings(ctx, q, orgScope)
	if err != nil {
		return false, err
	}
	projectPolicy, err := readMonthlyBudgetSettings(ctx, q, scope)
	if err != nil {
		return false, err
	}
	if !orgPolicy.Policy.Enabled && !projectPolicy.Policy.Enabled {
		return false, nil
	}
	var retained bool
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM monthly_budget_admissions a JOIN issues i ON i.id=a.issue_id JOIN leases l ON l.issue_id=i.id WHERE i.organization_id=? AND i.project_id=? AND l.machine_id=? AND l.fencing_token=(SELECT max(latest.fencing_token) FROM leases latest WHERE latest.issue_id=i.id))`, scope.organization, scope.project, machine).Scan(&retained)
	return retained, err
}
