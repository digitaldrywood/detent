package reports

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const (
	ReportsAllProjects     = "all"
	ReportsMaxFilterValues = 32
	reportsMaxCustomSpan   = 90 * 24 * time.Hour
	ReportsFacetLimit      = 200
	ReportsUnavailableName = "Unavailable"
)

type ReportsFilter struct {
	Labels     []string
	Priorities []string
	Types      []string
	FiledBy    []string
	Runners    []string
	Models     []string
	Stages     []string
}

func (f ReportsFilter) StoreFilter() reportstore.Filter {
	priorities := make([]int, len(f.Priorities))
	for i, value := range f.Priorities {
		v, err := strconv.Atoi(value)
		if err == nil {
			priorities[i] = v
		}
	}
	return reportstore.Filter{Labels: f.Labels, Priorities: priorities, Types: f.Types, FiledBy: f.FiledBy, Runners: f.Runners, Models: f.Models, Stages: f.Stages}
}

func ReportsQueryValues(params url.Values, name string) []string {
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

func ParseReportsFilter(params url.Values) (ReportsFilter, error) {
	f := ReportsFilter{
		Labels:     ReportsQueryValues(params, "labels"),
		Priorities: ReportsQueryValues(params, "priorities"),
		Types:      ReportsQueryValues(params, "types"),
		FiledBy:    ReportsQueryValues(params, "filed_by"),
		Runners:    ReportsQueryValues(params, "runners"),
		Models:     ReportsQueryValues(params, "models"),
		Stages:     ReportsQueryValues(params, "stages"),
	}
	for _, values := range [][]string{f.Labels, f.Priorities, f.Types, f.FiledBy, f.Runners, f.Models, f.Stages} {
		if len(values) > ReportsMaxFilterValues || len(strings.Join(values, "\x00")) > 4096 {
			return f, hubapi.PublicInvalid("Report filters accept at most 32 values each")
		}
	}
	for _, value := range f.Priorities {
		if priority, err := strconv.Atoi(value); err != nil || priority < 0 || priority > 3 || strconv.Itoa(priority) != value {
			return f, hubapi.PublicInvalid("Priority must be 0, 1, 2 or 3")
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

func ReportsWindow(params url.Values, now time.Time) (operatortool.AnalyticsWindow, error) {
	to := now.UTC().Truncate(time.Hour)
	var from time.Time
	fromValue, toValue := params.Get("from"), params.Get("to")
	if fromValue == "" && toValue == "" {
		hours := reportsRangeHours[params.Get("range")]
		if hours == 0 {
			return operatortool.AnalyticsWindow{}, hubapi.PublicInvalid("Unsupported report range")
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
		from, to = start, hubapi.MinTime(end, to)
		if !from.Before(to) && end.After(to) {
			to = now.UTC()
		}
		if !from.Before(to) || to.Sub(from) > reportsMaxCustomSpan {
			return operatortool.AnalyticsWindow{}, hubapi.PublicInvalid("A custom range runs forward from a past day and spans at most 90 days")
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

func reportsDay(value string, end bool) (time.Time, error) {
	day, err := time.Parse(time.DateOnly, value)
	if err != nil {
		return time.Time{}, hubapi.PublicInvalid("A custom range needs from and to as YYYY-MM-DD days in UTC")
	}
	if end {
		day = day.AddDate(0, 0, 1)
	}
	return day.UTC(), nil
}

type ReportsScopeKey struct{}

type ReportsScope struct {
	ReporterPrincipal string        `json:"-"`
	Issues            string        `json:"-"`
	IssueArgs         []any         `json:"-"`
	Attempts          ReportsFilter `json:"-"`
}

func ReportsScopeFrom(ctx context.Context) (ReportsScope, bool) {
	scope, ok := ctx.Value(ReportsScopeKey{}).(ReportsScope)
	return scope, ok
}

func ReportsIssueClause(ctx context.Context, column string) (string, []any) {
	scope, ok := ReportsScopeFrom(ctx)
	if !ok || scope.Issues == "" {
		return "", nil
	}
	return " AND " + column + " IN (" + scope.Issues + ")", slices.Clone(scope.IssueArgs)
}

type ReportsFacet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ReportsFacets struct {
	Labels  []string       `json:"labels"`
	FiledBy []ReportsFacet `json:"filed_by"`
	Runners []ReportsFacet `json:"runners"`
	Models  []string       `json:"models"`
	Stages  []string       `json:"stages"`
}
