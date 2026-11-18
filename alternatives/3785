package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
	Worked           *int     `json:"worked"`
	Reworked         *int     `json:"reworked"`
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
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Organization report scope requires all projects"))
	}
	projects, found, err := s.reportsReadableProjects(ctx, credential, reportsAllProjects)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	if !found {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
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
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if c.QueryParam("project") != reportsAllProjects {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: projects[0]}); err != nil {
			return s.nativeAPIError(c, hubapi.NativeNotFound())
		}
	}
	w, err := reportsWindow(c.QueryParams(), s.config.Clock())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var facets reportstore.Facets
	principal := reportPrincipal(credential)
	err = s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		reportReader, ok := reader.(reportstore.Reader)
		if !ok {
			return errors.New("PostgreSQL report reader is unavailable")
		}
		var err error
		facets, err = reportReader.ReadFacets(ctx, projectIDs(projects), w.From, w.To, principal, reportsFacetLimit)
		if err != nil {
			return fmt.Errorf("read report facets: %w", err)
		}
		return nil
	})
	if errors.Is(err, reportstore.ErrPopulationExceeded) {
		return s.nativeAPIError(c, &hubapi.NativeError{Code: "report_population_too_large", Message: "Report filter population exceeds its bounded read limit; narrow the range or project", Status: http.StatusUnprocessableEntity, PublicMessage: true})
	}
	if err != nil {
		return s.hostedJSONError(c, 503, "Report filters are temporarily unavailable")
	}
	return c.JSON(200, struct {
		reportsFacets
		Unavailable []string `json:"unavailable"`
	}{reportsFacets{Labels: facets.Labels, FiledBy: convertReportFacets(facets.FiledBy), Runners: convertReportFacets(facets.Runners), Models: facets.Models, Stages: facets.Stages}, facets.Unavailable})
}

func (s *Service) readOrganizationProductivityPostgres(ctx context.Context, credential hubapi.APICredential, projectIDsValue []string, w operatortool.AnalyticsWindow, filter reportsFilter) (postgresOrganizationProductivity, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
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
		Current:        postgresProductivityMetrics{ChangesLanded: new(int), CompletionCost: new(float64), Worked: new(int), Reworked: new(int)},
		Previous:       postgresProductivityMetrics{ChangesLanded: new(int), CompletionCost: new(float64), Worked: new(int), Reworked: new(int)},
		FiledBy:        []productivityActor{},
		CompletedBy:    []productivityActor{},
		Unavailable:    []string{},
	}
	projectIndex := make(map[string]int, len(projectIDsValue))
	for _, project := range projectIDsValue {
		projectIndex[project] = len(out.Projects)
		out.Projects = append(out.Projects, postgresProductivityProject{ProjectID: project})
	}
	if len(projectIDsTyped) == 0 {
		return out, nil
	}
	priorities := make([]int, len(filter.Priorities))
	for i, value := range filter.Priorities {
		priorities[i] = mustAtoi(value)
	}
	query := reportstore.OverviewQuery{Projects: projectIDsTyped, Principal: reportPrincipal(credential), From: prior.From, Split: w.From, To: w.To, Limit: 5000, Filter: reportstore.Filter{Labels: filter.Labels, Priorities: priorities, Types: filter.Types, FiledBy: filter.FiledBy, Runners: filter.Runners, Models: filter.Models, Stages: filter.Stages}}
	presentProjects := make(map[string]bool, len(projectIDsValue))
	if err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		for _, project := range projectIDsValue {
			_, err := reader.Project(ctx, tracker.ProjectID(project))
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			presentProjects[project] = true
		}
		return nil
	}); err != nil {
		return out, fmt.Errorf("read productivity project scope: %w", err)
	}
	qualityByProject := make(map[string][2]analyticsQualityInputs, len(projectIDsValue))
	for _, project := range projectIDsValue {
		if !presentProjects[project] {
			continue
		}
		scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), Project: tracker.ProjectID(project), Credential: credential}
		periods := [2]analyticsQualityInputs{}
		for i, window := range []operatortool.AnalyticsWindow{w, prior} {
			inputs, err := s.readAnalyticsQualityInputsTenant(ctx, scope, window)
			if err != nil {
				return out, fmt.Errorf("read productivity quality: %w", err)
			}
			periods[i] = inputs
			if inputs.Partial || len(inputs.Unavailable) > 0 {
				out.Unavailable = append(out.Unavailable, "rework_timeline_unavailable")
				out.Unavailable = append(out.Unavailable, inputs.Unavailable...)
			}
			for _, event := range inputs.Events {
				query.ActivityWorkItems = append(query.ActivityWorkItems, tracker.NativeWorkItemID(event.Item))
			}
			for _, landing := range inputs.Landings {
				query.ActivityWorkItems = append(query.ActivityWorkItems, landing.WorkItemID)
			}
		}
		qualityByProject[project] = periods
	}

	slices.Sort(query.ActivityWorkItems)
	query.ActivityWorkItems = slices.Compact(query.ActivityWorkItems)
	actorSources := map[string]eventstore.Event{}
	var facts []reportstore.OverviewFact
	var issueCosts map[reportstore.CompletionCostItem]reportstore.CompletionCostFact
	var priorRecorded bool
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return errors.New("PostgreSQL report reader is unavailable")
		}
		var err error
		if projects, ok := reader.(reportstore.PriorProjectReader); ok && query.Principal == "" {
			priorRecorded, err = projects.HasPriorProjects(ctx, projectIDsTyped, prior.To)
		} else {
			priorRecorded, err = store.HasPriorData(ctx, projectIDsTyped, prior.To, query.Principal)
		}
		if err != nil {
			return err
		}
		facts, err = store.ReadOverviewFacts(ctx, query)
		if err != nil {
			return err
		}
		keys := []eventstore.EventKey{}
		for _, fact := range facts {
			ids := []string{}
			filed := !fact.CreatedAt.Before(prior.From) && fact.CreatedAt.Before(w.To)
			if filed && !fact.FilerKnown {
				if !fact.FilerOriginKnown || fact.FilerEventID == "" {
					return errors.New("productivity filer classification is unavailable")
				}
				ids = append(ids, fact.FilerEventID)
			}
			if fact.CompletedState == "Done" && fact.CompletedAt != nil && !fact.CompletedAt.Before(w.From) && fact.CompletedAt.Before(w.To) && !fact.CompletedActorKnown {
				ids = append(ids, fact.CompletedEventID)
			}
			if fact.PreviousCompletedState == "Done" && fact.PreviousCompletedAt != nil && !fact.PreviousCompletedAt.Before(prior.From) && fact.PreviousCompletedAt.Before(prior.To) && !fact.PreviousCompletedActorKnown {
				ids = append(ids, fact.PreviousCompletedEventID)
			}
			for _, id := range ids {
				if id == "" {
					return errors.New("productivity actor source is unavailable")
				}
				keys = append(keys, eventstore.EventKey{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID, ID: id})
			}
		}
		if len(keys) > 0 {
			point, ok := reader.(eventstore.EventsByKeyReader)
			if !ok {
				return errors.New("PostgreSQL event reader is unavailable")
			}
			found, err := point.EventsByKey(ctx, keys)
			if err != nil {
				return err
			}
			for _, key := range keys {
				event, ok := found[key.ID]
				if !ok {
					return corestore.ErrNotFound
				}
				actorSources[key.ID] = event
			}
		}
		issueCosts, err = readPostgresReportCompletionCosts(ctx, reader, query, facts, w, prior, true)
		if err != nil {
			return err
		}

		return err
	})
	if err != nil {
		return out, fmt.Errorf("read PostgreSQL report facts: %w", err)
	}
	actors := map[string]tracker.Actor{}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for id, source := range actorSources {
		event, err := content.Event(ctx, source)
		if err != nil {
			return out, fmt.Errorf("read productivity actor source: %w", err)
		}
		actors[id] = event.Actor
	}
	for i := range facts {
		fact := &facts[i]
		if actor, ok := actors[fact.FilerEventID]; ok && !fact.FilerKnown {
			switch {
			case actor.Kind == "machine" || actor.Kind == "automation" || actor.Kind == "system" || actor.Kind == "integration":
				fact.FilerKind = "automation"
			case actor.Kind == "agent" || actor.Kind == "runner" || actor.Agent != "":
				fact.FilerKind = "agents"
			case actor.Kind == "human":
				fact.FilerKind = "people"
			default:
				fact.FilerKind = "unknown"
			}
			fact.FilerKnown = true
		}
		if actor, ok := actors[fact.CompletedEventID]; ok && !fact.CompletedActorKnown {
			if actor.Kind == "human" && actor.Agent == "" {
				fact.CompletedKind, fact.CompletedID, fact.CompletedName = "people", actor.PrincipalID, actor.DisplayName
			}
			fact.CompletedActorKnown = true
		}
		if actor, ok := actors[fact.PreviousCompletedEventID]; ok && !fact.PreviousCompletedActorKnown {
			if actor.Kind == "human" && actor.Agent == "" {
				fact.PreviousCompletedKind, fact.PreviousCompletedID, fact.PreviousCompletedName = "people", actor.PrincipalID, actor.DisplayName
			}
			fact.PreviousCompletedActorKnown = true
		}
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
			landed  bool
			prior   bool
		}{{w, &row.Current, out.Series, fact.ChangesLanded, false}, {prior, &row.Previous, out.PreviousSeries, fact.PreviousChangesLanded, true}} {
			metrics := period.metrics
			if period.landed {
				if metrics.ChangesLanded == nil {
					metrics.ChangesLanded = new(int)
				}
				*metrics.ChangesLanded++
			}
			if fact.CreatedAt.Before(period.window.To) && !fact.CreatedAt.Before(period.window.From) {
				if !fact.FilerKnown {
					return out, errors.New("productivity filer classification is unavailable")
				}
				metrics.Filed++
				productivityActorCount(filedBy, fact.FilerKind, "", "", period.prior)
				productivityBucketCount(period.series, period.window, fact.CreatedAt, project, false)
			}
			completedAt, completedState, completedKind, completedID, completedName, cycle := fact.CompletedAt, fact.CompletedState, fact.CompletedKind, fact.CompletedID, fact.CompletedName, fact.CycleSeconds
			if period.prior {
				completedAt, completedState, completedKind, completedID, completedName, cycle = fact.PreviousCompletedAt, fact.PreviousCompletedState, fact.PreviousCompletedKind, fact.PreviousCompletedID, fact.PreviousCompletedName, fact.PreviousCycleSeconds
			}
			if completedAt != nil && !completedAt.Before(period.window.From) && completedAt.Before(period.window.To) {
				switch completedState {
				case "Done":
					known := fact.CompletedActorKnown
					if period.prior {
						known = fact.PreviousCompletedActorKnown
					}
					if !known {
						return out, errors.New("productivity completion attribution is unavailable")
					}
					metrics.Completed++
					productivityActorCount(completedBy, completedKind, completedID, completedName, period.prior)
					productivityBucketCount(period.series, period.window, *completedAt, project, true)
					if cycle != nil {
						metrics.cycles = append(metrics.cycles, *cycle)
					}
				case "Cancelled":
					metrics.Cancelled++
				}
			}
		}
	}
	for i := range out.Projects {
		row := &out.Projects[i]
		row.Current.CompletionCost, row.Previous.CompletionCost = new(float64), new(float64)
		currentKnown, previousKnown := true, true
		selected := map[tracker.NativeWorkItemID]bool{}
		for _, fact := range facts {
			if string(fact.ProjectID) != row.ProjectID {
				continue
			}
			selected[fact.WorkItemID] = true
			cost, exists := issueCosts[reportstore.CompletionCostItem{ProjectID: fact.ProjectID, WorkItemID: fact.WorkItemID}]
			if fact.CompletedAt != nil && fact.CompletedState == "Done" && !fact.CompletedAt.Before(w.From) && fact.CompletedAt.Before(w.To) {
				*row.Current.CompletionCost += cost.Cost
				currentKnown = currentKnown && exists && cost.Complete
			}
			if fact.PreviousCompletedAt != nil && fact.PreviousCompletedState == "Done" && !fact.PreviousCompletedAt.Before(prior.From) && fact.PreviousCompletedAt.Before(prior.To) {
				*row.Previous.CompletionCost += cost.PriorCost
				previousKnown = previousKnown && exists && cost.Complete
			}
		}
		if !currentKnown {
			row.Current.CompletionCost = nil
		}
		if !previousKnown {
			row.Previous.CompletionCost = nil
		}
		for i, period := range []struct {
			metrics *postgresProductivityMetrics
			window  operatortool.AnalyticsWindow
		}{{&row.Current, w}, {&row.Previous, prior}} {
			inputs := qualityByProject[row.ProjectID][i]
			if inputs.Partial || len(inputs.Unavailable) > 0 {
				continue
			}
			inputs.Events = slices.DeleteFunc(slices.Clone(inputs.Events), func(event qualityHistoryEvent) bool { return !selected[tracker.NativeWorkItemID(event.Item)] })
			inputs.Landings = slices.DeleteFunc(slices.Clone(inputs.Landings), func(landing nativeAnalyticsLanding) bool { return !selected[landing.WorkItemID] })
			counts := qualityCounts(inputs.Landings, inputs.Events, nil, period.window.From, period.window.To, period.window.To)
			period.metrics.Worked, period.metrics.Reworked = new(counts.WorkedItems), new(counts.ReworkedItems)
		}

		if row.Current.ChangesLanded == nil {
			row.Current.ChangesLanded = new(int)
		}
		if row.Previous.ChangesLanded == nil {
			row.Previous.ChangesLanded = new(int)
		}
		finishPostgresProductivityMetrics(&row.Current)
		finishPostgresProductivityMetrics(&row.Previous)
		addPostgresProductivityMetrics(&out.Current, row.Current)
		addPostgresProductivityMetrics(&out.Previous, row.Previous)
	}

	finishPostgresProductivityMetrics(&out.Current)
	finishPostgresProductivityMetrics(&out.Previous)
	if out.Current.CompletionCost == nil || out.Previous.CompletionCost == nil {
		out.Unavailable = append(out.Unavailable, "completion_cost")
	}
	slices.Sort(out.Unavailable)
	out.Unavailable = slices.Compact(out.Unavailable)
	out.FiledBy = productivityActors(filedBy)
	out.CompletedBy = productivityActors(completedBy)
	return out, nil
}

func finishPostgresProductivityMetrics(metrics *postgresProductivityMetrics) {
	metrics.NetBacklog = metrics.Filed - metrics.Completed - metrics.Cancelled
	if metrics.CompletionCost != nil && metrics.Completed > 0 {
		metrics.CostPerCompleted = new(*metrics.CompletionCost / float64(metrics.Completed))
	}
	if metrics.Worked != nil && metrics.Reworked != nil && *metrics.Worked > 0 {
		metrics.ReworkRate = new(100 * float64(*metrics.Reworked) / float64(*metrics.Worked))
	}
	if len(metrics.cycles) > 0 {
		metrics.CycleP50 = new(hubapi.SampledDuration(metrics.cycles).P50Seconds)
	}
}

func addPostgresProductivityMetrics(to *postgresProductivityMetrics, from postgresProductivityMetrics) {
	to.Filed += from.Filed
	to.Completed += from.Completed
	to.Cancelled += from.Cancelled
	if to.Worked != nil && from.Worked != nil {
		*to.Worked += *from.Worked
	} else {
		to.Worked = nil
	}
	if to.Reworked != nil && from.Reworked != nil {
		*to.Reworked += *from.Reworked
	} else {
		to.Reworked = nil
	}
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

func reportPrincipal(credential hubapi.APICredential) string {
	if credential.HostedRole != "reporter" {
		return ""
	}
	return hubscope.OperatorIdentity(credential, "").PrincipalID
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
	valueInt, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return valueInt
}
