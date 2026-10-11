package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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
	Requests      *requestMetricsReport      `json:"requests,omitempty"`
	From          time.Time                  `json:"from"`
	To            time.Time                  `json:"to"`
	Partial       bool                       `json:"partial"`
	Findings      any                        `json:"findings"`
	DetectorTick  *time.Time                 `json:"detector_tick"`
	Capacity      []diagnosticsCapacity      `json:"capacity"`
	Claimed       *int                       `json:"claimed"`
	Ready         *int                       `json:"ready"`
	Skipped       *int                       `json:"skipped"`
	SkipReasons   []nativeAnalyticsSkip      `json:"skip_reasons"`
	MergeEntered  int                        `json:"merge_entered"`
	MergeLanded   int                        `json:"merge_landed"`
	MergeWaitP50  *float64                   `json:"merge_wait_p50"`
	MergeWaitP90  *float64                   `json:"merge_wait_p90"`
	MergeWaitMax  *float64                   `json:"merge_wait_max"`
	Refused       []diagnosticsRefusal       `json:"refused"`
	Coverage      []diagnosticsCoverage      `json:"coverage"`
	Configuration []diagnosticsConfiguration `json:"configuration"`
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
	analytics, err := s.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{Limit: 100}, w)
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Diagnostics are temporarily unavailable")
	}
	report, err := s.readHostedDiagnostics(ctx, analytics)
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Diagnostics are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readHostedDiagnostics(ctx context.Context, analytics nativeAnalyticsReport) (diagnosticsReport, error) {
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
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
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
		var projectReady int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT id FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='scheduler.decision' AND json_extract(data_json,'$.decision.outcome')='ready' AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?) LIMIT ?)`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), maxAnalyticsPopulation+1).Scan(&projectReady); err != nil {
			return out, err
		}
		out.Partial = out.Partial || projectReady > maxAnalyticsPopulation
		ready += min(projectReady, maxAnalyticsPopulation)
		states, issues, clipped, err := readAnalyticsResidenceHistory(ctx, tx, scope, w)
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
		for _, phase := range p.Efficiency {
			if phase.Name == "planning" {
				planning += phase.Count
			}
			if phase.Name == "validation" {
				validation += phase.Count
			}
		}
		configuration, err := diagnosticsPolicyValues(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		for name, value := range configuration {
			if values[name] == nil {
				values[name] = map[string]bool{}
			}
			values[name][value] = true
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
		if err := diagnosticsLeaseCapacity(ctx, tx, scope, &out); err != nil {
			return out, err
		}
		var entered int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='workflow.transitioned' AND json_extract(data_json,'$.to_state')='Merging' AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?)`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To)).Scan(&entered); err != nil {
			return out, err
		}
		out.MergeEntered += entered
		for _, l := range p.Landings.Items {
			var at sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT max(recorded_at) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned' AND json_extract(data_json,'$.to_state')='Merging' AND julianday(recorded_at)<=julianday(?)`, scope.organization, scope.project, l.WorkItemID, formatHubTime(l.Landing.LandedAt)).Scan(&at); err != nil {
				return out, err
			}
			if at.Valid {
				start, err := parseTimeValue(at.String)
				if err != nil {
					return out, err
				}
				waits = append(waits, l.Landing.LandedAt.Sub(start).Seconds())
			}
		}
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
	for _, name := range []string{"Plan gate", "Validator gate", "Gate kind", "Merge method", "Lifetime limit"} {
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
	return out, tx.Commit()
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
	values := map[string]string{"Plan gate": "", "Validator gate": "", "Gate kind": "", "Merge method": "", "Lifetime limit": ""}
	approval, err := readProjectPolicy(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		var unavailable *nativeError
		if errors.As(err, &unavailable) && unavailable.Code == "policy_mismatch" {
			return values, nil
		}
		return nil, err
	}
	values["Plan gate"] = strconv.FormatBool(approval.Policy.Gates.PlanEnabled)
	values["Validator gate"] = strconv.FormatBool(approval.Policy.Gates.Validator)
	values["Gate kind"] = approval.Policy.Gates.Kind
	values["Merge method"] = approval.Policy.Gates.MergeMethod
	if approval.Policy.Configuration != nil || approval.Policy.Authored != nil {
		workflow, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{}, approval.Policy)
		if err != nil {
			return nil, err
		}
		values["Lifetime limit"] = fmt.Sprintf("%d sessions", workflow.Config.Agent.LifetimeSessionLimit)
	}
	return values, nil
}

func diagnosticsLeaseCapacity(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) error {
	rows, err := q.QueryContext(ctx, `SELECT l.acquired_at,coalesce(l.released_at,l.expires_at),l.expires_at FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=? AND i.project_id=? AND julianday(l.acquired_at)<julianday(?) AND julianday(coalesce(l.released_at,l.expires_at))>julianday(?)`, scope.organization, scope.project, formatHubTime(out.To), formatHubTime(out.From))
	if err != nil {
		return err
	}
	defer rows.Close()
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
		for i := range out.Capacity {
			b := &out.Capacity[i]
			end := minTime(b.Hour.Add(time.Hour), out.To)
			from, to := maxTime(start, b.Hour), minTime(finish, end)
			if to.After(from) {
				b.Slots += to.Sub(from).Seconds() / end.Sub(b.Hour).Seconds()
			}
		}
	}
	return rows.Err()
}
