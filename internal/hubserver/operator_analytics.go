package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) executeAnalyticsRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, w, err := operatortool.DecodeAnalytics(call.Name, call.Arguments, s.config.Clock().UTC())
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if s.database == nil || s.config.Hosted == nil {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	if s.config.TenantRepository != nil && call.Name == operatortool.Reports && r.Scope == "organization" {
		projects, _, err := s.reportsReadableProjects(ctx, credential, reportsAllProjects)
		if err != nil {
			return operatortool.Result{}, err
		}
		if r.ProjectID != "" {
			projects = filterProductivityProjects(projects, []string{r.ProjectID})
		}
		projects = filterProductivityProjects(projects, r.Projects)
		if r.Range != "" {
			w, err = reportsWindow(url.Values{"range": {r.Range}}, s.config.Clock())
			if err != nil {
				return operatortool.Result{}, err
			}
		}
		filter := reportsFilter{Labels: r.Labels, Priorities: r.Priorities, Types: r.Types, FiledBy: r.FiledBy, Runners: r.Runners}
		if r.Scope == "organization" {
			report, err := s.readOrganizationProductivityPostgres(ctx, credential, projects, w, filter)
			if err != nil {
				return operatortool.Result{}, err
			}
			return hubOperatorResult(report)
		}
	}
	if call.Name == operatortool.Reports && r.Scope == "organization" {
		if r.Range != "" {
			w, err = reportsWindow(url.Values{"range": {r.Range}}, s.config.Clock())
			if err != nil {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
		}
		projects, _, err := s.reportsReadableProjects(ctx, credential, reportsAllProjects)
		if err != nil {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		if r.ProjectID != "" {
			projects = filterProductivityProjects(projects, []string{r.ProjectID})
		}
		projects = filterProductivityProjects(projects, r.Projects)
		filter := reportsFilter{Labels: r.Labels, Priorities: r.Priorities, Types: r.Types, FiledBy: r.FiledBy, Runners: r.Runners}
		report, err := s.readOrganizationProductivityPostgres(ctx, credential, projects, w, filter)
		if err != nil {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		return hubOperatorResult(report)
	}
	if _, _, cursorErr := analyticsPagingContext(ctx, r.PopulationCursor); cursorErr != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	report, err := s.readAnalyticsReport(ctx, credential, r, w)
	if err != nil {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	if call.Name == operatortool.Reports {
		requests, visible, err := s.adminRequestMetrics(ctx)
		if err != nil {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		if visible {
			report.Requests = &requests
		}
	}
	if call.Name == operatortool.TimeSeries {
		history, visible, err := s.adminRouteLatency(ctx, w.From, w.To, w.Bucket)
		if err != nil {
			s.config.Logger.Warn("read route latency history", "error", err)
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		if visible {
			report.RouteLatency = &history
		}
	}
	return hubOperatorResult(report)
}

func (s *Service) readAnalyticsReport(ctx context.Context, credential hubapi.APICredential, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (nativeAnalyticsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	projects, err := s.analyticsReadableProjects(ctx, credential, r)
	if err != nil {
		return nativeAnalyticsReport{}, err
	}
	return s.readAnalyticsReportFor(ctx, credential, r, w, projects)
}

func (s *Service) analyticsReadableProjects(ctx context.Context, credential hubapi.APICredential, r operatortool.AnalyticsRequest) ([]string, error) {
	projects, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return nil, err
	}
	projects = slices.DeleteFunc(projects, func(id string) bool {
		if r.ProjectID != "" && id != r.ProjectID {
			return true
		}
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id})
		return err != nil
	})
	return filterProductivityProjects(projects, r.Projects), nil
}

func (s *Service) readAnalyticsReportFor(ctx context.Context, credential hubapi.APICredential, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow, projects []string) (nativeAnalyticsReport, error) {
	if s.config.TenantRepository == nil {
		return nativeAnalyticsReport{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	_, _, err := analyticsPagingContext(ctx, r.PopulationCursor)
	if err != nil {
		return nativeAnalyticsReport{}, err
	}
	report := nativeAnalyticsReport{OrganizationID: s.config.Hosted.OrganizationID, Projects: []nativeAnalyticsProject{}, ObservedAt: s.config.Clock().UTC(), Window: w}
	sort.Strings(projects)
	end := min(len(projects), r.Offset+r.Limit)
	if end < len(projects) {
		report.NextOffset = &end
	}
	selected := projects[min(r.Offset, len(projects)):end]
	if r.PopulationCursor != "" && len(selected) > 1 {
		return report, operatortool.ErrInvalidArguments
	}
	for _, id := range selected {
		scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), Project: tracker.ProjectID(id), Credential: credential}
		projectCtx, _, err := analyticsPagingContext(context.WithValue(ctx, analyticsPagingKey{}, nil), r.PopulationCursor)
		if err != nil {
			return report, err
		}
		value, err := s.readNativeAnalyticsTenant(projectCtx, scope, r, w)
		if err != nil {
			return report, err
		}
		report.Projects = append(report.Projects, value)
	}
	return report, nil
}

func (s *Service) readNativeAnalyticsTenant(ctx context.Context, scope hubscope.NativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (nativeAnalyticsProject, error) {
	ctx, paging, err := analyticsPagingContext(ctx, r.PopulationCursor)
	if err != nil {
		return nativeAnalyticsProject{}, err
	}
	ctx, err = s.analyticsCohortContext(ctx, scope, r, w)
	if err != nil {
		return nativeAnalyticsProject{}, err
	}
	ctx = s.runDetailContext(ctx)
	out := nativeAnalyticsProject{ProjectID: string(scope.Project), Source: "native_change_landing_and_recorded_runtime", Window: w, PopulationLimit: maxAnalyticsPopulation, Unavailable: []string{"private_instruction_causality", "receipt_efficiency_quantiles"}, Digest: []nativeAnalyticsBucket{}, Efficiency: []nativeAnalyticsPhase{}, SkipReasons: []nativeAnalyticsSkip{}}
	usageDone := timeSpan(ctx, "analytics_usage")
	usagePage, _, err := s.readNativeAnalyticsUsagePopulationTenant(ctx, scope, w)
	usageDone()
	if err != nil {
		return out, err
	}
	out.Partial = usagePage.Partial
	out.CostPerOutcome.Clipped = usagePage.Partial
	if usagePage.ContinuationUnavailable {
		out.Unavailable = append(out.Unavailable, "usage_continuation_requires_secret_keys")
	}
	done := timeSpan(ctx, "analytics_failures")
	failures, partial, failureUnavailable, err := s.readNativeFailuresTenant(ctx, scope, "", w)
	done()
	if err != nil {
		return out, err
	}
	out.Failures = failures
	out.FailureSignatures = nativeFailureSignaturePage(groupNativeFailures(failures), r.RowOffset, r.Limit)
	out.Partial = out.Partial || partial
	out.Unavailable = append(out.Unavailable, failureUnavailable...)
	if partial {
		out.Unavailable = append(out.Unavailable, "failure_signatures_complete_population")
	}
	for _, failure := range failures {
		if failure.At.After(out.SourceAt) {
			out.SourceAt = failure.At
		}
	}
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		to := from.Add(w.Bucket)
		if to.After(w.To) {
			to = w.To
		}
		out.Digest = append(out.Digest, nativeAnalyticsBucket{From: from, To: to})
	}
	done = timeSpan(ctx, "analytics_landings")
	landingPopulation, err := s.readAnalyticsLandingPopulationTenant(ctx, scope, w, "landings", nil, nil)
	done()
	if err != nil {
		return out, err
	}
	landings := landingPopulation.Items
	if landingPopulation.Partial {
		out.Partial = true
		out.CostPerOutcome.Clipped = true
	}
	if landingPopulation.ContinuationUnavailable {
		out.Unavailable = append(out.Unavailable, "landings_continuation_requires_secret_keys")
	}
	out.AllLandings = landings
	out.CostPerOutcome.Shipped = len(landings)
	for _, l := range landings {
		if i := nativeAnalyticsBucketIndex(l.Landing.LandedAt, w); i >= 0 {
			out.Digest[i].Shipped++
		}
		if l.Landing.LandedAt.After(out.SourceAt) {
			out.SourceAt = l.Landing.LandedAt
		}
	}
	out.Landings = operatortool.OffsetPage(landings, r.RowOffset, r.Limit)
	done = timeSpan(ctx, "analytics_attempts")
	population, _, err := s.readNativeAnalyticsAttemptPopulationTenant(ctx, scope, w, "", usagePage.Items, usagePage.Partial)
	done()
	if err != nil {
		return out, err
	}
	attempts := population.Items
	out.Partial = out.Partial || population.Partial || r.PopulationCursor != ""
	if population.ContinuationUnavailable {
		out.Unavailable = append(out.Unavailable, "attempts_continuation_requires_secret_keys")
	}
	if population.Malformed {
		out.Unavailable = append(out.Unavailable, "recorded_phases_malformed")
	}
	if population.Clipped {
		out.CostPerOutcome.Clipped = true
		out.Activity.Partial = true
		for i := range out.Digest {
			out.Digest[i].Activity.Partial = true
		}
	}
	phases := map[string]nativeAnalyticsPhase{}
	for i := range attempts {
		a := &attempts[i]
		for _, p := range a.Phases {
			if p.FinishedAt.IsZero() || !p.FinishedAt.After(p.StartedAt) || p.FinishedAt.Before(w.From) || !p.FinishedAt.Before(w.To) {
				continue
			}
			phase := phases[p.Name]
			phase.Name = p.Name
			phase.Count++
			phase.Seconds += p.FinishedAt.Sub(p.StartedAt).Seconds()
			phase.AverageSeconds = phase.Seconds / float64(phase.Count)
			phases[p.Name] = phase
		}
		if a.ObservedAt.After(out.SourceAt) {
			out.SourceAt = a.ObservedAt
		}
		if len(a.Pipeline) == 0 && a.Landing == nil {
			out.Pipeline.Partial = true
		}
		out.PipelineTimings = append(out.PipelineTimings, a.Pipeline...)
		if a.PipelineDropped > 0 {
			out.Pipeline.Partial = true
		}
		if a.Landing != nil {
			out.PipelineTimings = append(out.PipelineTimings, a.Landing.Pipeline...)
			if a.Landing.PipelineDropped > 0 {
				out.Pipeline.Partial = true
			}
			if result := a.Landing.Gate; result != nil {
				out.PipelineTimings = append(out.PipelineTimings, result.PipelineTiming("landing_validation"))
				if result.Pipeline != nil {
					out.PipelineTimings = append(out.PipelineTimings, result.Pipeline.Timings...)
					out.Pipeline.Partial = out.Pipeline.Partial || result.Pipeline.Dropped > 0
				}
			}
		}
		projectNativeAnalyticsActivity(&out, a, a.ActivityRaw)
	}
	out.AttemptsObserved = len(attempts)
	for _, a := range attempts {
		if i := nativeAnalyticsBucketIndex(a.StartedAt, w); i >= 0 {
			out.Digest[i].Sessions++
		}
	}
	out.AllAttempts = attempts
	out.Attempts = nativeAnalyticsAttemptPage(attempts, r.RowOffset, r.Limit)
	if out.Activity.ProfilesObserved == 0 {
		out.Unavailable = append(out.Unavailable, "instruction_activity")
	}
	if out.Activity.Partial {
		out.Unavailable = append(out.Unavailable, "activity_complete_window")
	}

	for _, p := range phases {
		out.Efficiency = append(out.Efficiency, p)
	}
	sort.Slice(out.Efficiency, func(i, j int) bool { return out.Efficiency[i].Name < out.Efficiency[j].Name })
	if len(phases) == 0 {
		out.Unavailable = append(out.Unavailable, "recorded_phases")
	}
	done = timeSpan(ctx, "analytics_skips")
	err = s.projectNativeAnalyticsSkipsTenant(ctx, scope, &out)
	done()
	if err != nil {
		return out, err
	}
	done = timeSpan(ctx, "analytics_capacity_wait")
	err = s.projectNativeAnalyticsCapacityWaitTenant(ctx, scope, &out)
	done()
	if err != nil {
		return out, err
	}
	done = timeSpan(ctx, "analytics_residence")
	err = s.projectNativeAnalyticsResidenceTenant(ctx, scope, r, &out)
	done()
	if err != nil {
		return out, err
	}
	out.PopulationCursors = paging.Next
	out.Partial = out.Partial || paging.Name != ""
	pipelinePartial := out.Pipeline.Partial
	out.Pipeline = summarizePipelineTimings(out.PipelineTimings, w)
	out.Pipeline.Partial = out.Pipeline.Partial || pipelinePartial || population.Partial || out.LaneResidence.Partial || out.Partial
	if len(out.Pipeline.Stages) == 0 {
		out.Pipeline.Partial = true
		out.Unavailable = append(out.Unavailable, "recorded_pipeline_timings")
	}
	out.PipelineReceipts = pipelineTimingPage(uniquePipelineTimings(out.PipelineTimings, w), r.RowOffset, r.Limit)
	done = timeSpan(ctx, "analytics_quality")
	out.Quality, err = s.readAnalyticsQualityTenant(ctx, scope, r, w)
	done()
	if err != nil {
		return out, err
	}
	out.Partial = out.Partial || out.Quality.Partial
	out.Pipeline.Partial = out.Pipeline.Partial || out.Partial || len(paging.Next) > 0
	if paging.Name == "usage" || paging.Name == "attempts" || paging.Name == "landings" {
		out.CostPerOutcome.Clipped = true
	}
	out.UsageRowsObserved = len(usagePage.Items)
	var cost float64
	var tokens int64
	sessions := map[string]bool{}
	for _, row := range usagePage.Items {
		cost += row.Cost
		tokens += row.Input + row.Output
		sessions[row.AttemptID] = true
		if index := nativeAnalyticsBucketIndex(row.Period, w); index >= 0 {
			out.Digest[index].Cost += row.Cost
			out.Digest[index].Tokens += row.Input + row.Output
		}
		if row.Period.After(out.SourceAt) {
			out.SourceAt = row.Period
		}
	}
	out.CostPerOutcome.PopulationObserved = len(sessions)
	if !out.CostPerOutcome.Clipped {
		total := len(sessions)
		out.CostPerOutcome.PopulationTotal = &total
	}
	if out.CostPerOutcome.Shipped > 0 && len(sessions) > 0 {
		c := cost / float64(out.CostPerOutcome.Shipped)
		t := float64(tokens) / float64(out.CostPerOutcome.Shipped)
		out.CostPerOutcome.CostPerShipped = &c
		out.CostPerOutcome.TokensPerShipped = &t
	}
	if out.CostPerOutcome.Clipped {
		out.Unavailable = append(out.Unavailable, "cost_per_outcome_complete_population")
	}
	if !w.From.Equal(w.From.Truncate(time.Hour)) || !w.To.Equal(w.To.Truncate(time.Hour)) {
		out.Unavailable = append(out.Unavailable, "usage_partial_hour_attribution")
	}
	if len(sessions) == 0 && !usagePage.Partial {
		out.Unavailable = append(out.Unavailable, "recorded_usage")
	}
	if out.SourceAt.IsZero() {
		out.Unavailable = append(out.Unavailable, "recorded_source_time")
	}
	return out, nil
}

func (s *Service) analyticsSnapshotPrincipal(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (string, error) {
	if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
		return "", err
	}
	member, err := r.AuthorizeAccess(ctx, s.accessAuthority(scope, false), s.config.Clock())
	if err != nil {
		return "", err
	}
	if scope.Credential.HostedRole != "reporter" && member.Role != "reporter" {
		return "", nil
	}
	principal := member.PrincipalID
	if principal == "" {
		principal = scope.Actor().PrincipalID
	}
	if principal == "" {
		return "", hubapi.NativeNotFound()
	}
	return principal, nil
}

func (s *Service) analyticsCohortContext(ctx context.Context, scope hubscope.NativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (context.Context, error) {
	if len(r.Labels)+len(r.Priorities)+len(r.Types)+len(r.FiledBy)+len(r.Runners) == 0 {
		return ctx, nil
	}
	filter := reportstore.Filter{Labels: slices.Clone(r.Labels), Types: slices.Clone(r.Types), FiledBy: slices.Clone(r.FiledBy), Runners: slices.Clone(r.Runners)}
	for _, value := range r.Priorities {
		filter.Priorities = append(filter.Priorities, mustAtoi(value))
	}
	slices.Sort(filter.Labels)
	filter.Labels = slices.Compact(filter.Labels)
	slices.Sort(filter.Types)
	filter.Types = slices.Compact(filter.Types)
	slices.Sort(filter.FiledBy)
	filter.FiledBy = slices.Compact(filter.FiledBy)
	slices.Sort(filter.Runners)
	filter.Runners = slices.Compact(filter.Runners)
	slices.Sort(filter.Priorities)
	filter.Priorities = slices.Compact(filter.Priorities)
	raw, err := json.Marshal(filter)
	if err != nil {
		return ctx, err
	}
	digest := sha256.Sum256(raw)
	selection := &analyticsCohortSelection{Runners: slices.Clone(filter.Runners), Items: map[tracker.NativeWorkItemID]bool{}, Fingerprint: hex.EncodeToString(digest[:])}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return ctx, hubapi.NativeWorkItemStorageUnavailable()
	}
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, reader, scope)
		if err != nil {
			return err
		}
		owner, ok := reader.(reportstore.CohortReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		items, err := owner.ReadCohortWorkItems(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{scope.Project}, Principal: principal, From: w.From, To: w.To, Filter: filter, Limit: 5000})
		if err != nil {
			return err
		}
		for _, item := range items {
			selection.Items[item] = true
		}
		return nil
	})
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, analyticsCohortKey{}, selection), nil
}
