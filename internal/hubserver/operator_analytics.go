package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const maxAnalyticsPopulation = 1000

type nativeAnalyticsBucket struct {
	LaneResidence               operatortool.AnalyticsResidenceSummary `json:"lane_residence"`
	ProviderCapacityWaitSeconds float64                                `json:"provider_capacity_wait_seconds"`
	From                        time.Time                              `json:"from"`
	To                          time.Time                              `json:"to"`
	Shipped                     int                                    `json:"shipped"`
	Sessions                    int                                    `json:"sessions"`
	Tokens                      int64                                  `json:"tokens"`
	Cost                        float64                                `json:"cost_usd"`
	Activity                    nativeAnalyticsActivityTiming          `json:"activity"`
	QueueSeconds                float64                                `json:"queue_seconds"`
}
type nativeAnalyticsLanding struct {
	ChangeID   string                   `json:"change_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Landing    tracker.ChangeLanding    `json:"landing"`
}
type nativeAnalyticsPhase struct {
	Name           string  `json:"name"`
	Count          int     `json:"count"`
	Seconds        float64 `json:"seconds"`
	AverageSeconds float64 `json:"average_seconds"`
}
type nativeAnalyticsAttempt struct {
	Pipeline            []gate.PipelineTiming `json:"pipeline,omitempty"`
	PipelineDropped     int                   `json:"pipeline_dropped,omitempty"`
	activityRaw         string
	UsageRowsObserved   int                              `json:"usage_rows_observed"`
	Identity            agentidentity.Identity           `json:"identity,omitzero"`
	Landing             *tracker.NativeLandingReceipt    `json:"landing,omitempty"`
	AttemptID           string                           `json:"attempt_id"`
	WorkItemID          string                           `json:"work_item_id"`
	Status              string                           `json:"status"`
	StartedAt           time.Time                        `json:"started_at"`
	ObservedAt          time.Time                        `json:"observed_at"`
	Phases              []tracker.NativePhase            `json:"phases"`
	Activity            *workflowmetrics.ActivityProfile `json:"activity,omitempty"`
	ActivityUnavailable string                           `json:"activity_unavailable,omitempty"`
}
type nativeAnalyticsSkip struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}
type nativeAnalyticsOutcome struct {
	Shipped            int      `json:"shipped"`
	CostPerShipped     *float64 `json:"cost_per_shipped_usd,omitempty"`
	TokensPerShipped   *float64 `json:"tokens_per_shipped,omitempty"`
	PopulationObserved int      `json:"population_observed"`
	PopulationTotal    *int     `json:"population_total,omitempty"`
	Clipped            bool     `json:"clipped"`
}
type nativeAnalyticsProject struct {
	PopulationCursors    map[string]string `json:"population_cursors,omitempty"`
	pipelineTimings      []gate.PipelineTiming
	allAttempts          []nativeAnalyticsAttempt
	allLandings          []nativeAnalyticsLanding
	Pipeline             pipelineReport                                `json:"pipeline"`
	PipelineReceipts     operatortool.ReadPage[gate.PipelineTiming]    `json:"pipeline_receipts"`
	Quality              analyticsQuality                              `json:"quality"`
	FailureSignatures    operatortool.ReadPage[nativeFailureSignature] `json:"failure_signatures"`
	LaneResidence        operatortool.AnalyticsResidence               `json:"lane_residence"`
	ProviderCapacityWait nativeAnalyticsCapacityWait                   `json:"provider_capacity_wait"`
	ProjectID            string                                        `json:"project_id"`
	Source               string                                        `json:"source"`
	SourceAt             time.Time                                     `json:"source_at"`
	Window               operatortool.AnalyticsWindow                  `json:"window"`
	Digest               []nativeAnalyticsBucket                       `json:"digest"`
	Efficiency           []nativeAnalyticsPhase                        `json:"efficiency"`
	CostPerOutcome       nativeAnalyticsOutcome                        `json:"cost_per_outcome"`
	Landings             operatortool.ReadPage[nativeAnalyticsLanding] `json:"landings"`
	Attempts             operatortool.ReadPage[nativeAnalyticsAttempt] `json:"attempts"`
	Activity             nativeAnalyticsActivity                       `json:"activity"`
	QueueTime            operatortool.AnalyticsQueue                   `json:"queue_time"`
	SkipReasons          []nativeAnalyticsSkip                         `json:"skip_reasons"`
	PopulationLimit      int                                           `json:"population_limit"`
	AttemptsObserved     int                                           `json:"attempts_observed"`
	DecisionsObserved    int                                           `json:"decisions_observed"`
	UsageRowsObserved    int                                           `json:"usage_rows_observed"`
	Partial              bool                                          `json:"partial"`
	Unavailable          []string                                      `json:"unavailable"`
}
type nativeAnalyticsReport struct {
	Requests       *requestMetricsReport        `json:"requests,omitempty"`
	OrganizationID string                       `json:"organization_id"`
	Projects       []nativeAnalyticsProject     `json:"projects"`
	ObservedAt     time.Time                    `json:"observed_at"`
	Window         operatortool.AnalyticsWindow `json:"window"`
	NextOffset     *int                         `json:"next_offset,omitempty"`
}

func (s *Service) executeAnalyticsRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, w, err := operatortool.DecodeAnalytics(call.Name, call.Arguments, s.config.now().UTC())
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if s.database == nil || s.config.Hosted == nil {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	if _, _, cursorErr := analyticsPagingContext(ctx, r.PopulationCursor); cursorErr != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	report, err := s.readAnalyticsReport(ctx, credential, r, w)
	if err != nil {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	if call.Name == operatortool.Reports {
		requests, visible, err := s.adminRequestMetrics(ctx)
		if err != nil {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		if visible {
			report.Requests = &requests
		}
	}
	return hubOperatorResult(report)
}

func (s *Service) readAnalyticsReport(ctx context.Context, credential apiCredential, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (nativeAnalyticsReport, error) {
	_, _, pagingErr := analyticsPagingContext(ctx, r.PopulationCursor)
	if pagingErr != nil {
		return nativeAnalyticsReport{}, pagingErr
	}
	report := nativeAnalyticsReport{OrganizationID: s.config.Hosted.OrganizationID, Projects: []nativeAnalyticsProject{}, ObservedAt: s.config.now().UTC(), Window: w}
	projects, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return report, err
	}
	projects = slices.DeleteFunc(projects, func(id string) bool {
		if r.ProjectID != "" && id != r.ProjectID {
			return true
		}
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id})
		return err != nil
	})
	sort.Strings(projects)
	end := min(len(projects), r.Offset+r.Limit)
	if end < len(projects) {
		report.NextOffset = &end
	}
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, id := range projects[min(r.Offset, len(projects)):end] {
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(id), credential: credential}
		projectCtx, paging, err := analyticsPagingContext(context.WithValue(ctx, analyticsPagingKey{}, nil), r.PopulationCursor)
		if err != nil {
			return report, err
		}
		value, err := readNativeAnalytics(projectCtx, tx, scope, r, w)
		if err != nil {
			return report, err
		}
		rows, err := readUsageRows(projectCtx, tx, s.config.Hosted.OrganizationID, usageWindow{From: w.From, To: w.To, Hourly: w.Bucket < 24*time.Hour}, []string{id}, maxAnalyticsPopulation+1)
		if err != nil {
			return report, err
		}
		rows = slices.DeleteFunc(rows, func(row usageRow) bool { return !row.Period.Before(w.To) })
		if len(rows) > maxAnalyticsPopulation {
			value.Partial = true
			value.CostPerOutcome.Clipped = true
			rows = rows[:maxAnalyticsPopulation]
		}
		value.Pipeline.Partial = value.Pipeline.Partial || value.Partial
		if paging.name == "usage" || paging.name == "attempts" || paging.name == "landings" {
			value.CostPerOutcome.Clipped = true
		}
		value.UsageRowsObserved = len(rows)
		var cost float64
		var tokens int64
		sessions := map[string]bool{}
		for _, row := range rows {
			cost += row.Cost
			tokens += row.tokens()
			sessions[row.AttemptID] = true
			if i := nativeAnalyticsBucketIndex(row.Period, w); i >= 0 {
				value.Digest[i].Cost += row.Cost
				value.Digest[i].Tokens += row.tokens()
			}
			if row.Period.After(value.SourceAt) {
				value.SourceAt = row.Period
			}
		}

		partialHour := !w.From.Equal(w.From.Truncate(time.Hour)) || !w.To.Equal(w.To.Truncate(time.Hour))
		value.CostPerOutcome.PopulationObserved = len(sessions)
		if !value.CostPerOutcome.Clipped {
			total := len(sessions)
			value.CostPerOutcome.PopulationTotal = &total
		}
		if value.CostPerOutcome.Shipped > 0 && len(sessions) > 0 {
			c := cost / float64(value.CostPerOutcome.Shipped)
			t := float64(tokens) / float64(value.CostPerOutcome.Shipped)
			value.CostPerOutcome.CostPerShipped = &c
			value.CostPerOutcome.TokensPerShipped = &t
		}
		if value.Partial {
			value.Unavailable = append(value.Unavailable, "cost_per_outcome_complete_population")
		}
		if partialHour {
			value.Unavailable = append(value.Unavailable, "usage_partial_hour_attribution")
		}
		if len(sessions) == 0 {
			value.Unavailable = append(value.Unavailable, "recorded_usage")
		}
		if value.SourceAt.IsZero() {
			value.Unavailable = append(value.Unavailable, "recorded_source_time")
		}
		report.Projects = append(report.Projects, value)
	}
	return report, nil
}

func nativeAnalyticsBucketIndex(at time.Time, w operatortool.AnalyticsWindow) int {
	if at.Before(w.From) || !at.Before(w.To) {
		return -1
	}
	return int(at.Sub(w.From) / w.Bucket)
}

func readNativeAnalytics(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (nativeAnalyticsProject, error) {
	ctx, paging, pagingErr := analyticsPagingContext(ctx, r.PopulationCursor)
	if pagingErr != nil {
		return nativeAnalyticsProject{}, pagingErr
	}
	out := nativeAnalyticsProject{ProjectID: string(scope.project), Source: "native_change_landing_and_recorded_runtime", Window: w, PopulationLimit: maxAnalyticsPopulation, Unavailable: []string{"private_instruction_causality", "receipt_efficiency_quantiles"}, Digest: []nativeAnalyticsBucket{}, Efficiency: []nativeAnalyticsPhase{}, SkipReasons: []nativeAnalyticsSkip{}}
	failures, partial, err := readNativeFailures(ctx, q, scope, "", &w)
	if err != nil {
		return out, err
	}
	out.FailureSignatures = nativeFailureSignaturePage(groupNativeFailures(failures), r.RowOffset, r.Limit)
	out.Partial = partial
	if partial {
		out.Unavailable = append(out.Unavailable, "failure_signatures_complete_population")
	}
	for _, failure := range failures {
		if failure.at.After(out.SourceAt) {
			out.SourceAt = failure.at
		}
	}
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		to := from.Add(w.Bucket)
		if to.After(w.To) {
			to = w.To
		}
		out.Digest = append(out.Digest, nativeAnalyticsBucket{From: from, To: to})
	}
	landings, err := nativeAnalyticsLandings(ctx, q, scope, w)
	if err != nil {
		return out, err
	}
	if len(landings) > maxAnalyticsPopulation {
		out.Partial = true
		out.CostPerOutcome.Clipped = true
		landings = landings[:maxAnalyticsPopulation]
	}
	out.allLandings = landings
	out.CostPerOutcome.Shipped = len(landings)
	for _, l := range landings {
		if i := nativeAnalyticsBucketIndex(l.Landing.LandedAt, w); i >= 0 {
			out.Digest[i].Shipped++
		}
		if l.Landing.LandedAt.After(out.SourceAt) {
			out.SourceAt = l.Landing.LandedAt
		}
	}
	out.Landings = operatortool.OffsetPage(landings, r.RowOffset, r.Limit)
	population, err := loadNativeAnalyticsAttempts(ctx, q, scope, w, "")
	if err != nil {
		return out, err
	}
	attempts := population.Items
	out.Partial = out.Partial || population.Partial || r.PopulationCursor != ""
	if population.Malformed {
		out.Unavailable = append(out.Unavailable, "recorded_phases_malformed")
	}
	if population.Clipped {
		out.CostPerOutcome.Clipped = true
		out.Activity.Partial = true
		for i := range out.Digest {
			out.Digest[i].Activity.Partial = true
		}
	}
	phases := map[string]nativeAnalyticsPhase{}
	for i := range attempts {
		a := &attempts[i]
		for _, p := range a.Phases {
			if p.FinishedAt.IsZero() || !p.FinishedAt.After(p.StartedAt) || p.FinishedAt.Before(w.From) || !p.FinishedAt.Before(w.To) {
				continue
			}
			phase := phases[p.Name]
			phase.Name = p.Name
			phase.Count++
			phase.Seconds += p.FinishedAt.Sub(p.StartedAt).Seconds()
			phase.AverageSeconds = phase.Seconds / float64(phase.Count)
			phases[p.Name] = phase
		}
		if a.ObservedAt.After(out.SourceAt) {
			out.SourceAt = a.ObservedAt
		}
		if len(a.Pipeline) == 0 && a.Landing == nil {
			out.Pipeline.Partial = true
		}
		out.pipelineTimings = append(out.pipelineTimings, a.Pipeline...)
		if a.PipelineDropped > 0 {
			out.Pipeline.Partial = true
		}
		if a.Landing != nil {
			out.pipelineTimings = append(out.pipelineTimings, a.Landing.Pipeline...)
			if a.Landing.PipelineDropped > 0 {
				out.Pipeline.Partial = true
			}
			for _, result := range []struct {
				receipt *gate.CommandResult
				stage   string
			}{{a.Landing.Gate, "landing_validation"}, {a.Landing.Barrier, "barrier"}} {
				if result.receipt != nil {
					out.pipelineTimings = append(out.pipelineTimings, result.receipt.PipelineTiming(result.stage))
					if result.receipt.Pipeline != nil {
						out.pipelineTimings = append(out.pipelineTimings, result.receipt.Pipeline.Timings...)
						out.Pipeline.Partial = out.Pipeline.Partial || result.receipt.Pipeline.Dropped > 0
					}
				}
			}
		}
		projectNativeAnalyticsActivity(&out, a, a.activityRaw)
	}
	out.AttemptsObserved = len(attempts)
	for _, a := range attempts {
		if i := nativeAnalyticsBucketIndex(a.StartedAt, w); i >= 0 {
			out.Digest[i].Sessions++
		}
	}
	out.allAttempts = attempts
	out.Attempts = nativeAnalyticsAttemptPage(attempts, r.RowOffset, r.Limit)
	if out.Activity.ProfilesObserved == 0 {
		out.Unavailable = append(out.Unavailable, "instruction_activity")
	}
	if out.Activity.Partial {
		out.Unavailable = append(out.Unavailable, "activity_complete_window")
	}

	for _, p := range phases {
		out.Efficiency = append(out.Efficiency, p)
	}
	sort.Slice(out.Efficiency, func(i, j int) bool { return out.Efficiency[i].Name < out.Efficiency[j].Name })
	if len(phases) == 0 {
		out.Unavailable = append(out.Unavailable, "recorded_phases")
	}
	rows, err := queryAnalyticsPopulation(ctx, q, "decisions", `SELECT coalesce(json_extract(data_json,'$.decision.source'),''), coalesce(json_extract(data_json,'$.decision.reason'),''), recorded_at
FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='scheduler.decision' AND json_extract(data_json,'$.decision.outcome')='skipped'
AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?) ORDER BY recorded_at,id LIMIT ? OFFSET ?`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	skips := map[string]nativeAnalyticsSkip{}
	for rows.Next() {
		if out.DecisionsObserved == maxAnalyticsPopulation {
			out.Partial = true
			break
		}
		var source, reason, at string
		if err := rows.Scan(&source, &reason, &at); err != nil {
			return out, err
		}
		key := source + "\x00" + reason
		value := skips[key]
		value.Source = source
		value.Reason = reason
		value.Count++
		skips[key] = value
		out.DecisionsObserved++
		when, err := parseTimeValue(at)
		if err != nil {
			return out, err
		}
		if when.After(out.SourceAt) {
			out.SourceAt = when
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, s := range skips {
		out.SkipReasons = append(out.SkipReasons, s)
	}
	sort.Slice(out.SkipReasons, func(i, j int) bool {
		a, b := out.SkipReasons[i], out.SkipReasons[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Reason < b.Reason
	})
	if out.DecisionsObserved == 0 {
		out.Unavailable = append(out.Unavailable, "recorded_skip_reasons")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := projectNativeAnalyticsCapacityWait(ctx, q, scope, &out); err != nil {
		return out, err
	}
	if err := projectNativeAnalyticsResidence(ctx, q, scope, r, &out); err != nil {
		return out, err
	}
	if err := readPipelineBarriers(ctx, q, scope, &out); err != nil {
		return out, err
	}
	out.PopulationCursors = paging.next
	out.Partial = out.Partial || paging.name != ""
	pipelinePartial := out.Pipeline.Partial
	out.Pipeline = summarizePipelineTimings(out.pipelineTimings, w)
	out.Pipeline.Partial = out.Pipeline.Partial || pipelinePartial || population.Partial || out.LaneResidence.Partial || out.Partial
	if len(out.Pipeline.Stages) == 0 {
		out.Pipeline.Partial = true
		out.Unavailable = append(out.Unavailable, "recorded_pipeline_timings")
	}
	out.PipelineReceipts = pipelineTimingPage(uniquePipelineTimings(out.pipelineTimings, w), r.RowOffset, r.Limit)
	out.Quality, err = readAnalyticsQuality(ctx, q, scope, r, w)
	if err != nil {
		return out, err
	}
	out.Pipeline.Partial = out.Pipeline.Partial || out.Partial || len(paging.next) > 0
	return out, nil
}

func nativeAnalyticsLandings(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) ([]nativeAnalyticsLanding, error) {
	rows, err := queryAnalyticsPopulation(ctx, q, "landings", `SELECT c.id,c.work_item_id,json_extract(c.record_json,'$.landed') FROM change_requests c
JOIN change_versions v ON v.change_id=c.id AND v.id=json_extract(c.record_json,'$.landed.version_id')
WHERE c.organization_id=? AND c.project_id=? AND json_extract(c.record_json,'$.landed.head_sha')=json_extract(v.record_json,'$.head_sha')
AND length(json_extract(c.record_json,'$.landed.merge_sha')) IN (40,64)
AND julianday(json_extract(c.record_json,'$.landed.landed_at'))>=julianday(?) AND julianday(json_extract(c.record_json,'$.landed.landed_at'))<julianday(?)
ORDER BY json_extract(c.record_json,'$.landed.landed_at'),c.id LIMIT ? OFFSET ?`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []nativeAnalyticsLanding{}
	for rows.Next() {
		var landing nativeAnalyticsLanding
		var raw string
		if err := rows.Scan(&landing.ChangeID, &landing.WorkItemID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &landing.Landing); err != nil {
			return nil, err
		}
		landing.Landing.Quality = nil
		out = append(out, landing)
	}
	return out, rows.Err()
}
