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

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		encoded, err := sqlList(filter.values)
		if err != nil {
			return "", nil, fmt.Errorf("encode report attempt filter: %w", err)
		}
		clause.WriteString(" AND " + filter.expression + " IN (SELECT value FROM sql_rows(?))")
		args = append(args, encoded)
	}
	return bindSQLParameters(clause.String(), args)
}

// reportsScopedClause is the issue and attempt narrowing of a read whose
// rows each belong to one attempt.
func reportsScopedClause(ctx context.Context, alias string) (string, []any, error) {
	issue, issueArgs := reportsIssueClause(ctx, alias+".work_item_id")
	attempt, attemptArgs, err := reportsAttemptClause(ctx, alias)
	return issue + attempt, append(issueArgs, attemptArgs...), err
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
	if s.config.TenantRepository == nil {
		return out, errors.New("PostgreSQL report reader is unavailable")
	}
	ownership, _ := reportsScopeFrom(ctx)
	var facets reportstore.Facets
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return errors.New("PostgreSQL report reader is unavailable")
		}
		var err error
		facets, err = store.ReadFacets(ctx, projectIDs(projects), w.From, w.To, ownership.reporterPrincipal, reportsFacetLimit)
		return err
	})
	if err != nil {
		return out, fmt.Errorf("read PostgreSQL report facets: %w", err)
	}
	return reportsFacets{Labels: facets.Labels, FiledBy: convertReportFacets(facets.FiledBy), Runners: convertReportFacets(facets.Runners), Models: facets.Models, Stages: facets.Stages}, nil
}
