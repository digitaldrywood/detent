package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedReportsDetailPostgres(c echo.Context, credential hubapi.APICredential) error {
	ctx := c.Request().Context()
	params := c.QueryParams()
	project := params.Get("project")
	if params.Get("scope") != "" || project == "" {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Project report scope requires a project"))
	}
	projects, found, err := s.reportsReadableProjects(ctx, credential, project)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if project != reportsAllProjects {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: projects[0]}); err != nil {
			return s.nativeAPIError(c, hubapi.NativeNotFound())
		}
	}
	w, err := reportsWindow(params, s.config.Clock())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	filter, err := parseReportsFilter(params)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	compare := params.Get("compare")
	if compare != "" && compare != "prior" && compare != "off" {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("compare must be prior or off"))
	}
	selectedProjects := reportsQueryValues(params, "projects")
	if len(selectedProjects) > reportsMaxFilterValues {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Report project filter accepts at most 32 values"))
	}
	projects = filterProductivityProjects(projects, selectedProjects)
	if len(projects) == 0 {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	report, err := s.readPostgresHostedReport(ctx, credential, projects, w, filter, compare != "off")
	if err != nil {
		s.config.Logger.Warn("read PostgreSQL report", "error", err)
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Reports are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readPostgresHostedReport(ctx context.Context, credential hubapi.APICredential, projects []string, w operatortool.AnalyticsWindow, filter reportsFilter, compare bool) (hostedReportsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	ids := projectIDs(projects)
	priorities := make([]int, len(filter.Priorities))
	for i, value := range filter.Priorities {
		priorities[i] = mustAtoi(value)
	}
	query := reportstore.OverviewQuery{Projects: ids, Principal: reportPrincipal(credential), From: prior.From, Split: w.From, To: w.To, Limit: 5000, Filter: reportstore.Filter{Labels: filter.Labels, Priorities: priorities, Types: filter.Types, FiledBy: filter.FiledBy, Runners: filter.Runners, Models: filter.Models, Stages: filter.Stages}}
	var openFacts []reportstore.OpenFact
	var openFactsAvailable bool
	var facts []reportstore.OverviewFact
	var attempts []reportstore.AttemptFact
	var history []reportstore.ProjectHistory
	var landings []reportstore.LandingFact
	var escapes []reportstore.EscapeFact
	var attemptFactsAvailable bool
	var costs map[reportstore.CostKey]float64
	var costAvailable bool
	var usage postgresReportUsage
	var completionCosts map[reportstore.CompletionCostItem]reportstore.CompletionCostFact
	var priorRecorded bool
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return errors.New("PostgreSQL report reader is unavailable")
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
		if openReader, ok := reader.(reportstore.OpenReader); ok && len(query.Filter.Models) == 0 && len(query.Filter.Stages) == 0 {
			openFacts, err = openReader.ReadOpenFacts(ctx, reportstore.OpenQuery{Projects: ids, Principal: query.Principal, At: w.To, Filter: query.Filter, Limit: query.Limit})
			if err != nil {
				return err
			}
			openFactsAvailable = true
		}
		historyReader, ok := reader.(reportstore.HistoryReader)
		if !ok {
			return errors.New("PostgreSQL report history reader is unavailable")
		}
		history, err = historyReader.ReadHistory(ctx, query)
		if err != nil {
			return err
		}
		landingReader, ok := reader.(reportstore.LandingReader)
		if !ok {
			return errors.New("PostgreSQL report landing reader is unavailable")
		}
		landings, err = landingReader.ReadLandingFacts(ctx, query)
		if err != nil {
			return err
		}
		qualityReader, ok := reader.(reportstore.QualityReader)
		if !ok {
			return errors.New("PostgreSQL report quality reader is unavailable")
		}
		escapes, err = qualityReader.ReadEscapeFacts(ctx, query)
		if err != nil {
			return err
		}
		usage, err = readPostgresReportUsage(ctx, reader, query, facts, w, prior)
		if err != nil {
			return err
		}
		if _, ok := reader.(reportstore.AttemptUsageReader); ok {
			attempts, attemptFactsAvailable = usage.Attempts, true
		} else if attemptReader, ok := reader.(reportstore.AttemptReader); ok {
			attempts, err = attemptReader.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: ids, Principal: query.Principal, From: prior.From, To: w.To, Filter: query.Filter, Limit: 50000})
			attemptFactsAvailable = err == nil
			if err != nil {
				return err
			}
		}
		attemptFactsAvailable = attemptFactsAvailable && !slices.ContainsFunc(attempts, func(attempt reportstore.AttemptFact) bool { return !attempt.ProjectionKnown })
		costs, costAvailable = usage.Costs, usage.CostsAvailable

		completionCosts, err = readPostgresReportCompletionCosts(ctx, reader, query, facts, w, prior, compare)
		if err != nil {
			return err
		}

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
	trackingPartial, err := s.populatePostgresReportTracking(ctx, credential, projects, attempts, w, &out)
	if err != nil {
		return hostedReportsReport{}, err
	}
	if trackingPartial {
		out.Unavailable = append(out.Unavailable, "failure_tracking_partial")
		out.Analytics.Unavailable = append(out.Analytics.Unavailable, "failure_tracking_partial")
		out.Analytics.Partial = true
	}

	if usage.UsageAvailable {
		if err := applyPostgresReportUsage(&out, facts, attempts, usage.Facts, w, prior, compare); err != nil {
			return hostedReportsReport{}, err
		}
	}
	applyPostgresReportCompletionCosts(&out, facts, completionCosts, w, prior, compare)
	if openFactsAvailable {
		out.OpenWork, out.Unavailable = postgresReportOpenWork(openFacts, w.To, out.Unavailable)
	}
	markPostgresReportCoverage(&out, history, attempts)
	if !compare {
		out = withoutPriorPeriod(out)
	}
	return out, nil
}
