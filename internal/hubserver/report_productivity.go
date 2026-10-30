package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type productivityMetrics struct {
	Filed            int      `json:"filed"`
	Completed        int      `json:"completed"`
	Cancelled        int      `json:"cancelled"`
	NetBacklog       int      `json:"net_backlog"`
	ChangesLanded    int      `json:"changes_landed"`
	Worked           int      `json:"worked"`
	Reworked         int      `json:"reworked"`
	CompletionCost   float64  `json:"completion_cost_usd"`
	CostPerCompleted *float64 `json:"cost_per_completed_usd"`
	CycleP50         *float64 `json:"cycle_p50_seconds"`
	ReworkRate       *float64 `json:"rework_percent"`
	cycles           []float64
}

type productivityProject struct {
	ProjectID string              `json:"project_id"`
	Current   productivityMetrics `json:"current"`
	Previous  productivityMetrics `json:"previous"`
}

type productivityCount struct {
	Filed     int `json:"filed"`
	Completed int `json:"completed"`
}

type productivityBucket struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	productivityCount
	Projects map[string]productivityCount `json:"projects"`
}

type productivityActor struct {
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Current  int    `json:"current"`
	Previous int    `json:"previous"`
}

type organizationProductivity struct {
	OrganizationID string                       `json:"organization_id"`
	Window         operatortool.AnalyticsWindow `json:"window"`
	PreviousWindow operatortool.AnalyticsWindow `json:"previous_window"`
	PriorRecorded  bool                         `json:"prior_recorded"`
	Current        productivityMetrics          `json:"current"`
	Previous       productivityMetrics          `json:"previous"`
	Projects       []productivityProject        `json:"projects"`
	Series         []productivityBucket         `json:"series"`
	PreviousSeries []productivityBucket         `json:"previous_series"`
	FiledBy        []productivityActor          `json:"filed_by"`
	CompletedBy    []productivityActor          `json:"completed_by"`
}

func productivityFilter(projects []string, filter reportsFilter) (string, []any, error) {
	encoded, err := sqlList(projects)
	if err != nil {
		return "", nil, err
	}
	var clause strings.Builder
	clause.WriteString(`s.organization_id=? AND s.project_id IN (SELECT value FROM sql_rows(?))`)
	args := []any{encoded}
	for _, dimension := range []struct {
		values     []string
		expression string
	}{
		{filter.Labels, `EXISTS(SELECT 1 FROM report_issue_labels l WHERE l.organization_id=s.organization_id AND l.project_id=s.project_id AND l.work_item_id=s.work_item_id AND l.label IN (SELECT value FROM sql_rows(?)))`},
		{filter.Priorities, `s.priority IN (SELECT value FROM sql_rows(?))`},
		{filter.Types, `s.issue_type IN (SELECT value FROM sql_rows(?))`},
		{filter.FiledBy, `s.filer_id IN (SELECT value FROM sql_rows(?))`},
	} {
		if len(dimension.values) == 0 {
			continue
		}
		raw, err := sqlList(dimension.values)
		if err != nil {
			return "", nil, err
		}
		clause.WriteString(" AND ")
		clause.WriteString(dimension.expression)
		args = append(args, raw)
	}
	return clause.String(), args, nil
}

func (s *Service) readOrganizationProductivity(ctx context.Context, projects []string, w operatortool.AnalyticsWindow, filter reportsFilter) (organizationProductivity, error) {
	done := timeSpan(ctx, "reports_productivity")
	defer done()
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	w.Bucket = time.Hour
	if w.To.Sub(w.From) >= 6*24*time.Hour {
		w.Bucket = 24 * time.Hour
	}
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	out := organizationProductivity{OrganizationID: s.config.Hosted.OrganizationID, Window: w, PreviousWindow: prior, Projects: []productivityProject{}, Series: productivityBuckets(w, projects), PreviousSeries: productivityBuckets(prior, projects), FiledBy: []productivityActor{}, CompletedBy: []productivityActor{}}
	projectIndex := map[string]int{}
	for _, project := range projects {
		projectIndex[project] = len(out.Projects)
		out.Projects = append(out.Projects, productivityProject{ProjectID: project})
	}
	clause, args, err := productivityFilter(projects, filter)
	if err != nil {
		return out, err
	}
	if err := queryRowSQL(ctx, s.database.reader, `SELECT EXISTS(SELECT 1 FROM projects WHERE organization_id=? AND id IN (SELECT value FROM sql_rows(?)) AND julianday(created_at)<julianday(?))`, out.OrganizationID, args[0], formatHubTime(prior.To)).Scan(&out.PriorRecorded); err != nil {
		return out, fmt.Errorf("read organization prior availability: %w", err)
	}
	args = append([]any{out.OrganizationID}, args...)
	if len(filter.Runners) > 0 {
		raw, err := sqlList(filter.Runners)
		if err != nil {
			return out, err
		}
		clause += ` AND EXISTS(SELECT 1 FROM report_issue_runners r WHERE r.organization_id=s.organization_id AND r.project_id=s.project_id AND r.work_item_id=s.work_item_id AND r.started_ns<? AND r.updated_ns>=? AND r.runner_id IN (SELECT value FROM sql_rows(?)))`
		args = append(args, w.To.UnixNano(), prior.From.UnixNano(), raw)
	}
	var statement strings.Builder
	statement.WriteString(`WITH selected AS (SELECT s.* FROM report_issue_summaries s WHERE `)
	statement.WriteString(clause)
	statement.WriteString(`), activity AS (
 SELECT h.organization_id,h.project_id,h.work_item_id,
 sum(CASE WHEN h.hour<? THEN h.cost_usd ELSE 0 END) AS current_cost,
 sum(CASE WHEN h.hour<? THEN h.cost_usd ELSE 0 END) AS previous_cost,
 sum(CASE WHEN h.hour>=? AND h.hour<? THEN h.landed ELSE 0 END) AS landed,
 sum(CASE WHEN h.hour>=? AND h.hour<? THEN h.landed ELSE 0 END) AS previous_landed,
 max(CASE WHEN h.hour>=? AND h.hour<? THEN h.worked ELSE 0 END) AS worked,
 max(CASE WHEN h.hour>=? AND h.hour<? THEN h.worked ELSE 0 END) AS previous_worked,
 max(CASE WHEN h.hour>=? AND h.hour<? THEN h.reworked ELSE 0 END) AS reworked,
 max(CASE WHEN h.hour>=? AND h.hour<? THEN h.reworked ELSE 0 END) AS previous_reworked
 FROM selected s JOIN report_issue_hours h ON h.organization_id=s.organization_id AND h.project_id=s.project_id AND h.work_item_id=s.work_item_id WHERE h.hour<? GROUP BY h.organization_id,h.project_id,h.work_item_id
 ) SELECT s.project_id,s.created_ns,s.closed_ns,s.state,s.filer_kind,s.completed_kind,s.completed_id,s.completed_name,s.cycle_seconds,
 coalesce(a.current_cost,0),coalesce(a.previous_cost,0),coalesce(a.landed,0),coalesce(a.previous_landed,0),coalesce(a.worked,0),coalesce(a.previous_worked,0),coalesce(a.reworked,0),coalesce(a.previous_reworked,0)
 FROM selected s LEFT JOIN activity a ON a.organization_id=s.organization_id AND a.project_id=s.project_id AND a.work_item_id=s.work_item_id
 WHERE (s.created_ns>=? AND s.created_ns<?) OR (s.closed_ns>=? AND s.closed_ns<?) OR coalesce(a.worked,0)>0 OR coalesce(a.previous_worked,0)>0 OR coalesce(a.landed,0)>0 OR coalesce(a.previous_landed,0)>0`)
	args = append(args, w.To.Unix(), w.From.Unix(), w.From.Unix(), w.To.Unix(), prior.From.Unix(), w.From.Unix(), w.From.Unix(), w.To.Unix(), prior.From.Unix(), w.From.Unix(), w.From.Unix(), w.To.Unix(), prior.From.Unix(), w.From.Unix(), w.To.Unix(), prior.From.UnixNano(), w.To.UnixNano(), prior.From.UnixNano(), w.To.UnixNano())
	rows, err := querySQL(ctx, s.database.reader, statement.String(), args...)
	if err != nil {
		return out, fmt.Errorf("read organization productivity: %w", err)
	}
	defer rows.Close()
	filedBy, completedBy := map[string]*productivityActor{}, map[string]*productivityActor{}
	for rows.Next() {
		var project, state, filer, completedKind, completedID, completedName string
		var created, closed int64
		var cycle sql.NullFloat64
		var cost, previousCost float64
		var landed, previousLanded, worked, previousWorked, reworked, previousReworked int
		if err := rows.Scan(&project, &created, &closed, &state, &filer, &completedKind, &completedID, &completedName, &cycle, &cost, &previousCost, &landed, &previousLanded, &worked, &previousWorked, &reworked, &previousReworked); err != nil {
			return out, err
		}
		row := &out.Projects[projectIndex[project]]
		for _, period := range []struct {
			w                        operatortool.AnalyticsWindow
			metrics                  *productivityMetrics
			series                   []productivityBucket
			cost                     float64
			landed, worked, reworked int
			previous                 bool
		}{
			{w, &row.Current, out.Series, cost, landed, worked, reworked, false},
			{prior, &row.Previous, out.PreviousSeries, previousCost, previousLanded, previousWorked, previousReworked, true},
		} {
			m := period.metrics
			m.ChangesLanded += period.landed
			m.Worked += period.worked
			m.Reworked += period.reworked
			if created >= period.w.From.UnixNano() && created < period.w.To.UnixNano() {
				m.Filed++
				productivityActorCount(filedBy, filer, "", "", period.previous)
				productivityBucketCount(period.series, period.w, time.Unix(0, created), project, false)
			}
			if closed >= period.w.From.UnixNano() && closed < period.w.To.UnixNano() {
				switch state {
				case nativeShippedState:
					m.Completed++
					m.CompletionCost += period.cost
					if cycle.Valid {
						m.cycles = append(m.cycles, cycle.Float64)
					}
					productivityActorCount(completedBy, completedKind, completedID, completedName, period.previous)
					productivityBucketCount(period.series, period.w, time.Unix(0, closed), project, true)
				case nativeCancelledState:
					m.Cancelled++
				}
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	for i := range out.Projects {
		row := &out.Projects[i]
		finishProductivityMetrics(&row.Current)
		finishProductivityMetrics(&row.Previous)
		addProductivityMetrics(&out.Current, row.Current)
		addProductivityMetrics(&out.Previous, row.Previous)
	}
	finishProductivityMetrics(&out.Current)
	finishProductivityMetrics(&out.Previous)
	out.FiledBy = productivityActors(filedBy)
	out.CompletedBy = productivityActors(completedBy)
	return out, nil
}

func productivityBuckets(w operatortool.AnalyticsWindow, projects []string) []productivityBucket {
	out := []productivityBucket{}
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		b := productivityBucket{From: from, To: minTime(from.Add(w.Bucket), w.To), Projects: map[string]productivityCount{}}
		for _, project := range projects {
			b.Projects[project] = productivityCount{}
		}
		out = append(out, b)
	}
	return out
}

func productivityBucketCount(series []productivityBucket, w operatortool.AnalyticsWindow, at time.Time, project string, completed bool) {
	i := int(at.Sub(w.From) / w.Bucket)
	counts := series[i].Projects[project]
	if completed {
		series[i].Completed++
		counts.Completed++
	} else {
		series[i].Filed++
		counts.Filed++
	}
	series[i].Projects[project] = counts
}

func productivityActorCount(actors map[string]*productivityActor, kind, id, name string, previous bool) {
	key := kind + "/" + id
	if actors[key] == nil {
		actors[key] = &productivityActor{Kind: kind, ID: id, Name: name}
	}
	if previous {
		actors[key].Previous++
	} else {
		actors[key].Current++
	}
}

func productivityActors(actors map[string]*productivityActor) []productivityActor {
	out := make([]productivityActor, 0, len(actors))
	for _, actor := range actors {
		out = append(out, *actor)
	}
	slices.SortFunc(out, func(a, b productivityActor) int { return strings.Compare(a.Kind+"/"+a.ID, b.Kind+"/"+b.ID) })
	return out
}

func finishProductivityMetrics(m *productivityMetrics) {
	m.NetBacklog = m.Filed - m.Completed - m.Cancelled
	if m.Completed > 0 {
		m.CostPerCompleted = new(m.CompletionCost / float64(m.Completed))
	}
	if m.Worked > 0 {
		m.ReworkRate = new(100 * float64(m.Reworked) / float64(m.Worked))
	}
	if len(m.cycles) > 0 {
		m.CycleP50 = new(hubanalytics.SampledDuration(m.cycles).P50Seconds)
	}
}

func addProductivityMetrics(to *productivityMetrics, from productivityMetrics) {
	to.Filed += from.Filed
	to.Completed += from.Completed
	to.Cancelled += from.Cancelled
	to.ChangesLanded += from.ChangesLanded
	to.Worked += from.Worked
	to.Reworked += from.Reworked
	to.CompletionCost += from.CompletionCost
	to.cycles = append(to.cycles, from.cycles...)
}

func filterProductivityProjects(readable, selected []string) []string {
	if len(selected) == 0 {
		return readable
	}
	return slices.DeleteFunc(slices.Clone(readable), func(id string) bool { return !slices.Contains(selected, id) })
}
