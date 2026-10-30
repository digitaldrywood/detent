package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	maxFailureExampleLength = 512
	maxFailureWorkItems     = 20
)

func failureFromAttempt(a tracker.NativeRunData, status string) (string, string) {
	if a.Runtime != nil && a.Runtime.Landing != nil && !a.Runtime.Landing.Landed {
		switch a.Runtime.Landing.RefusalKind {
		case "conflict", "base_protected":
			return "landing refused: " + a.Runtime.Landing.RefusalKind, "merge"
		}
	}
	if f := a.TerminalFailure; f != nil && f.Error != "" && status != "cancelled" {
		return f.Error, nativeFailureClass(*f)
	}
	if r := a.Runtime; r != nil && r.Landing != nil && r.Landing.GateFailed && r.Landing.Gate != nil {
		return fmt.Sprintf("landing gate failed: exit status %d", r.Landing.Gate.ExitCode), "merge"
	}
	if status != "failed" && status != "interrupted" {
		return "", ""
	}
	if r := a.Runtime; r != nil && r.Validation != nil && r.Validation.ExitCode != 0 && r.Validation.Stage == gate.StageSourceFinalization {
		return fmt.Sprintf("source finalization gate failed: exit status %d", r.Validation.ExitCode), "attempt"
	}
	if f := a.Finalization; f != nil {
		if f.VersionError != "" {
			return f.VersionCode + ": " + f.VersionError, "attempt"
		}
		if f.Error != "" {
			return f.Error, "attempt"
		}
	}
	if f := a.TerminalFailure; f != nil {
		value := f.Error
		if value == "" {
			value = strings.TrimSpace(f.Provider + " " + f.Operation + " " + f.ProviderCode + ": " + f.Summary)
		}
		return value, nativeFailureClass(*f)
	}
	if status == "failed" {
		return "attempt failed: recorded error unavailable", "attempt"
	}
	return "", ""
}

func nativeFailureClass(f tracker.NativeTerminalFailure) string {
	if f.ErrorClass == "backend_startup" || f.ErrorClass == "protocol" || f.ErrorClass == "workspace_hook" || f.Operation == "initialize" || f.RPCCode != nil {
		return "infrastructure"
	}
	return "attempt"
}

const analyticsRollupFailures = `SELECT a.id,a.work_item_id,a.status,a.started_at,a.updated_at,a.failed_at,
a.data_json,
(SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id),
(SELECT coalesce(sum(u.input+u.output),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id)
FROM (SELECT r.organization_id,r.project_id,r.dimension AS id,r.work_item_id,r.lease_id,r.status,
r.started_at,r.updated_at,COALESCE(NULLIF(r.payload_object_key,''),NULLIF(r.data_json,'{}'),n.data_json) AS data_json,r.failed_at,r.failed_day,n.runner_id,n.role,n.report_requested_model,n.report_resolved_model
FROM analytics_hourly_rollups r CROSS JOIN report_attempts n ON n.id=r.dimension AND n.organization_id=r.organization_id AND n.project_id=r.project_id WHERE r.metric='attempt' AND r.failed_at IS NOT NULL) a WHERE a.organization_id=? AND a.project_id=?`

func readNativeFailures(ctx context.Context, q nativeQueryer, scope nativeScope, item string, w *operatortool.AnalyticsWindow) ([]hubanalytics.Failure, bool, error) {
	q, err := prepareReportsQuery(ctx, q, scope, "report_attempts")
	if err != nil {
		return nil, false, err
	}
	query := `SELECT a.id,a.work_item_id,a.status,a.started_at,a.updated_at,a.report_failed_at,
a.data_json,
(SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id),
(SELECT coalesce(sum(u.input+u.output),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id)
FROM report_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.report_failed_at IS NOT NULL`
	failedAt := "a.report_failed_at"
	_, projected := q.(analyticsRollupReader)
	if projected {
		query, failedAt = analyticsRollupFailures, "a.failed_day"
	}
	args := []any{scope.organization, scope.project}
	if item != "" {
		query += " AND a.work_item_id=?"
		args = append(args, item)
	}
	if w != nil {
		if projected {
			query += " AND " + failedAt + ">=julianday(?) AND " + failedAt + "<julianday(?)"
			args = append(args, formatHubTime(w.From), formatHubTime(w.To))
		} else {
			query += " AND " + failedAt + ">=? AND " + failedAt + "<?"
			args = append(args, changeColumnTime(w.From), changeColumnTime(w.To))
		}
	}
	scoped, scopedArgs, err := reportsScopedClause(ctx, "a")
	if err != nil {
		return nil, false, err
	}
	query += scoped
	args = append(args, scopedArgs...)
	query += " ORDER BY " + failedAt + " DESC,a.id DESC LIMIT ? OFFSET ?"
	rows, err := hubanalytics.QueryAnalyticsPopulation(ctx, q, "failures", query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	type failureRow struct {
		failure                   hubanalytics.Failure
		status, from, to, at, raw string
	}
	var batch []failureRow
	partial := false
	for rows.Next() {
		if len(batch) == hubanalytics.MaxPopulation {
			partial = true
			break
		}
		var row failureRow
		if err := rows.Scan(&row.failure.AttemptID, &row.failure.WorkItem, &row.status, &row.from, &row.to, &row.at, &row.raw, &row.failure.Cost, &row.failure.Tokens); err != nil {
			return nil, false, err
		}
		batch = append(batch, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	failures := []hubanalytics.Failure{}
	for _, row := range batch {
		f, status, from, to, at, raw := row.failure, row.status, row.from, row.to, row.at, row.raw
		var stored struct {
			Detail          *runDetailReference            `json:"detail"`
			TerminalFailure *tracker.NativeTerminalFailure `json:"terminal_failure"`
			Finalization    *tracker.NativeFinalization    `json:"finalization"`
			Runtime         *struct {
				Landing    *tracker.NativeLandingReceipt `json:"landing"`
				Validation *gate.CommandResult           `json:"validation"`
			} `json:"runtime"`
		}
		if err := decodeStorageJSON(payloadQueryContext(ctx, q), []byte(raw), &stored); err != nil {
			return nil, false, err
		}
		data := tracker.NativeRunData{TerminalFailure: stored.TerminalFailure, Finalization: stored.Finalization}
		if stored.Detail != nil {
			if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
				return nil, false, err
			}
		} else if stored.Runtime != nil {
			data.Runtime = &tracker.NativeRuntimeObservation{Landing: stored.Runtime.Landing, Validation: stored.Runtime.Validation}
		}
		f.Example, f.Class = failureFromAttempt(data, status)
		if f.Example == "" {
			continue
		}
		f.Started, err = parseTimeValue(from)
		if err != nil {
			return nil, false, err
		}
		f.Finished, err = parseTimeValue(to)
		if err != nil {
			return nil, false, err
		}
		f.At, err = parseTimeValue(at)
		if err != nil {
			return nil, false, err
		}
		failures = append(failures, f)
	}
	return failures, partial, nil
}

func groupNativeFailures(failures []hubanalytics.Failure) []hubanalytics.FailureSignature {
	type group struct {
		value hubanalytics.FailureSignature
		items map[string][]time.Time
	}
	groups := map[string]*group{}
	for _, f := range failures {
		signature := hubhealth.NormalizeFailureSignature(f.Example)
		g := groups[signature]
		if g == nil {
			g = &group{value: hubanalytics.FailureSignature{Signature: signature, Class: f.Class, FirstSeen: f.At, LastSeen: f.At}, items: map[string][]time.Time{}}
			groups[signature] = g
		}
		v := &g.value
		v.Count++
		v.Cost += f.Cost
		v.Tokens += f.Tokens
		finished := f.Finished
		if finished.IsZero() {
			finished = f.At
		}
		v.SlotMinutes += max(0, finished.Sub(f.Started).Minutes())
		if f.At.Before(v.FirstSeen) {
			v.FirstSeen = f.At
		}
		example := hubhealth.BoundedFailureText(hubhealth.FailureFirstLine(f.Example), maxFailureExampleLength)
		if f.At.After(v.LastSeen) || f.At.Equal(v.LastSeen) && (v.Example == "" || example < v.Example) {
			v.LastSeen, v.Example = f.At, example
		}
		if f.Class == "merge" || f.Class == "infrastructure" && v.Class != "merge" {
			v.Class = f.Class
		}
		g.items[f.WorkItem] = append(g.items[f.WorkItem], f.At)
	}
	out := []hubanalytics.FailureSignature{}
	for _, g := range groups {
		v := g.value
		v.WorkItems = []string{}
		for id, times := range g.items {
			v.WorkItems = append(v.WorkItems, id)
			if len(times) < 5 {
				continue
			}
			sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
			for i := 4; i < len(times); i++ {
				if times[i].Sub(times[i-4]) <= time.Hour {
					v.Class = "retry storm"
					break
				}
			}
		}
		sort.Strings(v.WorkItems)
		v.WorkItemsCount = len(v.WorkItems)
		v.WorkItemsPartial = len(v.WorkItems) > maxFailureWorkItems
		v.WorkItems = v.WorkItems[:min(len(v.WorkItems), maxFailureWorkItems)]
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Signature < out[j].Signature
	})
	return out
}

func latestNativeFailureSignature(failures []hubanalytics.Failure, partial bool) *explain.FailureSignature {
	if len(failures) == 0 {
		return nil
	}
	latest := failures[0]
	for _, f := range failures[1:] {
		if f.At.After(latest.At) || f.At.Equal(latest.At) && f.AttemptID > latest.AttemptID {
			latest = f
		}
	}
	signature := hubhealth.NormalizeFailureSignature(latest.Example)
	out := &explain.FailureSignature{Signature: signature, Partial: partial}
	for _, f := range failures {
		if hubhealth.NormalizeFailureSignature(f.Example) == signature {
			out.Count++
		}
	}
	return out
}

func nativeFailureSignaturePage(signatures []hubanalytics.FailureSignature, offset, limit int) operatortool.ReadPage[hubanalytics.FailureSignature] {
	page := operatortool.OffsetPage(signatures, offset, limit)
	size := 2
	for i, item := range page.Items {
		raw, err := json.Marshal(item)
		if err != nil {
			return page
		}
		if i > 0 && size+len(raw)+1 > operatortool.WorkListPageBytes {
			page.Items = page.Items[:i]
			next := offset + i
			page.NextOffset = &next
			break
		}
		size += len(raw) + 1
	}
	return page
}
