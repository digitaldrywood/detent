package hubserver

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type postgresReportUsage struct {
	Costs          map[reportstore.CostKey]float64
	Facts          []reportstore.UsageFact
	CostsAvailable bool
	UsageAvailable bool
}

func readPostgresReportUsage(ctx context.Context, reader tenantstore.Reader, query reportstore.OverviewQuery, facts []reportstore.OverviewFact, w, prior operatortool.AnalyticsWindow) (postgresReportUsage, error) {
	keys := make([]reportstore.CostKey, 0, len(facts)*2)
	for _, fact := range facts {
		keys = append(keys, reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: w.From, To: w.To}, reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: prior.From, To: prior.To})
	}
	if usageReader, ok := reader.(reportstore.UsageReader); ok {
		usageQuery := query
		usageQuery.Limit = 100000
		rows, err := usageReader.ReadReportUsage(ctx, usageQuery)
		if err != nil {
			return postgresReportUsage{}, err
		}
		if _, err := postgresReportUsageRows(rows); err != nil {
			return postgresReportUsage{}, err
		}
		coverage := false
		if coverageReader, ok := reader.(reportstore.UsageCoverageReader); ok {
			coverage, err = coverageReader.ReadReportUsageCoverage(ctx, usageQuery)
			if err != nil {
				return postgresReportUsage{}, err
			}
		}
		return postgresReportUsage{Costs: postgresReportIssueCosts(keys, rows), Facts: rows, CostsAvailable: coverage, UsageAvailable: coverage}, nil
	}
	if costReader, ok := reader.(reportstore.CostReader); ok {
		costs, err := costReader.ReadIssueCosts(ctx, keys)
		if err != nil {
			return postgresReportUsage{}, err
		}
		return postgresReportUsage{Costs: costs, CostsAvailable: completeReportCosts(keys, costs)}, nil
	}
	return postgresReportUsage{}, nil
}

func postgresReportUsageRows(facts []reportstore.UsageFact) (map[string][]usageRow, error) {
	out := map[string][]usageRow{}
	for _, fact := range facts {
		if fact.AttemptID == "" || fact.ProjectID == "" || fact.WorkItemID == "" || fact.Period.IsZero() || !strings.EqualFold(fact.Currency, "usd") || fact.Cost < 0 || math.IsNaN(fact.Cost) || math.IsInf(fact.Cost, 0) || fact.Input < 0 || fact.CachedInput < 0 || fact.Output < 0 {
			return nil, fmt.Errorf("invalid typed report usage row")
		}
		out[fact.AttemptID] = append(out[fact.AttemptID], usageRow{AttemptID: fact.AttemptID, Period: fact.Period, Provider: fact.Provider, Model: fact.Model, Input: fact.Input, CachedInput: fact.CachedInput, Output: fact.Output, Cost: fact.Cost, Currency: "usd", RunnerID: fact.RunnerID, RunnerName: fact.RunnerName})
	}
	return out, nil
}

func postgresReportIssueCosts(keys []reportstore.CostKey, facts []reportstore.UsageFact) map[reportstore.CostKey]float64 {
	out := make(map[reportstore.CostKey]float64, len(keys))
	byIssue := map[[2]string][]reportstore.CostKey{}
	for _, key := range keys {
		out[key] = 0
		id := [2]string{string(key.ProjectID), string(key.WorkItemID)}
		byIssue[id] = append(byIssue[id], key)
	}
	for _, fact := range facts {
		for _, key := range byIssue[[2]string{string(fact.ProjectID), string(fact.WorkItemID)}] {
			if !fact.Period.Before(key.From) && fact.Period.Before(key.To) {
				out[key] += fact.Cost
			}
		}
	}
	return out
}

func applyPostgresReportUsage(out *hostedReportsReport, work []reportstore.OverviewFact, attempts []reportstore.AttemptFact, facts []reportstore.UsageFact, w, prior operatortool.AnalyticsWindow, compare bool) error {
	if _, err := postgresReportUsageRows(facts); err != nil {
		return err
	}
	current, previous := map[string][]usageRow{}, map[string][]usageRow{}
	byAttempt := map[string]reportstore.AttemptFact{}
	for _, attempt := range attempts {
		byAttempt[attempt.AttemptID] = attempt
	}
	for _, fact := range facts {
		attempt, ok := byAttempt[fact.AttemptID]
		if !ok || attempt.ProjectID != fact.ProjectID || attempt.WorkItemID != fact.WorkItemID {
			return fmt.Errorf("typed report usage has no scoped attempt")
		}
		rows, err := postgresReportUsageRows([]reportstore.UsageFact{fact})
		if err != nil {
			return err
		}
		if !fact.Period.Before(w.From) && fact.Period.Before(w.To) {
			current[fact.AttemptID] = append(current[fact.AttemptID], rows[fact.AttemptID]...)
		}
		if !fact.Period.Before(prior.From) && fact.Period.Before(prior.To) {
			previous[fact.AttemptID] = append(previous[fact.AttemptID], rows[fact.AttemptID]...)
		}
	}
	selected := []nativeAnalyticsAttempt{}
	stageDurations, runnerDurations := map[[3]string][]float64{}, map[string][]float64{}
	for _, attempt := range attempts {
		if len(current[attempt.AttemptID]) == 0 && (attempt.StartedAt.Before(w.From) || !attempt.StartedAt.Before(w.To)) {
			continue
		}
		identity := agentidentity.Identity{Role: attempt.Role, ResolvedModel: agentidentity.NewValue(attempt.Model, agentidentity.ProvenanceRuntime), ReasoningEffort: agentidentity.NewValue(attempt.Effort, agentidentity.ProvenanceRuntime)}
		selected = append(selected, nativeAnalyticsAttempt{AttemptID: attempt.AttemptID, WorkItemID: string(attempt.WorkItemID), Status: attempt.Status, StartedAt: attempt.StartedAt, ObservedAt: attempt.OutcomeAt, Identity: identity})
		if (attempt.Status == "succeeded" || attempt.Status == "failed") && attempt.DurationSeconds > 0 {
			key := [3]string{attempt.Role, attempt.Model, attempt.Effort}
			for i := range key {
				if key[i] == "" {
					key[i] = "Unavailable"
				}
			}
			stageDurations[key] = append(stageDurations[key], attempt.DurationSeconds)
			runnerDurations[attempt.RunnerID] = append(runnerDurations[attempt.RunnerID], attempt.DurationSeconds)
		}
	}
	out.Stages, out.Spend = reportsAttemptTotals(selected, current)
	out.Runners = reportsRunnerTotals(selected, current)
	for i := range out.Stages {
		out.Stages[i].Duration = sampledDuration(stageDurations[[3]string{out.Stages[i].Stage, out.Stages[i].Model, out.Stages[i].Effort}])
	}
	for i := range out.Runners {
		out.Runners[i].Duration = sampledDuration(runnerDurations[out.Runners[i].RunnerID])
	}
	out.Cost, out.Tokens, out.Input, out.Cached = 0, 0, 0, 0
	priorCost := 0.0
	issueCost := map[string]float64{}
	for _, rows := range current {
		for _, row := range rows {
			out.Cost += row.Cost
			out.Tokens += row.tokens()
			out.Input += row.Input
			out.Cached += row.CachedInput
			issueCost[string(byAttempt[row.AttemptID].WorkItemID)] += row.Cost
		}
	}
	for _, rows := range previous {
		for _, row := range rows {
			priorCost += row.Cost
		}
	}
	if compare {
		out.PriorCost = &priorCost
	}
	shipped, priorShipped := map[string]bool{}, map[string]bool{}
	titles, states := map[string]string{}, map[string]string{}
	for _, item := range work {
		id := string(item.WorkItemID)
		titles[id], states[id] = item.Title, item.State
		if item.CompletedAt != nil && item.CompletedState == "Done" {
			if !item.CompletedAt.Before(w.From) && item.CompletedAt.Before(w.To) {
				shipped[id] = true
			}

		}
		if item.PreviousCompletedAt != nil && item.PreviousCompletedState == "Done" && !item.PreviousCompletedAt.Before(prior.From) && item.PreviousCompletedAt.Before(prior.To) {
			priorShipped[id] = true
		}
	}
	counts := map[string]int{}
	for _, attempt := range selected {
		counts[attempt.WorkItemID]++
	}
	for i := range out.Spend {
		item := &out.Spend[i]
		item.Title, item.State, item.Shipped, item.Attempts = titles[item.WorkItemID], states[item.WorkItemID], shipped[item.WorkItemID], counts[item.WorkItemID]
	}
	out.Completion.Cost, out.Previous.Cost = 0, 0
	costSamples := []float64{}
	for id := range shipped {
		out.Completion.Cost += issueCost[id]
		costSamples = append(costSamples, issueCost[id])
	}
	for _, rows := range previous {
		for _, row := range rows {
			if priorShipped[string(byAttempt[row.AttemptID].WorkItemID)] {
				out.Previous.Cost += row.Cost
			}
		}
	}
	if len(costSamples) > 0 {
		spread := sampledDuration(costSamples)
		out.Completion.CostP50 = &spread.P50Seconds
		out.Completion.CostP90 = &spread.P90Seconds
	}
	index := reportsSeriesIndex(reportsSeriesSpans(w))
	for i := range out.Series {
		out.Series[i].StageCost = map[string]float64{}
		out.Series[i].NotShippedCost = 0
	}
	for attemptID, rows := range current {
		for _, row := range rows {
			if i := index(row.Period); i >= 0 {
				attempt := byAttempt[attemptID]
				if !shipped[string(attempt.WorkItemID)] {
					out.Series[i].NotShippedCost += row.Cost
				} else {
					stage := attempt.Role
					if stage == "" {
						stage = reportsOtherStage
					}
					out.Series[i].StageCost[stage] += row.Cost
				}
			}
		}
	}
	failureCosts := map[reportClass]float64{}
	for _, attempt := range attempts {
		if attempt.Failed && !attempt.OutcomeAt.Before(w.From) && attempt.OutcomeAt.Before(w.To) {
			class := reportClass{Name: attempt.FailureClass, Kind: attempt.FailureKind}
			for _, row := range current[attempt.AttemptID] {
				failureCosts[class] += row.Cost
			}
		}
	}
	for i := range out.Failures {
		out.Failures[i].Cost = failureCosts[reportClass{Name: out.Failures[i].Class, Kind: out.Failures[i].Kind}]
	}
	rework := reportReworkClasses(out.Analytics.Quality.returns, selected, current)
	for i := range out.Rework {
		for _, cost := range rework {
			if out.Rework[i].Class == cost.Class && out.Rework[i].Kind == cost.Kind {
				out.Rework[i].Cost = cost.Cost
			}
		}
	}
	out.Unavailable = postgresReportRemoveUnavailable(out.Unavailable, "recorded_usage", "stage_usage", "runner_usage")
	out.Analytics.Unavailable = postgresReportRemoveUnavailable(out.Analytics.Unavailable, "recorded_usage", "stage_usage", "runner_usage")
	return nil
}
