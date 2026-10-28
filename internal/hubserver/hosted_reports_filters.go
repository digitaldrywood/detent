package hubserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	reportsAllProjects     = "all"
	reportsMaxFilterValues = 32
	reportsMaxCustomSpan   = 90 * 24 * time.Hour
	reportsFacetLimit      = 200
	reportsUnavailableName = "Unavailable"
)

// reportsFilter narrows a report. Labels, priorities, types and filers select
// issues; runners select the attempts that ran on them and the issues those
// attempts worked; models and stages select attempts only.
type reportsFilter struct {
	Labels     []string
	Priorities []string
	Types      []string
	FiledBy    []string
	Runners    []string
	Models     []string
	Stages     []string
}

func (f reportsFilter) issues() bool {
	return len(f.Labels)+len(f.Priorities)+len(f.Types)+len(f.FiledBy)+len(f.Runners) > 0
}

func (f reportsFilter) attempts() bool {
	return len(f.Runners)+len(f.Models)+len(f.Stages) > 0
}

func reportsQueryValues(params url.Values, name string) []string {
	values := slices.Concat(params[name], params[name+"[]"])
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func parseReportsFilter(params url.Values) (reportsFilter, error) {
	f := reportsFilter{
		Labels:     reportsQueryValues(params, "labels"),
		Priorities: reportsQueryValues(params, "priorities"),
		Types:      reportsQueryValues(params, "types"),
		FiledBy:    reportsQueryValues(params, "filed_by"),
		Runners:    reportsQueryValues(params, "runners"),
		Models:     reportsQueryValues(params, "models"),
		Stages:     reportsQueryValues(params, "stages"),
	}
	for _, values := range [][]string{f.Labels, f.Priorities, f.Types, f.FiledBy, f.Runners, f.Models, f.Stages} {
		if len(values) > reportsMaxFilterValues || len(strings.Join(values, "\x00")) > 4096 {
			return f, nativePublicInvalid("Report filters accept at most 32 values each")
		}
	}
	for _, value := range f.Priorities {
		if priority, err := strconv.Atoi(value); err != nil || priority < 0 || priority > 3 || strconv.Itoa(priority) != value {
			return f, nativePublicInvalid("Priority must be 0, 1, 2 or 3")
		}
	}
	for _, value := range f.Types {
		if err := validateNativeIssueType(value, true); err != nil {
			return f, err
		}
	}
	return f, nil
}

var reportsRangeHours = map[string]int{"24h": 24, "48h": 48, "7d": 168, "30d": 720, "90d": 2160}

// reportsWindow resolves a preset range, or a custom from/to of whole UTC
// days, to a window ending at the last complete hour; a custom range of only
// today, read in its first hour, ends now.
func reportsWindow(params url.Values, now time.Time) (operatortool.AnalyticsWindow, error) {
	to := now.UTC().Truncate(time.Hour)
	var from time.Time
	fromValue, toValue := params.Get("from"), params.Get("to")
	if fromValue == "" && toValue == "" {
		hours := reportsRangeHours[params.Get("range")]
		if hours == 0 {
			return operatortool.AnalyticsWindow{}, nativePublicInvalid("Unsupported report range")
		}
		from = reportsWindowStart(to, hours)
	} else {
		start, err := reportsDay(fromValue, false)
		if err != nil {
			return operatortool.AnalyticsWindow{}, err
		}
		end, err := reportsDay(toValue, true)
		if err != nil {
			return operatortool.AnalyticsWindow{}, err
		}
		from, to = start, minTime(end, to)
		if !from.Before(to) && end.After(to) {
			to = now.UTC()
		}
		if !from.Before(to) || to.Sub(from) > reportsMaxCustomSpan {
			return operatortool.AnalyticsWindow{}, nativePublicInvalid("A custom range runs forward from a past day and spans at most 90 days")
		}
	}
	span := to.Sub(from)
	bucket := time.Hour
	if span > 720*time.Hour {
		bucket = 24 * time.Hour
	} else if span > 240*time.Hour {
		bucket = 4 * time.Hour
	}
	return operatortool.AnalyticsWindow{From: from, To: to, Bucket: bucket}, nil
}

// reportsDay reads a UTC calendar day; the end of a range is the midnight
// after its last day.
func reportsDay(value string, end bool) (time.Time, error) {
	day, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return time.Time{}, nativePublicInvalid("A custom range needs from and to as YYYY-MM-DD days in UTC")
	}
	if end {
		day = day.AddDate(0, 0, 1)
	}
	return day.UTC(), nil
}

type reportsScopeKey struct{}

// reportsScope is a report's filter, resolved for one project, that the
// window reads add to their statements. A read without a scope keeps its
// statement unchanged, so an unfiltered report reads exactly as before.
type reportsScope struct {
	reporterPrincipal string
	issues            string
	issueArgs         []any
	attempts          reportsFilter
}

// withReportsScope narrows the window reads under ctx to the filter. The
// issue dimensions become one subquery over the project's issues that were
// open, closed or worked during [from, to), joined into each read, so no
// issue list leaves the database.
func withReportsScope(ctx context.Context, scope nativeScope, f reportsFilter, from, to time.Time) (context.Context, error) {
	out := reportsScope{attempts: reportsFilter{Runners: f.Runners, Models: f.Models, Stages: f.Stages}}
	if parent, ok := reportsScopeFrom(ctx); ok && parent.reporterPrincipal != "" {
		out.issues, out.issueArgs, out.reporterPrincipal = parent.issues, parent.issueArgs, parent.reporterPrincipal
	}
	if f.issues() {
		statement, args, err := reportsIssueSubquery(scope, f, from, to)
		if err != nil {
			return ctx, err
		}
		if out.issues != "" {
			statement += " AND ri.native_id IN (" + out.issues + ")"
			args = append(args, out.issueArgs...)
		}
		out.issues, out.issueArgs = statement, args
	}
	if out.issues == "" && !f.attempts() {
		return ctx, nil
	}
	return context.WithValue(ctx, reportsScopeKey{}, out), nil
}

func reportsScopeFrom(ctx context.Context) (reportsScope, bool) {
	scope, ok := ctx.Value(reportsScopeKey{}).(reportsScope)
	return scope, ok
}

// reportsIssueClause narrows a read to the report's issue set by the column
// that holds each row's work item.
func reportsIssueClause(ctx context.Context, column string) (string, []any) {
	scope, ok := reportsScopeFrom(ctx)
	if !ok || scope.issues == "" {
		return "", nil
	}
	return " AND " + column + " IN (" + scope.issues + ")", slices.Clone(scope.issueArgs)
}

// reportsAttemptClause narrows a read of native_attempts, under alias, to the
// report's runners, models and stages.
func reportsAttemptClause(ctx context.Context, alias string) (string, []any, error) {
	scope, ok := reportsScopeFrom(ctx)
	if !ok {
		return "", nil, nil
	}
	var clause strings.Builder
	var args []any
	for _, filter := range []struct {
		values     []string
		expression string
	}{
		{scope.attempts.Runners, reportRunnerColumn(alias)},
		{scope.attempts.Models, reportModelColumn(alias)},
		{scope.attempts.Stages, reportRoleColumn(alias)},
	} {
		if len(filter.values) == 0 {
			continue
		}
		encoded, err := marshalNative(filter.values)
		if err != nil {
			return "", nil, fmt.Errorf("encode report attempt filter: %w", err)
		}
		clause.WriteString(" AND " + filter.expression + " IN (SELECT value FROM json_each(?))")
		args = append(args, encoded)
	}
	return clause.String(), args, nil
}

// reportsScopedClause is the issue and attempt narrowing of a read whose
// rows each belong to one attempt.
func reportsScopedClause(ctx context.Context, alias string) (string, []any, error) {
	issue, issueArgs := reportsIssueClause(ctx, alias+".work_item_id")
	attempt, attemptArgs, err := reportsAttemptClause(ctx, alias)
	return issue + attempt, append(issueArgs, attemptArgs...), err
}

// reportsIssueSubquery selects the project's issues that match the issue
// dimensions of a filter and were open, closed or worked during [from, to).
// Runners select the issues with an attempt on one of them in that window.
// It names main.issues because the rollup reader rewrites a bare issues
// table to its narrow projection.
func reportsIssueSubquery(scope nativeScope, f reportsFilter, from, to time.Time) (string, []any, error) {
	var statement strings.Builder
	statement.WriteString(`SELECT ri.native_id FROM main.issues ri LEFT JOIN workflow_states rws ON rws.id=ri.workflow_state_id
 WHERE ri.organization_id=? AND ri.project_id=? AND ri.native_id IS NOT NULL AND julianday(ri.created_at)<julianday(?)
 AND (coalesce(rws.terminal,0)=0 OR julianday(ri.updated_at)>=julianday(?) OR EXISTS (SELECT 1 FROM report_attempts rwa WHERE rwa.organization_id=ri.organization_id AND rwa.project_id=ri.project_id AND rwa.work_item_id=ri.native_id AND julianday(rwa.updated_at)>=julianday(?)))`)
	args := []any{scope.organization, scope.project, formatHubTime(to), formatHubTime(from), formatHubTime(from)}
	for _, filter := range []struct {
		values []string
		clause string
		window bool
	}{
		{f.Labels, "EXISTS (SELECT 1 FROM native_issue_page_filter_values rl WHERE rl.issue_id=ri.id AND rl.kind='label' AND rl.value IN (SELECT value FROM json_each(?)))", false},
		{f.Types, "ri.issue_type IN (SELECT value FROM json_each(?))", false},
		{f.FiledBy, nativeIssuePrincipal("ri") + " IN (SELECT value FROM json_each(?))", false},
		{f.Priorities, "EXISTS (SELECT 1 FROM queue_entries rq WHERE rq.issue_id=ri.id AND CAST(rq.priority_override AS TEXT) IN (SELECT value FROM json_each(?)))", false},
		{f.Runners, `EXISTS (SELECT 1 FROM report_attempts ra WHERE ra.organization_id=ri.organization_id AND ra.project_id=ri.project_id AND ra.work_item_id=ri.native_id
 AND julianday(ra.started_at)<julianday(?) AND julianday(ra.updated_at)>=julianday(?)
 AND coalesce((SELECT rlr.runner_id FROM lease_runners rlr WHERE rlr.lease_id=ra.lease_id),ra.runner_id,'') IN (SELECT value FROM json_each(?)))`, true},
	} {
		if len(filter.values) == 0 {
			continue
		}
		encoded, err := marshalNative(filter.values)
		if err != nil {
			return "", nil, fmt.Errorf("encode report issue filter: %w", err)
		}
		statement.WriteString(" AND " + filter.clause)
		if filter.window {
			args = append(args, formatHubTime(to), formatHubTime(from))
		}
		args = append(args, encoded)
	}
	return statement.String(), args, nil
}

type reportsFacet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// reportsFacets are the values the filter chips offer, from the projects the
// report reads: their issues' labels and filers, and the runners, models and
// stages of their attempts in the window.
type reportsFacets struct {
	Labels  []string       `json:"labels"`
	FiledBy []reportsFacet `json:"filed_by"`
	Runners []reportsFacet `json:"runners"`
	Models  []string       `json:"models"`
	Stages  []string       `json:"stages"`
}

func (s *Service) readReportsFacets(ctx context.Context, projects []string, w operatortool.AnalyticsWindow) (reportsFacets, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	out := reportsFacets{Labels: []string{}, FiledBy: []reportsFacet{}, Runners: []reportsFacet{}, Models: []string{}, Stages: []string{}}
	encoded, err := marshalNative(projects)
	if err != nil {
		return out, fmt.Errorf("encode report projects: %w", err)
	}
	organization := s.config.Hosted.OrganizationID
	q, err := prepareReportsQuery(ctx, s.database.reader, nativeScope{organization: tracker.OrganizationID(organization)}, "report_attempts", "native_issue_actors", "native_issue_page_filter_values")
	if err != nil {
		return out, err
	}
	read := func(statement string, scan func(values ...string), args ...any) error {
		if ownership, ok := reportsScopeFrom(ctx); ok && ownership.reporterPrincipal != "" {
			column := "a.work_item_id"
			if strings.Contains(statement, "FROM issues i") {
				column = "i.native_id"
			}
			visibility, visibilityArgs := reportsIssueClause(ctx, column)
			split := strings.Index(statement, " ORDER BY")
			if split < 0 {
				split = strings.Index(statement, " LIMIT")
			}
			if column == "i.native_id" && strings.Contains(statement, ") f ORDER BY") {
				split = strings.Index(statement, ") f ORDER BY")
			}
			statement = statement[:split] + visibility + statement[split:]
			scopedArgs := append([]any{}, args[:len(args)-1]...)
			scopedArgs = append(scopedArgs, visibilityArgs...)
			args = append(scopedArgs, args[len(args)-1])
		}
		rows, err := q.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			scan(id, name)
		}
		return errors.Join(rows.Err(), rows.Close())
	}
	if err := read(`SELECT DISTINCT l.value,'' FROM issues i JOIN native_issue_page_filter_values l ON l.issue_id=i.id AND l.kind='label' WHERE i.organization_id=? AND i.project_id IN (SELECT value FROM json_each(?)) ORDER BY l.value LIMIT ?`,
		func(values ...string) { out.Labels = append(out.Labels, values[0]) }, organization, encoded, reportsFacetLimit); err != nil {
		return out, fmt.Errorf("read report label facets: %w", err)
	}
	if err := read(`SELECT f.principal,`+nativeActorDisplayName("f.principal", "?")+` FROM (SELECT DISTINCT `+nativeIssuePrincipal("i")+` AS principal FROM issues i WHERE i.organization_id=? AND i.project_id IN (SELECT value FROM json_each(?)) AND coalesce(`+nativeIssuePrincipal("i")+`,'')<>'') f ORDER BY 2,1 LIMIT ?`,
		func(values ...string) { out.FiledBy = append(out.FiledBy, reportsFacet{values[0], values[1]}) }, organization, organization, encoded, reportsFacetLimit); err != nil {
		return out, fmt.Errorf("read report filer facets: %w", err)
	}
	models, stages, runners := map[string]bool{}, map[string]bool{}, map[string]reportsFacet{}
	for _, project := range projects {
		if err := read(`SELECT DISTINCT `+reportModelColumn("a")+`,`+reportRoleColumn("a")+`
FROM report_attempts a WHERE a.organization_id=? AND a.project_id=? AND julianday(a.updated_at)>=julianday(?) AND julianday(a.started_at)<julianday(?) LIMIT ?`,
			func(values ...string) { models[values[0]], stages[values[1]] = true, true }, organization, project, formatHubTime(w.From), formatHubTime(w.To), reportsFacetLimit); err != nil {
			return out, fmt.Errorf("read report attempt facets: %w", err)
		}
		if err := read(`SELECT DISTINCT r.id,coalesce(r.display_name,'') FROM report_attempts a JOIN lease_runners lr ON lr.lease_id=a.lease_id JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=a.organization_id
WHERE a.organization_id=? AND a.project_id=? AND julianday(a.updated_at)>=julianday(?) AND julianday(a.started_at)<julianday(?) LIMIT ?`,
			func(values ...string) { runners[values[0]] = reportsFacet{values[0], values[1]} }, organization, project, formatHubTime(w.From), formatHubTime(w.To), reportsFacetLimit); err != nil {
			return out, fmt.Errorf("read report runner facets: %w", err)
		}
	}
	for _, runner := range runners {
		out.Runners = append(out.Runners, runner)
	}
	slices.SortFunc(out.Runners, func(a, b reportsFacet) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	for model := range models {
		out.Models = append(out.Models, model)
	}
	for stage := range stages {
		out.Stages = append(out.Stages, stage)
	}
	slices.Sort(out.Models)
	slices.Sort(out.Stages)
	return out, nil
}
