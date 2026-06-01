package web

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const budgetHistoryWindowDays = 7

type configuredBudget struct {
	ProjectID      string
	PerDayMaxUSD   float64
	PerIssueMaxUSD float64
}

func (s *Server) enrichSnapshot(ctx context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot {
	budget, ok := s.snapshotBudget(ctx, snapshot.GeneratedAt)
	if !ok {
		return snapshot
	}

	budget.ProjectedCostUSD = snapshot.Budget.ProjectedCostUSD
	budget.Refusals = append([]telemetry.BudgetRefusal(nil), snapshot.Budget.Refusals...)
	snapshot.Budget = budget
	return snapshot
}

func (s *Server) snapshotBudget(ctx context.Context, now time.Time) (telemetry.Budget, bool) {
	projects := s.configuredBudgets()
	if len(projects) == 0 {
		return telemetry.Budget{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Second)
	}
	now = now.UTC()
	periodStart, periodEnd := dailyBudgetPeriod(now)
	queryFrom := periodStart.AddDate(0, 0, -(budgetHistoryWindowDays - 1))

	events, err := s.store.BudgetCostEvents(ctx, store.BudgetCostQuery{
		ProjectIDs: budgetProjectIDs(projects),
		From:       queryFrom,
		To:         periodEnd,
	})
	if err != nil {
		s.logger.Warn("budget spend query failed", slog.Any("error", err))
		return telemetry.Budget{}, false
	}

	points, currentSpend := currentBudgetSpendPoints(events, periodStart, periodEnd)
	budget := telemetry.Budget{
		Enabled:           true,
		PerDayMaxUSD:      positiveFloatPtr(totalDailyBudgetCap(projects)),
		PerIssueMaxUSD:    issueBudgetCap(projects),
		CurrentSpendUSD:   currentSpend,
		ProjectedSpendUSD: projectedBudgetSpend(periodStart, periodEnd, now, currentSpend),
		PeriodStart:       periodStart,
		PeriodEnd:         periodEnd,
		SpendPoints:       points,
		Days:              budgetSpendDays(events),
	}
	return budget, true
}

func (s *Server) configuredBudgets() []configuredBudget {
	if s.registry == nil {
		return nil
	}

	projects := s.registry.List()
	budgets := make([]configuredBudget, 0, len(projects))
	for _, project := range projects {
		if project == nil {
			continue
		}
		workflow := project.Workflow()
		cfg := workflow.Config.Budget
		if !cfg.Enabled {
			continue
		}
		projectID := strings.TrimSpace(string(project.ID()))
		if projectID == "" {
			continue
		}
		budgets = append(budgets, configuredBudget{
			ProjectID:      projectID,
			PerDayMaxUSD:   cfg.PerDayMaxUSD,
			PerIssueMaxUSD: cfg.PerIssueMaxUSD,
		})
	}
	return budgets
}

func dailyBudgetPeriod(now time.Time) (time.Time, time.Time) {
	year, month, day := now.UTC().Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

func budgetProjectIDs(projects []configuredBudget) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ProjectID)
	}
	return ids
}

func totalDailyBudgetCap(projects []configuredBudget) float64 {
	total := 0.0
	for _, project := range projects {
		if project.PerDayMaxUSD > 0 {
			total += project.PerDayMaxUSD
		}
	}
	return total
}

func issueBudgetCap(projects []configuredBudget) *float64 {
	if len(projects) != 1 || projects[0].PerIssueMaxUSD <= 0 {
		return nil
	}
	value := projects[0].PerIssueMaxUSD
	return &value
}

func currentBudgetSpendPoints(events []store.BudgetCostEvent, periodStart time.Time, periodEnd time.Time) ([]telemetry.BudgetSpendPoint, float64) {
	periodEvents := make([]store.BudgetCostEvent, 0, len(events))
	for _, event := range events {
		at := event.At.UTC()
		if at.IsZero() || at.Before(periodStart) || !at.Before(periodEnd) {
			continue
		}
		periodEvents = append(periodEvents, store.BudgetCostEvent{
			ProjectID: event.ProjectID,
			At:        at,
			CostUSD:   event.CostUSD,
		})
	}
	sort.SliceStable(periodEvents, func(i, j int) bool {
		return periodEvents[i].At.Before(periodEvents[j].At)
	})

	points := make([]telemetry.BudgetSpendPoint, 0, len(periodEvents))
	total := 0.0
	for _, event := range periodEvents {
		if event.CostUSD <= 0 {
			continue
		}
		total += event.CostUSD
		points = append(points, telemetry.BudgetSpendPoint{
			At:       event.At,
			SpendUSD: total,
		})
	}
	return points, total
}

func budgetSpendDays(events []store.BudgetCostEvent) []telemetry.BudgetDay {
	byDay := map[string]float64{}
	for _, event := range events {
		if event.CostUSD <= 0 || event.At.IsZero() {
			continue
		}
		day := event.At.UTC().Format("2006-01-02")
		byDay[day] += event.CostUSD
	}
	if len(byDay) == 0 {
		return nil
	}

	days := make([]string, 0, len(byDay))
	for day := range byDay {
		days = append(days, day)
	}
	sort.Strings(days)
	if len(days) > budgetHistoryWindowDays {
		days = days[len(days)-budgetHistoryWindowDays:]
	}

	out := make([]telemetry.BudgetDay, 0, len(days))
	for _, day := range days {
		out = append(out, telemetry.BudgetDay{
			Date:     day,
			SpendUSD: byDay[day],
		})
	}
	return out
}

func projectedBudgetSpend(periodStart time.Time, periodEnd time.Time, now time.Time, currentSpend float64) float64 {
	if currentSpend <= 0 {
		return 0
	}
	if periodStart.IsZero() || !periodEnd.After(periodStart) {
		return currentSpend
	}
	elapsed := now.Sub(periodStart).Seconds()
	if elapsed <= 0 {
		return currentSpend
	}
	total := periodEnd.Sub(periodStart).Seconds()
	if total <= 0 {
		return currentSpend
	}
	projected := currentSpend * total / elapsed
	if projected < currentSpend {
		return currentSpend
	}
	return projected
}

func positiveFloatPtr(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	return &value
}
