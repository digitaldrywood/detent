package hubserver

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type postgresProductivityMetrics struct {
	Filed            int      `json:"filed"`
	Completed        int      `json:"completed"`
	Cancelled        int      `json:"cancelled"`
	NetBacklog       int      `json:"net_backlog"`
	ChangesLanded    *int     `json:"changes_landed"`
	Worked           int      `json:"worked"`
	Reworked         int      `json:"reworked"`
	CompletionCost   *float64 `json:"completion_cost_usd"`
	CostPerCompleted *float64 `json:"cost_per_completed_usd"`
	CycleP50         *float64 `json:"cycle_p50_seconds"`
	ReworkRate       *float64 `json:"rework_percent"`
	cycles           []float64
}

type postgresProductivityProject struct {
	ProjectID string                      `json:"project_id"`
	Current   postgresProductivityMetrics `json:"current"`
	Previous  postgresProductivityMetrics `json:"previous"`
}

type postgresOrganizationProductivity struct {
	OrganizationID string                        `json:"organization_id"`
	Window         operatortool.AnalyticsWindow  `json:"window"`
	PreviousWindow operatortool.AnalyticsWindow  `json:"previous_window"`
	PriorRecorded  bool                          `json:"prior_recorded"`
	Current        postgresProductivityMetrics   `json:"current"`
	Previous       postgresProductivityMetrics   `json:"previous"`
	Projects       []postgresProductivityProject `json:"projects"`
	Series         []productivityBucket          `json:"series"`
	PreviousSeries []productivityBucket          `json:"previous_series"`
	FiledBy        []productivityActor           `json:"filed_by"`
	CompletedBy    []productivityActor           `json:"completed_by"`
	Unavailable    []string                      `json:"unavailable"`
}

func (s *Service) hostedReportsPostgres(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	params := c.QueryParams()
	project := params.Get("project")
	if params.Get("scope") != "organization" {
		return s.hostedReportsDetailPostgres(c, credential)
	}
	if project != reportsAllProjects {
		return s.nativeAPIError(c, nativePublicInvalid("Organization report scope requires all projects"))
	}
	projects, found, err := s.reportsReadableProjects(ctx, credential, reportsAllProjects)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, nativeNotFound())
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
	report, err := s.readOrganizationProductivityPostgres(ctx, credential, projects, w, filter)
	if err != nil {
		s.config.Logger.Warn("read PostgreSQL organization reports", "error", err)
		return s.hostedJSONError(c, 503, "Reports are temporarily unavailable")
	}
	return c.JSON(200, report)
}

func (s *Service) hostedReportOptionsPostgres(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	projects, found, err := s.reportsReadableProjects(ctx, credential, c.QueryParam("project"))
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if c.QueryParam("project") != reportsAllProjects {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: projects[0]}); err != nil {
			return s.nativeAPIError(c, nativeNotFound())
		}
	}
	w, err := reportsWindow(c.QueryParams(), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var facets reportstore.Facets
	principal := reportPrincipal(credential)
	err = s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		reportReader, ok := reader.(reportstore.Reader)
		if !ok {
			return fmt.Errorf("PostgreSQL report reader is unavailable")
		}
		var err error
		facets, err = reportReader.ReadFacets(ctx, projectIDs(projects), w.From, w.To, principal, reportsFacetLimit)
		return err
	})
	if err != nil {
		return s.hostedJSONError(c, 503, "Report filters are temporarily unavailable")
	}
	return c.JSON(200, struct {
		reportsFacets
		Unavailable []string `json:"unavailable"`
	}{reportsFacets{Labels: facets.Labels, FiledBy: convertReportFacets(facets.FiledBy), Runners: convertReportFacets(facets.Runners), Models: facets.Models, Stages: facets.Stages}, facets.Unavailable})
}

func (s *Service) readOrganizationProductivityPostgres(ctx context.Context, credential apiCredential, projectIDsValue []string, w operatortool.AnalyticsWindow, filter reportsFilter) (postgresOrganizationProductivity, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	w.Bucket = time.Hour
	if w.To.Sub(w.From) >= 6*24*time.Hour {
		w.Bucket = 24 * time.Hour
	}
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	projectIDsTyped := projectIDs(projectIDsValue)
	out := postgresOrganizationProductivity{
		OrganizationID: s.config.Hosted.OrganizationID,
		Window:         w,
		PreviousWindow: prior,
		Projects:       make([]postgresProductivityProject, 0, len(projectIDsValue)),
		Series:         productivityBuckets(w, projectIDsValue),
		PreviousSeries: productivityBuckets(prior, projectIDsValue),
		Current:        postgresProductivityMetrics{ChangesLanded: new(int)},
		Previous:       postgresProductivityMetrics{ChangesLanded: new(int)},
		FiledBy:        []productivityActor{},
		CompletedBy:    []productivityActor{},
		Unavailable:    []string{},
	}
	projectIndex := make(map[string]int, len(projectIDsValue))
	for _, project := range projectIDsValue {
		projectIndex[project] = len(out.Projects)
		out.Projects = append(out.Projects, postgresProductivityProject{ProjectID: project})
	}
	priorities := make([]int, len(filter.Priorities))
	for i, value := range filter.Priorities {
		priorities[i] = mustAtoi(value)
	}
	query := reportstore.OverviewQuery{Projects: projectIDsTyped, Principal: reportPrincipal(credential), From: prior.From, Split: w.From, To: w.To, Limit: 5000, Filter: reportstore.Filter{Labels: filter.Labels, Priorities: priorities, Types: filter.Types, FiledBy: filter.FiledBy, Runners: filter.Runners, Models: filter.Models, Stages: filter.Stages}}
	var facts []reportstore.OverviewFact
	var issueCosts map[reportstore.CostKey]float64
	var costAvailable bool
	var priorRecorded bool
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return fmt.Errorf("PostgreSQL report reader is unavailable")
		}
		var err error
		priorRecorded, err = store.HasPriorData(ctx, projectIDsTyped, prior.To, query.Principal)
		if err != nil {
			return err
		}
		facts, err = store.ReadOverviewFacts(ctx, query)
		if err != nil {
			return err
		}
		usage, costErr := readPostgresReportUsage(ctx, reader, query, facts, w, prior)
		if costErr != nil {
			return costErr
		}
		issueCosts, costAvailable = usage.Costs, usage.CostsAvailable

		return err
	})
	if err != nil {
		return out, fmt.Errorf("read PostgreSQL report facts: %w", err)
	}
	if !costAvailable {
		out.Unavailable = append(out.Unavailable, "completion_cost")
	}
	out.PriorRecorded = priorRecorded
	filedBy, completedBy := map[string]*productivityActor{}, map[string]*productivityActor{}
	for _, fact := range facts {
		project := string(fact.ProjectID)
		row := &out.Projects[projectIndex[project]]
		for _, period := range []struct {
			window  operatortool.AnalyticsWindow
			metrics *postgresProductivityMetrics
			series  []productivityBucket
			worked  bool
			rework  bool
			landed  bool
			prior   bool
		}{{w, &row.Current, out.Series, fact.Worked, fact.Reworked, fact.ChangesLanded, false}, {prior, &row.Previous, out.PreviousSeries, fact.PreviousWorked, fact.PreviousReworked, fact.PreviousChangesLanded, true}} {
			metrics := period.metrics
			if period.landed {
				if metrics.ChangesLanded == nil {
					metrics.ChangesLanded = new(int)
				}
				*metrics.ChangesLanded++
			}
			if fact.CreatedAt.Before(period.window.To) && !fact.CreatedAt.Before(period.window.From) {
				metrics.Filed++
				productivityActorCount(filedBy, fact.FilerKind, fact.FilerID, "", period.prior)
				productivityBucketCount(period.series, period.window, fact.CreatedAt, project, false)
			}
			completedAt, completedState, completedKind, completedID, cycle := fact.CompletedAt, fact.CompletedState, fact.CompletedKind, fact.CompletedID, fact.CycleSeconds
			if period.prior {
				completedAt, completedState, completedKind, completedID, cycle = fact.PreviousCompletedAt, fact.PreviousCompletedState, fact.PreviousCompletedKind, fact.PreviousCompletedID, fact.PreviousCycleSeconds
			}
			if completedAt != nil && !completedAt.Before(period.window.From) && completedAt.Before(period.window.To) {
				switch completedState {
				case "Done":
					metrics.Completed++
					productivityActorCount(completedBy, completedKind, completedID, fact.CompletedName, period.prior)
					productivityBucketCount(period.series, period.window, *completedAt, project, true)
					if cycle != nil {
						metrics.cycles = append(metrics.cycles, *cycle)
					}
				case "Cancelled":
					metrics.Cancelled++
				}
			}
			if period.worked {
				metrics.Worked++
			}
			if period.rework {
				metrics.Reworked++
			}
		}
	}
	for i := range out.Projects {
		if costAvailable {
			out.Projects[i].Current.CompletionCost = new(float64)
			out.Projects[i].Previous.CompletionCost = new(float64)
			for _, fact := range facts {
				if string(fact.ProjectID) != out.Projects[i].ProjectID {
					continue
				}
				if fact.CompletedAt != nil && fact.CompletedState == "Done" && !fact.CompletedAt.Before(w.From) && fact.CompletedAt.Before(w.To) {
					if cost, ok := issueCosts[reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: w.From, To: w.To}]; ok {
						*out.Projects[i].Current.CompletionCost += cost
					}
				}
				if fact.PreviousCompletedAt != nil && fact.PreviousCompletedState == "Done" && !fact.PreviousCompletedAt.Before(prior.From) && fact.PreviousCompletedAt.Before(prior.To) {
					if cost, ok := issueCosts[reportstore.CostKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, From: prior.From, To: prior.To}]; ok {
						*out.Projects[i].Previous.CompletionCost += cost
					}
				}
			}
		}
		if out.Projects[i].Current.ChangesLanded == nil {
			out.Projects[i].Current.ChangesLanded = new(int)
		}
		if out.Projects[i].Previous.ChangesLanded == nil {
			out.Projects[i].Previous.ChangesLanded = new(int)
		}
		if costAvailable {
			if out.Current.CompletionCost == nil {
				out.Current.CompletionCost = new(float64)
			}
			if out.Previous.CompletionCost == nil {
				out.Previous.CompletionCost = new(float64)
			}
		}
		finishPostgresProductivityMetrics(&out.Projects[i].Current)
		finishPostgresProductivityMetrics(&out.Projects[i].Previous)
		addPostgresProductivityMetrics(&out.Current, out.Projects[i].Current)
		addPostgresProductivityMetrics(&out.Previous, out.Projects[i].Previous)
	}
	finishPostgresProductivityMetrics(&out.Current)
	finishPostgresProductivityMetrics(&out.Previous)
	out.FiledBy = productivityActors(filedBy)
	out.CompletedBy = productivityActors(completedBy)
	return out, nil
}

func finishPostgresProductivityMetrics(metrics *postgresProductivityMetrics) {
	metrics.NetBacklog = metrics.Filed - metrics.Completed - metrics.Cancelled
	if metrics.CompletionCost != nil && metrics.Completed > 0 {
		metrics.CostPerCompleted = new(*metrics.CompletionCost / float64(metrics.Completed))
	}
	if metrics.Worked > 0 {
		metrics.ReworkRate = new(100 * float64(metrics.Reworked) / float64(metrics.Worked))
	}
	if len(metrics.cycles) > 0 {
		metrics.CycleP50 = new(sampledDuration(metrics.cycles).P50Seconds)
	}
}

func addPostgresProductivityMetrics(to *postgresProductivityMetrics, from postgresProductivityMetrics) {
	to.Filed += from.Filed
	to.Completed += from.Completed
	to.Cancelled += from.Cancelled
	to.Worked += from.Worked
	to.Reworked += from.Reworked
	if to.ChangesLanded == nil || from.ChangesLanded == nil {
		to.ChangesLanded = nil
	} else {
		*to.ChangesLanded += *from.ChangesLanded
	}
	if to.CompletionCost != nil && from.CompletionCost != nil {
		*to.CompletionCost += *from.CompletionCost
	} else {
		to.CompletionCost = nil
	}
	to.cycles = append(to.cycles, from.cycles...)
}

func reportPrincipal(credential apiCredential) string {
	if credential.HostedRole != "reporter" {
		return ""
	}
	return credential.Hosted.Subject
}

func projectIDs(projects []string) []tracker.ProjectID {
	ids := make([]tracker.ProjectID, len(projects))
	for i, project := range projects {
		ids[i] = tracker.ProjectID(project)
	}
	return ids
}

func convertReportFacets(facets []reportstore.Facet) []reportsFacet {
	out := make([]reportsFacet, len(facets))
	for i, facet := range facets {
		out[i] = reportsFacet{ID: facet.ID, Name: facet.Name}
	}
	return out
}

func mustAtoi(value string) int {
	valueInt, _ := strconv.Atoi(value)
	return valueInt
}
