package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
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
			return f, nativeInvalid("Report filters accept at most 32 values each")
		}
	}
	for _, value := range f.Priorities {
		if priority, err := strconv.Atoi(value); err != nil || priority < 0 || priority > 3 || strconv.Itoa(priority) != value {
			return f, nativeInvalid("Priority must be 0, 1, 2 or 3")
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
// days, to a window ending at the last complete hour.
func reportsWindow(params url.Values, now time.Time) (operatortool.AnalyticsWindow, error) {
	to := now.UTC().Truncate(time.Hour)
	var from time.Time
	fromValue, toValue := params.Get("from"), params.Get("to")
	if fromValue == "" && toValue == "" {
		hours := reportsRangeHours[params.Get("range")]
		if hours == 0 {
			return operatortool.AnalyticsWindow{}, nativeInvalid("Unsupported report range")
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
		if !from.Before(to) || to.Sub(from) > reportsMaxCustomSpan {
			return operatortool.AnalyticsWindow{}, nativeInvalid("A custom range runs forward from a past day and spans at most 90 days")
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
		return time.Time{}, nativeInvalid("A custom range needs from and to as YYYY-MM-DD days in UTC")
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
	issues   string
	attempts reportsFilter
}

func withReportsScope(ctx context.Context, issues []string, f reportsFilter) (context.Context, error) {
	scope := reportsScope{attempts: reportsFilter{Runners: f.Runners, Models: f.Models, Stages: f.Stages}}
	if f.issues() {
		if issues == nil {
			issues = []string{}
		}
		encoded, err := marshalNative(issues)
		if err != nil {
			return ctx, fmt.Errorf("encode report issues: %w", err)
		}
		scope.issues = encoded
	}
	if scope.issues == "" && !f.attempts() {
		return ctx, nil
	}
	return context.WithValue(ctx, reportsScopeKey{}, scope), nil
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
	return " AND " + column + " IN (SELECT value FROM json_each(?))", []any{scope.issues}
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
		{scope.attempts.Runners, "coalesce((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=" + alias + ".lease_id),json_extract(" + alias + ".data_json,'$.runner_id'),'')"},
		{scope.attempts.Models, "coalesce(nullif(trim(json_extract(" + alias + ".data_json,'$.runtime.identity.resolved_model.value')),''),nullif(trim(json_extract(" + alias + ".data_json,'$.runtime.identity.requested_model.value')),''),'" + reportsUnavailableName + "')"},
		{scope.attempts.Stages, "coalesce(nullif(trim(json_extract(" + alias + ".data_json,'$.runtime.identity.role')),''),'" + reportsUnavailableName + "')"},
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

// readReportsIssueSet resolves the issue dimensions of a filter to the
// project's matching issues. Runners select the issues with an attempt on
// one of them during [from, to).
func readReportsIssueSet(ctx context.Context, q nativeQueryer, scope nativeScope, f reportsFilter, from, to time.Time) ([]string, error) {
	var statement strings.Builder
	statement.WriteString(`SELECT i.native_id FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id IS NOT NULL`)
	args := []any{scope.organization, scope.project}
	for _, filter := range []struct {
		values []string
		clause string
	}{
		{f.Labels, "EXISTS (SELECT 1 FROM json_each(i.labels_json) l WHERE l.value IN (SELECT value FROM json_each(?)))"},
		{f.Types, "i.issue_type IN (SELECT value FROM json_each(?))"},
		{f.FiledBy, "json_extract(i.actor_json,'$.principal_id') IN (SELECT value FROM json_each(?))"},
		{f.Priorities, "EXISTS (SELECT 1 FROM queue_entries q WHERE q.issue_id=i.id AND CAST(q.priority_override AS TEXT) IN (SELECT value FROM json_each(?)))"},
	} {
		if len(filter.values) == 0 {
			continue
		}
		encoded, err := marshalNative(filter.values)
		if err != nil {
			return nil, fmt.Errorf("encode report issue filter: %w", err)
		}
		statement.WriteString(" AND " + filter.clause)
		args = append(args, encoded)
	}
	if len(f.Runners) > 0 {
		encoded, err := marshalNative(f.Runners)
		if err != nil {
			return nil, fmt.Errorf("encode report runner filter: %w", err)
		}
		statement.WriteString(` AND EXISTS (SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id
 AND julianday(a.started_at)<julianday(?) AND julianday(a.updated_at)>=julianday(?)
 AND coalesce((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=a.lease_id),json_extract(a.data_json,'$.runner_id'),'') IN (SELECT value FROM json_each(?)))`)
		args = append(args, formatHubTime(to), formatHubTime(from), encoded)
	}
	rows, err := q.QueryContext(ctx, statement.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read report issues: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read report issues: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read report issues: %w", err)
	}
	return ids, nil
}

type reportsFacet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// reportsFacets are the values the filter chips offer: labels and filers of
// the readable projects' issues, the organization's runners, and the models
// and stages of the window's attempts.
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
	q := s.database.reader
	read := func(statement string, scan func(values ...string), args ...any) error {
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
	if err := read(`SELECT DISTINCT l.value,'' FROM issues i, json_each(i.labels_json) l WHERE i.organization_id=? AND i.project_id IN (SELECT value FROM json_each(?)) AND l.type='text' ORDER BY l.value LIMIT ?`,
		func(values ...string) { out.Labels = append(out.Labels, values[0]) }, organization, encoded, reportsFacetLimit); err != nil {
		return out, fmt.Errorf("read report label facets: %w", err)
	}
	if err := read(`SELECT f.principal,`+nativeActorDisplayName("f.principal", "?")+` FROM (SELECT DISTINCT json_extract(i.actor_json,'$.principal_id') AS principal FROM issues i WHERE i.organization_id=? AND i.project_id IN (SELECT value FROM json_each(?)) AND coalesce(json_extract(i.actor_json,'$.principal_id'),'')<>'') f ORDER BY 2,1 LIMIT ?`,
		func(values ...string) { out.FiledBy = append(out.FiledBy, reportsFacet{values[0], values[1]}) }, organization, organization, encoded, reportsFacetLimit); err != nil {
		return out, fmt.Errorf("read report filer facets: %w", err)
	}
	if err := read(`SELECT r.id,coalesce(r.display_name,'') FROM runner_identities r WHERE r.organization_id=? ORDER BY 2,1 LIMIT ?`,
		func(values ...string) { out.Runners = append(out.Runners, reportsFacet{values[0], values[1]}) }, organization, reportsFacetLimit); err != nil {
		return out, fmt.Errorf("read report runner facets: %w", err)
	}
	models, stages := map[string]bool{}, map[string]bool{}
	for _, project := range projects {
		if err := read(`SELECT DISTINCT coalesce(nullif(trim(json_extract(a.data_json,'$.runtime.identity.resolved_model.value')),''),nullif(trim(json_extract(a.data_json,'$.runtime.identity.requested_model.value')),''),'`+reportsUnavailableName+`'),coalesce(nullif(trim(json_extract(a.data_json,'$.runtime.identity.role')),''),'`+reportsUnavailableName+`')
FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND julianday(a.updated_at)>=julianday(?) AND julianday(a.started_at)<julianday(?) LIMIT ?`,
			func(values ...string) { models[values[0]], stages[values[1]] = true, true }, organization, project, formatHubTime(w.From), formatHubTime(w.To), reportsFacetLimit); err != nil {
			return out, fmt.Errorf("read report attempt facets: %w", err)
		}
	}
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
