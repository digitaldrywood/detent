package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedReportsDetailPostgres(c echo.Context, credential apiCredential) error {
	ctx := c.Request().Context()
	params := c.QueryParams()
	project := params.Get("project")
	if params.Get("scope") != "" || project == "" {
		return s.nativeAPIError(c, nativePublicInvalid("Project report scope requires a project"))
	}
	projects, found, err := s.reportsReadableProjects(ctx, credential, project)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if project != reportsAllProjects {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: projects[0]}); err != nil {
			return s.nativeAPIError(c, nativeNotFound())
		}
	}
	w, err := reportsWindow(params, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	filter, err := parseReportsFilter(params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	compare := params.Get("compare")
	if compare != "" && compare != "prior" && compare != "off" {
		return s.nativeAPIError(c, nativePublicInvalid("compare must be prior or off"))
	}
	selectedProjects := reportsQueryValues(params, "projects")
	if len(selectedProjects) > reportsMaxFilterValues {
		return s.nativeAPIError(c, nativePublicInvalid("Report project filter accepts at most 32 values"))
	}
	projects = filterProductivityProjects(projects, selectedProjects)
	if len(projects) == 0 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	report, err := s.readPostgresHostedReport(ctx, credential, projects, w, filter, compare != "off")
	if err != nil {
		s.config.Logger.Warn("read PostgreSQL report", "error", err)
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Reports are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readPostgresHostedReport(ctx context.Context, credential apiCredential, projects []string, w operatortool.AnalyticsWindow, filter reportsFilter, compare bool) (hostedReportsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	ids := projectIDs(projects)
	priorities := make([]int, len(filter.Priorities))
	for i, value := range filter.Priorities {
		priorities[i] = mustAtoi(value)
	}
	query := reportstore.OverviewQuery{Projects: ids, Principal: reportPrincipal(credential), From: prior.From, Split: w.From, To: w.To, Limit: 5000, Filter: reportstore.Filter{Labels: filter.Labels, Priorities: priorities, Types: filter.Types, FiledBy: filter.FiledBy, Runners: filter.Runners, Models: filter.Models, Stages: filter.Stages}}
	var facts []reportstore.OverviewFact
	var attempts []reportstore.AttemptFact
	var history []reportstore.ProjectHistory
	var landings []reportstore.LandingFact
	var escapes []reportstore.EscapeFact
	var attemptFactsAvailable bool
	var costs map[reportstore.CostKey]float64
	var costAvailable bool
	var usage postgresReportUsage
	var priorRecorded bool
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return fmt.Errorf("PostgreSQL report reader is unavailable")
		}
		var err error
		priorRecorded, err = store.HasPriorData(ctx, ids, prior.To, query.Principal)
		if err != nil {
			return err
		}
		facts, err = store.ReadOverviewFacts(ctx, query)
		if err != nil {
			return err
		}
		if attemptReader, ok := reader.(reportstore.AttemptReader); ok {
			attempts, err = attemptReader.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: ids, Principal: query.Principal, From: prior.From, To: w.To, Filter: query.Filter, Limit: 50000})
			attemptFactsAvailable = err == nil
			if err != nil {
				return err
			}
		}
		historyReader, ok := reader.(reportstore.HistoryReader)
		if !ok {
			return fmt.Errorf("PostgreSQL report history reader is unavailable")
		}
		history, err = historyReader.ReadHistory(ctx, query)
		if err != nil {
			return err
		}
		landingReader, ok := reader.(reportstore.LandingReader)
		if !ok {
			return fmt.Errorf("PostgreSQL report landing reader is unavailable")
		}
		landings, err = landingReader.ReadLandingFacts(ctx, query)
		if err != nil {
			return err
		}
		qualityReader, ok := reader.(reportstore.QualityReader)
		if !ok {
			return fmt.Errorf("PostgreSQL report quality reader is unavailable")
		}
		escapes, err = qualityReader.ReadEscapeFacts(ctx, query)
		if err != nil {
			return err
		}
		for _, attempt := range attempts {
			if !attempt.ProjectionKnown {
				return fmt.Errorf("PostgreSQL report attempt projection is missing")
			}
		}
		usage, err = readPostgresReportUsage(ctx, reader, query, facts, w, prior)
		if err != nil {
			return err
		}
		costs, costAvailable = usage.Costs, usage.CostsAvailable

		return err
	})
	if err != nil {
		return hostedReportsReport{}, fmt.Errorf("read typed PostgreSQL report facts: %w", err)
	}
	out := summarizePostgresHostedReport(facts, attempts, costs, costAvailable, attemptFactsAvailable, priorRecorded, projects, w, prior, compare)
	applyPostgresReportHistory(&out, history, facts, w, prior)
	applyPostgresReportLandings(&out, landings, w, prior, compare)
	applyPostgresReportQuality(&out, history, landings, escapes, facts, w, prior)
	applyPostgresReportFailures(&out, attempts, w, prior, compare)
	if usage.UsageAvailable {
		if err := applyPostgresReportUsage(&out, facts, attempts, usage.Facts, w, prior, compare); err != nil {
			return hostedReportsReport{}, err
		}
	}
	if !compare {
		out = withoutPriorPeriod(out)
	}
	return out, nil
}

func postgresReportOpenWork(facts []reportstore.OpenFact, at time.Time, unavailable []string) (reportsOpenWork, []string) {
	out := reportsOpenWork{Items: []operatortool.AnalyticsAging{}}
	for _, fact := range facts {
		group := analyticsResidenceGroup(fact.State, map[string]tracker.NativeState{fact.State: {Name: fact.State, Terminal: fact.Terminal}})
		if group != "system" && group != "held" {
			if fact.State == "" {
				unavailable = append(unavailable, "current_open_work_state_unavailable")
			}
			continue
		}
		if group == "held" {
			out.Held++
		} else {
			out.Count++
		}
		if fact.EnteredAt == nil {
			unavailable = append(unavailable, "current_open_work_age_partial")
			continue
		}
		hours := at.Sub(*fact.EnteredAt).Hours()
		if hours > reportsStuckAfter.Hours() {
			out.Stuck++
		}
		item := operatortool.AnalyticsAging{WorkItemID: string(fact.WorkItemID), Lane: fact.State, EnteredAt: *fact.EnteredAt, Hours: hours}
		if fact.FirstWorkedAt != nil {
			since := at.Sub(*fact.FirstWorkedAt).Hours()
			item.SinceTodoHours = &since
		}
		out.Items = append(out.Items, item)
	}
	slices.SortFunc(out.Items, func(a, b operatortool.AnalyticsAging) int {
		switch {
		case a.Hours > b.Hours:
			return -1
		case a.Hours < b.Hours:
			return 1
		default:
			return strings.Compare(a.WorkItemID, b.WorkItemID)
		}
	})
	out.Items = out.Items[:min(len(out.Items), reportsOpenWorkRows)]
	return out, slices.Compact(slices.Sorted(slices.Values(unavailable)))
}

func summarizePostgresHostedReport(facts []reportstore.OverviewFact, attempts []reportstore.AttemptFact, costs map[reportstore.CostKey]float64, costAvailable, attemptFactsAvailable, priorRecorded bool, projects []string, w, prior operatortool.AnalyticsWindow, compare bool) hostedReportsReport {
	unavailable := []string{"lane_residence_and_unclaimed_waits_unavailable", "landed_first_try", "failure_classification", "rework_timeline_unavailable", "stage_usage", "runner_usage"}
	if !attemptFactsAvailable {
		unavailable = append(unavailable, "attempt_outcomes_unavailable")
	}
	if !costAvailable {
		unavailable = append(unavailable, "recorded_usage")
	}
	analyticsUnavailable := append([]string{}, unavailable...)
	analyticsUnavailable = append(analyticsUnavailable, "escape_classification", "infrastructure_return_classification")
	firstProject := reportsAllProjects
	if len(projects) == 1 {
		firstProject = projects[0]
	}
	quality := analyticsQuality{analyticsQualityCounts: analyticsQualityCounts{Causes: map[string]int{}}, Source: "postgres_typed_work_item_facts", Coverage: "bounded issue and workflow facts; unrecorded evidence marked unavailable", Partial: true, Unavailable: []string{"escape_classification", "infrastructure_return_classification", "rework_timeline_unavailable"}, Buckets: []analyticsQualityBucket{}, Occurrences: operatortool.ReadPage[analyticsQualityEscape]{Items: []analyticsQualityEscape{}}}
	residence := operatortool.AnalyticsResidence{AnalyticsResidenceSummary: operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}, Partial: true}, Source: "postgres_typed_work_item_facts", Coverage: "lane residence and current aging are not available in typed PostgreSQL reports", Issues: operatortool.ReadPage[operatortool.AnalyticsIssueResidence]{Items: []operatortool.AnalyticsIssueResidence{}}, Aging: operatortool.ReadPage[operatortool.AnalyticsAging]{Items: []operatortool.AnalyticsAging{}}}
	projectAnalytics := nativeAnalyticsProject{ProjectID: firstProject, Source: "postgres_typed_work_item_facts", SourceAt: w.To, Window: w, Quality: quality, LaneResidence: residence, FailureSignatures: operatortool.ReadPage[nativeFailureSignature]{Items: []nativeFailureSignature{}}, Unavailable: analyticsUnavailable}
	spans := reportsSeriesSpans(w)
	priorSpans := make([]reportsSeriesSpan, len(spans))
	for i, span := range spans {
		priorSpans[i] = reportsSeriesSpan{from: span.from.Add(-w.To.Sub(w.From)), to: span.to.Add(-w.To.Sub(w.From))}
	}
	out := hostedReportsReport{
		Pipeline:   pipelineReport{Stages: []pipelineStage{}, Partial: true, Source: "pipeline receipts are unavailable in typed PostgreSQL report reads"},
		Analytics:  projectAnalytics,
		Completion: reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}, Partial: true},
		Previous:   reportsCompletion{Lanes: []operatortool.AnalyticsLaneDuration{}, Partial: true},
		Series:     []reportsSeriesBucket{}, Runners: []reportsRunner{}, OpenWork: reportsOpenWork{Items: []operatortool.AnalyticsAging{}}, Prior: reportsPrior{LaneSeconds: map[string]float64{}},
		Stages: []reportsStage{}, Spend: []reportsSpend{}, Throughput: []reportsBucket{}, Titles: map[string]string{}, Coverage: []diagnosticsCoverage{},
		Rework: []reportClassTotal{}, Failures: []reportClassTotal{}, Unavailable: unavailable, PriorRecorded: priorRecorded,
	}
	if costAvailable && compare {
		out.PriorCost = new(float64)
	}
	cycleByBucket := make([][]float64, len(spans))
	priorIndex := reportsSeriesIndex(priorSpans)
	currentIndex := reportsSeriesIndex(spans)
	for _, span := range spans {
		out.Series = append(out.Series, reportsSeriesBucket{From: span.from, To: span.to, LaneSeconds: map[string]float64{}, StageCost: map[string]float64{}, ReworkClasses: map[string]int{}})
		out.Throughput = append(out.Throughput, reportsBucket{From: span.from, To: span.to})
	}
	currentCycles, previousCycles := []float64{}, []float64{}
	currentWorked, currentReworked, currentLanded := 0, 0, 0
	previousWorked, previousReworked := 0, 0
	failedIssues := map[string]bool{}
	for _, fact := range facts {
		id := string(fact.WorkItemID)
		out.Titles[id] = fact.Title
		currentKey := reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: w.From, To: w.To}
		previousKey := reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: prior.From, To: prior.To}
		currentCost, previousCost := costs[currentKey], costs[previousKey]
		if costAvailable {
			out.Cost += currentCost
			if compare {
				*out.PriorCost += previousCost
			}
		}
		if fact.Worked {
			currentWorked++
		}
		if fact.Reworked {
			currentReworked++
		}
		if fact.PreviousWorked {
			previousWorked++
		}
		if fact.PreviousReworked {
			previousReworked++
		}
		if fact.ChangesLanded {
			currentLanded++
		}
		out.FailedIssues = len(failedIssues)
		for i := range out.Series {
			out.Series[i].ReworkClasses["Not classified"] += boolInt(fact.Reworked && fact.CompletedAt != nil && !fact.CompletedAt.Before(spans[i].from) && fact.CompletedAt.Before(spans[i].to))
		}
		if fact.PreviousCompletedAt != nil {
			at := *fact.PreviousCompletedAt
			if !at.Before(prior.From) && at.Before(prior.To) {
				if fact.PreviousCompletedState == "Done" {
					out.Previous.Done++
					if fact.PreviousCycleSeconds != nil {
						previousCycles = append(previousCycles, *fact.PreviousCycleSeconds)
					}
					if i := priorIndex(at); i >= 0 {
						out.Series[i].PriorDone++
					}
					if costAvailable {
						out.Previous.Cost += previousCost
					}
				} else if fact.PreviousCompletedState == "Cancelled" {
					out.Previous.Cancelled++
				}
			}
		}
		if fact.CompletedAt == nil {
			continue
		}
		at := *fact.CompletedAt
		if !at.Before(w.From) && at.Before(w.To) {
			if fact.CompletedState == "Done" {
				out.Completion.Done++
				if fact.CycleSeconds != nil {
					currentCycles = append(currentCycles, *fact.CycleSeconds)
					if i := currentIndex(at); i >= 0 {
						cycleByBucket[i] = append(cycleByBucket[i], *fact.CycleSeconds)
						out.Series[i].Done++
						out.Throughput[i].Done++
					}
				}
				if costAvailable {
					out.Completion.Cost += currentCost
				}
			} else if fact.CompletedState == "Cancelled" {
				out.Completion.Cancelled++
				out.Cancelled++
			}
		}

	}
	for _, attempt := range attempts {
		id := string(attempt.WorkItemID)
		if attempt.Status != "failed" || attempt.OutcomeAt.Before(w.From) || !attempt.OutcomeAt.Before(w.To) {
			continue
		}
		out.FailedIssues += boolInt(!failedIssues[id])
		failedIssues[id] = true
		if i := currentIndex(attempt.OutcomeAt); i >= 0 {
			out.Series[i].Failed++
			out.Throughput[i].Failed++
		}
	}
	previousFailed := 0
	for _, attempt := range attempts {
		if attempt.Status == "failed" && !attempt.OutcomeAt.Before(prior.From) && attempt.OutcomeAt.Before(prior.To) {
			previousFailed++
		}
	}
	out.Completion.Lead = sampledDuration(currentCycles)
	out.Previous.Lead = sampledDuration(previousCycles)
	if costAvailable {
		for _, fact := range facts {
			shipped := fact.CompletedAt != nil && !fact.CompletedAt.Before(w.From) && fact.CompletedAt.Before(w.To) && fact.CompletedState == "Done"
			out.Spend = append(out.Spend, reportsSpend{WorkItemID: string(fact.WorkItemID), Title: fact.Title, Cost: costs[reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: w.From, To: w.To}], Shipped: shipped, State: fact.State})
		}
	}
	out.Analytics.Quality.WorkedItems = currentWorked
	out.Analytics.Quality.ReworkedItems = currentReworked
	out.Analytics.Quality.LandedVersions = currentLanded
	if currentReworked > 0 {
		workItems := make([]string, 0, currentReworked)
		for _, fact := range facts {
			if fact.Reworked {
				workItems = append(workItems, string(fact.WorkItemID))
			}
		}
		out.Rework = []reportClassTotal{{Class: reportNotClassified.Name, Kind: reportNotClassified.Kind, Count: currentReworked, Issues: currentReworked, WorkItems: workItems}}
	}
	if currentWorked > 0 {
		out.Analytics.Quality.ReworkPercent = new(100 * float64(currentReworked) / float64(currentWorked))
	}
	if previousWorked > 0 {
		out.Prior.ReworkPercent = new(100 * float64(previousReworked) / float64(previousWorked))
	}
	out.Analytics.Quality.Buckets = make([]analyticsQualityBucket, len(out.Series))
	for i := range out.Series {
		out.Series[i].Cycle = sampledDuration(cycleByBucket[i])
		out.Series[i].Lead = out.Series[i].Cycle
		out.Analytics.Quality.Buckets[i] = analyticsQualityBucket{From: out.Series[i].From, To: out.Series[i].To}
		out.Analytics.Quality.Buckets[i].analyticsQualityCounts = analyticsQualityCounts{Causes: map[string]int{}}
	}
	if compare {
		out.PriorFailed = &previousFailed
	}
	out.Tokens, out.Input, out.Cached = 0, 0, 0
	observed := len(facts)
	out.Coverage = []diagnosticsCoverage{{Source: "typed_work_item_facts", Observed: &observed}}
	return out
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func completeReportCosts(keys []reportstore.CostKey, costs map[reportstore.CostKey]float64) bool {
	if len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if _, ok := costs[key]; !ok {
			return false
		}
	}
	return true
}
