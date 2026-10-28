package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type diagnosticsCoverage struct {
	Source   string `json:"source"`
	Observed *int   `json:"observed"`
	Total    *int   `json:"total"`
}

type diagnosticsCapacity struct {
	Hour  time.Time `json:"hour"`
	Slots float64   `json:"slots"`
	Todo  *int      `json:"todo"`
}

type diagnosticsReport struct {
	StorageConversions []diagnosticsStorageConversion `json:"storage_conversions"`
	Requests           *requestMetricsReport          `json:"requests,omitempty"`
	From               time.Time                      `json:"from"`
	To                 time.Time                      `json:"to"`
	Partial            bool                           `json:"partial"`
	Findings           any                            `json:"findings"`
	DetectorTick       *time.Time                     `json:"detector_tick"`
	Capacity           []diagnosticsCapacity          `json:"capacity"`
	Claimed            *int                           `json:"claimed"`
	Ready              *int                           `json:"ready"`
	Skipped            *int                           `json:"skipped"`
	SkipReasons        []nativeAnalyticsSkip          `json:"skip_reasons"`
	MergeEntered       int                            `json:"merge_entered"`
	MergeLanded        int                            `json:"merge_landed"`
	MergeWaitP50       *float64                       `json:"merge_wait_p50"`
	MergeWaitP90       *float64                       `json:"merge_wait_p90"`
	MergeWaitMax       *float64                       `json:"merge_wait_max"`
	Refused            []diagnosticsRefusal           `json:"refused"`
	Coverage           []diagnosticsCoverage          `json:"coverage"`
	Configuration      []diagnosticsConfiguration     `json:"configuration"`
}

type diagnosticsStorageConversion struct {
	ProjectID string                      `json:"project_id"`
	Progress  []storageConversionProgress `json:"progress"`
}

type diagnosticsRefusal struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type diagnosticsConfiguration struct {
	Name   string  `json:"name"`
	Value  *string `json:"value"`
	Effect *string `json:"effect"`
}

func (s *Service) hostedDiagnostics(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	window, err := usageRangeWindow(c.QueryParam("range"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	w := operatortool.AnalyticsWindow{From: window.From, To: window.To, Bucket: time.Hour}
	report, err := s.readCloudDiagnostics(ctx, credential, w)
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Diagnostics are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readCloudDiagnostics(ctx context.Context, credential apiCredential, w operatortool.AnalyticsWindow) (diagnosticsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return diagnosticsReport{}, err
	}
	defer tx.Rollback()
	q := analyticsRollupReader{tx}
	analytics, err := s.readAnalyticsReportFor(ctx, credential, operatortool.AnalyticsRequest{Limit: 100}, w, q, true)
	if err != nil {
		return diagnosticsReport{}, err
	}
	return s.readHostedDiagnosticsFrom(ctx, analytics, q)
}

func (s *Service) readHostedDiagnostics(ctx context.Context, analytics nativeAnalyticsReport) (diagnosticsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return diagnosticsReport{}, err
	}
	defer tx.Rollback()
	return s.readHostedDiagnosticsFrom(ctx, analytics, analyticsRollupReader{tx})
}

func (s *Service) readHostedDiagnosticsFrom(ctx context.Context, analytics nativeAnalyticsReport, reader nativeQueryer) (diagnosticsReport, error) {
	ctx = s.runDetailContext(ctx)
	reader = analyticsSourceReader(reader)
	w := analytics.Window
	out := diagnosticsReport{From: w.From, To: w.To, Partial: analytics.NextOffset != nil, Capacity: []diagnosticsCapacity{}, SkipReasons: []nativeAnalyticsSkip{}}
	for at := w.From; at.Before(w.To); at = at.Add(time.Hour) {
		zero := 0
		out.Capacity = append(out.Capacity, diagnosticsCapacity{Hour: at, Todo: &zero})
	}
	requests, visible, err := s.adminRequestMetrics(ctx)
	if err != nil {
		return out, err
	}
	if visible {
		out.Requests = &requests
	}
	var transitions, attempts, usage, receipts, receiptTotal, queue, claims, ready, skipped, identity int
	refusals := map[string]int{}
	values := map[string]map[string]bool{}
	var planning, validation int
	var waits []float64
	for _, p := range analytics.Projects {
		out.Partial = out.Partial || p.Partial || p.Landings.NextOffset != nil
		attempts += p.AttemptsObserved
		usage += p.CostPerOutcome.PopulationObserved
		receipts += p.Activity.ProfilesObserved
		receiptTotal += p.Activity.ProfilesObserved + p.Activity.ProfilesUnavailable
		queue += p.ProviderCapacityWait.IntervalsObserved
		claims += p.ProviderCapacityWait.ClaimsObserved
		skipped += p.DecisionsObserved
		out.SkipReasons = append(out.SkipReasons, p.SkipReasons...)
		out.MergeLanded += p.CostPerOutcome.Shipped
		identity += len(p.Landings.Items)
		out.Partial = out.Partial || p.FailureSignatures.NextOffset != nil
		for _, failure := range p.FailureSignatures.Items {
			if strings.HasPrefix(failure.Signature, "landing refused: ") {
				refusals[strings.TrimPrefix(failure.Signature, "landing refused: ")] += failure.Count
			}
		}
		scope := nativeScope{organization: tracker.OrganizationID(analytics.OrganizationID), project: tracker.ProjectID(p.ProjectID)}
		progress, err := readStorageConversionProgress(ctx, reader, scope)
		if err != nil {
			return out, err
		}
		out.StorageConversions = append(out.StorageConversions, diagnosticsStorageConversion{ProjectID: p.ProjectID, Progress: progress})
		q, err := analyticsProjectReader(ctx, reader, scope)
		if err != nil {
			return out, err
		}
		q, err = prepareReportsQuery(ctx, q, scope, "report_attempts", "report_events")
		if err != nil {
			return out, err
		}

		countersAvailable, err := analyticsCountersAvailable(ctx, q, scope)
		if err != nil {
			return out, err
		}
		var projectReady int

		if countersAvailable {
			projectReady, err = readAnalyticsCounter(ctx, q, scope, "ready", w.From, w.To)
			if err != nil {
				return out, err
			}
		} else {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT id FROM report_events WHERE organization_id=? AND project_id=? AND type='scheduler.decision' AND scheduler_outcome='ready' AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?) LIMIT ?)`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation+1).Scan(&projectReady); err != nil {
				return out, err
			}

		}

		out.Partial = out.Partial || projectReady > maxAnalyticsPopulation
		ready += min(projectReady, maxAnalyticsPopulation)

		states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, q, scope, w)
		if err != nil {
			return out, err
		}
		out.Partial = out.Partial || clipped
		for _, issue := range issues {
			for _, event := range issue.events {
				if event.kind == "workflow.transitioned" && !event.at.Before(w.From) && event.at.Before(w.To) {
					transitions++
				}
			}
		}
		for i := range out.Capacity {
			at := minTime(out.Capacity[i].Hour.Add(time.Hour), w.To)
			depth := diagnosticsTodoDepth(issues, states, clipped, at)
			if depth == nil {
				out.Capacity[i].Todo = nil
			} else if out.Capacity[i].Todo != nil {
				*out.Capacity[i].Todo += *depth
			}
		}

		for _, phase := range p.Efficiency {
			if phase.Name == "planning" {
				planning += phase.Count
			}
			if phase.Name == "validation" {
				validation += phase.Count
			}
		}
		configuration, err := diagnosticsPolicyValues(ctx, q, scope)
		if err != nil {
			return out, err
		}
		for name, value := range configuration {
			if values[name] == nil {
				values[name] = map[string]bool{}
			}
			values[name][value] = true
		}
		if err := diagnosticsLeaseCapacity(ctx, q, scope, &out); err != nil {
			return out, err
		}
		var entered int

		if countersAvailable {
			entered, err = readAnalyticsCounter(ctx, q, scope, "merge_entered", w.From, w.To)
			if err != nil {
				return out, err
			}
		} else {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM report_events WHERE organization_id=? AND project_id=? AND type='workflow.transitioned' AND analytics_to_state='Merging' AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?)`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To)).Scan(&entered); err != nil {
				return out, err
			}

		}

		out.MergeEntered += entered
		projectWaits, err := diagnosticsMergeWaits(ctx, q, scope, p.Landings.Items)
		if err != nil {
			return out, err
		}
		waits = append(waits, projectWaits...)

	}
	out.Claimed = &claims
	if ready > 0 {
		out.Ready = &ready
	}
	if skipped > 0 {
		out.Skipped = &skipped
	}
	for reason, count := range refusals {
		out.Refused = append(out.Refused, diagnosticsRefusal{Reason: reason, Count: count})
	}
	sort.Slice(out.Refused, func(i, j int) bool { return out.Refused[i].Reason < out.Refused[j].Reason })
	if len(waits) > 0 {
		d := analyticsDuration(waits)
		out.MergeWaitP50 = &d.P50Seconds
		out.MergeWaitP90 = &d.P90Seconds
		maximum := waits[0]
		for _, wait := range waits {
			maximum = max(maximum, wait)
		}
		out.MergeWaitMax = &maximum
	}
	out.Coverage = []diagnosticsCoverage{
		{Source: "Transitions", Observed: &transitions},
		{Source: "Attempts", Observed: &attempts},
		{Source: "Usage joins", Observed: &usage, Total: &attempts},
		{Source: "Activity receipts", Observed: &receipts, Total: &receiptTotal},
		{Source: "Queue intervals", Observed: &queue, Total: &claims},
		{Source: "Skip reasons", Observed: out.Skipped, Total: out.Skipped},
		{Source: "Merge identity", Observed: &identity, Total: &out.MergeLanded},
		{Source: "Landing refusal reason"},
		{Source: "Detector tick"},
	}
	if len(out.Refused) > 0 {
		observed := 0
		for _, refusal := range out.Refused {
			observed += refusal.Count
		}
		out.Coverage[7].Observed = &observed
	}
	for _, name := range []string{"Plan gate", "Validator gate", "Gate kind", "Merge method"} {
		row := diagnosticsConfiguration{Name: name}
		if len(values[name]) > 1 {
			value := "Varies by project"
			row.Value = &value
		} else {
			for value := range values[name] {
				if value != "" {
					row.Value = &value
				}
			}
		}
		if planning > 0 && name == "Plan gate" {
			effect := fmt.Sprintf("%d recorded planning phases", planning)
			row.Effect = &effect
		}
		if validation > 0 && name == "Validator gate" {
			effect := fmt.Sprintf("%d recorded validation phases", validation)
			row.Effect = &effect
		}
		if out.MergeLanded > 0 && name == "Merge method" {
			effect := fmt.Sprintf("%d recorded landings", out.MergeLanded)
			row.Effect = &effect
		}
		out.Configuration = append(out.Configuration, row)
	}
	return out, nil
}

func diagnosticsMergeWaits(ctx context.Context, q nativeQueryer, scope nativeScope, landings []nativeAnalyticsLanding) ([]float64, error) {
	q, prepareErr := prepareReportsQuery(ctx, q, scope, "report_events")
	if prepareErr != nil {
		return nil, prepareErr
	}
	encoded, err := marshalNative(landings)
	if err != nil {
		return nil, err
	}
	statement := `SELECT json_extract(l.value,'$.landing.landed_at'),e.recorded_at,l.key,e.analytics_to_state FROM json_each(?) l JOIN report_events e ON e.organization_id=? AND e.project_id=? AND e.work_item_id=json_extract(l.value,'$.work_item_id') AND e.type='workflow.transitioned' AND (e.analytics_to_state='Merging' OR e.analytics_projection_version IS NULL) AND julianday(e.recorded_at)<=julianday(json_extract(l.value,'$.landing.landed_at')) ORDER BY l.key,e.recorded_at DESC`

	rows, err := q.QueryContext(ctx, statement, encoded, scope.organization, scope.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type wait struct {
		landing int
		seconds float64
	}
	var ordered []wait
	seen := map[int]bool{}
	for rows.Next() {
		var landed, entered, raw string
		var landing int
		if err := rows.Scan(&landed, &entered, &landing, &raw); err != nil {
			return nil, err
		}
		if raw != "Merging" {
			continue
		}
		if seen[landing] {
			continue
		}
		seen[landing] = true
		finish, err := parseTimeValue(landed)
		if err != nil {
			return nil, err
		}
		start, err := parseTimeValue(entered)
		if err != nil {
			return nil, err
		}
		ordered = append(ordered, wait{landing, finish.Sub(start).Seconds()})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].landing < ordered[j].landing })
	var waits []float64
	for _, w := range ordered {
		waits = append(waits, w.seconds)
	}
	return waits, rows.Err()
}

func diagnosticsTodoDepth(issues []analyticsResidenceIssue, states map[string]tracker.NativeState, clipped bool, at time.Time) *int {
	if clipped {
		return nil
	}
	count := 0
	for _, issue := range issues {
		if !issue.created.Before(at) {
			continue
		}
		past := issue
		past.events = nil
		for _, event := range issue.events {
			if event.at.Before(at) {
				past.events = append(past.events, event)
			}
		}
		timeline := buildAnalyticsResidenceTimeline(past, states, at)
		if timeline.issue.partial {
			return nil
		}
		if timeline.state == "Todo" {
			count++
		}
	}
	return &count
}

func diagnosticsPolicyValues(ctx context.Context, q policyQuerier, scope nativeScope) (map[string]string, error) {
	values := map[string]string{"Plan gate": "", "Validator gate": "", "Gate kind": "", "Merge method": ""}
	approval, found, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return nil, err
	}
	if !found {
		return values, nil
	}
	values["Plan gate"] = strconv.FormatBool(approval.Policy.Gates.PlanEnabled)
	values["Validator gate"] = strconv.FormatBool(approval.Policy.Gates.Validator)
	values["Gate kind"] = approval.Policy.Gates.Kind
	values["Merge method"] = approval.Policy.Gates.MergeMethod
	return values, nil
}

func diagnosticsLeaseCapacity(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) error {

	return diagnosticsLeaseCapacityFrom(ctx, q, scope, out)
}

func diagnosticsLeaseCapacityFrom(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) error {
	statement := `SELECT l.acquired_at,coalesce(l.released_at,l.expires_at),l.expires_at FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=? AND i.project_id=? AND julianday(l.acquired_at)<julianday(?) AND julianday(coalesce(l.released_at,l.expires_at))>julianday(?)`

	rows, err := q.QueryContext(ctx, statement, scope.organization, scope.project, formatHubTime(out.To), formatHubTime(out.From))
	if err != nil {
		return err
	}
	defer rows.Close()
	held := make([]time.Duration, len(out.Capacity))
	for rows.Next() {
		var acquired, released, expires string
		if err := rows.Scan(&acquired, &released, &expires); err != nil {
			return err
		}
		start, err := parseTimeValue(acquired)
		if err != nil {
			return err
		}
		finish, err := parseTimeValue(released)
		if err != nil {
			return err
		}
		expiry, err := parseTimeValue(expires)
		if err != nil {
			return err
		}
		finish = minTime(finish, expiry)
		for i, b := range out.Capacity {
			from, to := maxTime(start, b.Hour), minTime(finish, minTime(b.Hour.Add(time.Hour), out.To))
			if to.After(from) {
				held[i] += to.Sub(from)
			}
		}
	}
	for i := range out.Capacity {
		b := &out.Capacity[i]
		if held[i] > 0 {
			b.Slots += held[i].Seconds() / minTime(b.Hour.Add(time.Hour), out.To).Sub(b.Hour).Seconds()
		}
	}
	return rows.Err()
}
