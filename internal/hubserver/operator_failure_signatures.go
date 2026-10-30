package hubserver

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatorfailure"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	maxFailureWorkItems = 20
)

type nativeFailureSignature struct {
	Signature        string    `json:"signature"`
	Example          string    `json:"example"`
	Class            string    `json:"class"`
	Count            int       `json:"count"`
	WorkItems        []string  `json:"work_items"`
	WorkItemsCount   int       `json:"work_items_count"`
	WorkItemsPartial bool      `json:"work_items_partial"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
	Cost             float64   `json:"cost_usd"`
	Tokens           int64     `json:"tokens"`
	SlotMinutes      float64   `json:"slot_minutes"`
}

type nativeFailure struct {
	attemptID string
	workItem  string
	at        time.Time
	started   time.Time
	finished  time.Time
	example   string
	class     string
	cost      float64
	tokens    int64
}

const analyticsRollupFailures = `SELECT a.id,a.work_item_id,a.status,a.started_at,a.updated_at,a.failed_at,
a.data_json,
(SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id),
(SELECT coalesce(sum(u.input+u.output),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id)
FROM (SELECT r.organization_id,r.project_id,r.dimension AS id,r.work_item_id,r.lease_id,r.status,
r.started_at,r.updated_at,COALESCE(NULLIF(r.payload_object_key,''),NULLIF(r.data_json,'{}'),n.data_json) AS data_json,r.failed_at,r.failed_day,n.runner_id,n.role,n.report_requested_model,n.report_resolved_model
FROM analytics_hourly_rollups r CROSS JOIN report_attempts n ON n.id=r.dimension AND n.organization_id=r.organization_id AND n.project_id=r.project_id WHERE r.metric='attempt' AND r.failed_at IS NOT NULL) a WHERE a.organization_id=? AND a.project_id=?`

func readNativeFailures(ctx context.Context, q nativeQueryer, scope nativeScope, item string, w *operatortool.AnalyticsWindow) ([]nativeFailure, bool, error) {
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
	rows, err := queryAnalyticsPopulation(ctx, q, "failures", query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	type failureRow struct {
		failure                   nativeFailure
		status, from, to, at, raw string
	}
	var batch []failureRow
	partial := false
	for rows.Next() {
		if len(batch) == maxAnalyticsPopulation {
			partial = true
			break
		}
		var row failureRow
		if err := rows.Scan(&row.failure.attemptID, &row.failure.workItem, &row.status, &row.from, &row.to, &row.at, &row.raw, &row.failure.cost, &row.failure.tokens); err != nil {
			return nil, false, err
		}
		batch = append(batch, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	failures := []nativeFailure{}
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
		f.example, f.class = operatorfailure.FromAttempt(data, status)
		if f.example == "" {
			continue
		}
		f.started, err = parseTimeValue(from)
		if err != nil {
			return nil, false, err
		}
		f.finished, err = parseTimeValue(to)
		if err != nil {
			return nil, false, err
		}
		f.at, err = parseTimeValue(at)
		if err != nil {
			return nil, false, err
		}
		failures = append(failures, f)
	}
	return failures, partial, nil
}

func groupNativeFailures(failures []nativeFailure) []nativeFailureSignature {
	type group struct {
		value nativeFailureSignature
		items map[string][]time.Time
	}
	groups := map[string]*group{}
	for _, f := range failures {
		signature := operatorfailure.Normalize(f.example)
		g := groups[signature]
		if g == nil {
			g = &group{value: nativeFailureSignature{Signature: signature, Class: f.class, FirstSeen: f.at, LastSeen: f.at}, items: map[string][]time.Time{}}
			groups[signature] = g
		}
		v := &g.value
		v.Count++
		v.Cost += f.cost
		v.Tokens += f.tokens
		finished := f.finished
		if finished.IsZero() {
			finished = f.at
		}
		v.SlotMinutes += max(0, finished.Sub(f.started).Minutes())
		if f.at.Before(v.FirstSeen) {
			v.FirstSeen = f.at
		}
		example := operatorfailure.BoundedText(operatorfailure.FirstLine(f.example), operatorfailure.MaxExampleLength)
		if f.at.After(v.LastSeen) || f.at.Equal(v.LastSeen) && (v.Example == "" || example < v.Example) {
			v.LastSeen, v.Example = f.at, example
		}
		if f.class == "merge" || f.class == "infrastructure" && v.Class != "merge" {
			v.Class = f.class
		}
		g.items[f.workItem] = append(g.items[f.workItem], f.at)
	}
	out := []nativeFailureSignature{}
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

func latestNativeFailureSignature(failures []nativeFailure, partial bool) *explain.FailureSignature {
	if len(failures) == 0 {
		return nil
	}
	latest := failures[0]
	for _, f := range failures[1:] {
		if f.at.After(latest.at) || f.at.Equal(latest.at) && f.attemptID > latest.attemptID {
			latest = f
		}
	}
	signature := operatorfailure.Normalize(latest.example)
	out := &explain.FailureSignature{Signature: signature, Partial: partial}
	for _, f := range failures {
		if operatorfailure.Normalize(f.example) == signature {
			out.Count++
		}
	}
	return out
}

func nativeFailureSignaturePage(signatures []nativeFailureSignature, offset, limit int) operatortool.ReadPage[nativeFailureSignature] {
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
