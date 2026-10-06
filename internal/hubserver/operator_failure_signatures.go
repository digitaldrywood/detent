package hubserver

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	maxFailureSignatureLength = 256
	maxFailureExampleLength   = 512
	maxFailureWorkItems       = 20
)

var failurePathDigits = regexp.MustCompile(`\d+`)

var failureSignatureReplacements = []struct {
	pattern *regexp.Regexp
	value   string
}{
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ][\d:.]+(?:Z|[+-]\d{2}:?\d{2})?\b`), "<timestamp>"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<id>"},
	{regexp.MustCompile(`\b(?:wi|prj|attempt|lease|run|session|change|version|machine|org)_[a-zA-Z0-9_-]+\b`), "<id>"},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:bytes?|[kmgt]i?b)\b`), "<bytes>"},
	{regexp.MustCompile(`\b[[:xdigit:]]{7,64}\b`), "<hash>"},
	{regexp.MustCompile(`(?:[a-zA-Z]:[\\/]|[~.]?[\\/]|[a-zA-Z0-9_.-]+[\\/])[^\s"'<>]+`), "<path>"},
	{regexp.MustCompile(`\b(attempt|run|lease|session|process|pid|issue|thread|turn)([ _:#=]+)\d+\b`), "${1}${2}<id>"},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?(\s+(?:characters?|chars?|tokens?|attempts?|items?))\b`), "<n>${1}"},
	{regexp.MustCompile(`(?i)\b(actual|max|expected|got|limit|count|size|length|input|output)([ =:]+)\d+\b`), "${1}${2}<n>"},
}

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

func boundedFailureText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	end := limit
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func failureFirstLine(value string) string {
	first, _, _ := strings.Cut(value, "\n")
	return strings.TrimSpace(first)
}

func normalizeFailureSignature(value string) string {
	value = failureFirstLine(value)
	for _, replacement := range failureSignatureReplacements {
		if replacement.value == "<path>" {
			value = replacement.pattern.ReplaceAllStringFunc(value, func(match string) string {
				return failurePathDigits.ReplaceAllString(match, "<n>")
			})
		} else if replacement.value == "<hash>" {
			value = replacement.pattern.ReplaceAllStringFunc(value, func(match string) string {
				if strings.ContainsAny(match, "abcdefABCDEF") || len(match) == 40 || len(match) == 64 {
					return replacement.value
				}
				return match
			})
		} else {
			value = replacement.pattern.ReplaceAllString(value, replacement.value)
		}
	}
	return boundedFailureText(value, maxFailureSignatureLength)
}

func failureFromAttempt(a tracker.NativeRunData, status string) (string, string) {
	if a.Runtime != nil && a.Runtime.Landing != nil && !a.Runtime.Landing.Landed {
		switch a.Runtime.Landing.RefusalKind {
		case "conflict", "base_protected":
			return "landing refused: " + a.Runtime.Landing.RefusalKind, "merge"
		}
	}
	if status != "failed" && status != "interrupted" {
		return "", ""
	}
	if f := a.TerminalFailure; f != nil && f.Error != "" {
		return f.Error, nativeFailureClass(*f)
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

func readNativeFailures(ctx context.Context, q nativeQueryer, scope nativeScope, item string, w *operatortool.AnalyticsWindow) ([]nativeFailure, bool, error) {
	query := `WITH failures AS (
SELECT a.*, CASE WHEN json_extract(data_json,'$.runtime.landing.refusal_kind') IN ('conflict','base_protected') AND coalesce(json_extract(data_json,'$.runtime.landing.landed'),0)=0
THEN coalesce(nullif(json_extract(data_json,'$.runtime.landing.observed_at'),'0001-01-01T00:00:00Z'),updated_at) ELSE updated_at END AS failed_at
FROM native_attempts a WHERE organization_id=? AND project_id=?
AND (status IN ('failed','interrupted') OR json_extract(data_json,'$.runtime.landing.refusal_kind') IN ('conflict','base_protected')))
SELECT a.id,a.work_item_id,a.status,a.started_at,a.updated_at,a.failed_at,
json_object('terminal_failure',json_extract(a.data_json,'$.terminal_failure'),'finalization',json_extract(a.data_json,'$.finalization'),'runtime',json_object('landing',json_extract(a.data_json,'$.runtime.landing'))),
(SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id),
(SELECT coalesce(sum(u.input+u.output),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id)
FROM failures a WHERE 1=1`
	args := []any{scope.organization, scope.project}
	if item != "" {
		query += " AND a.work_item_id=?"
		args = append(args, item)
	}
	if w != nil {
		query += " AND julianday(a.failed_at)>=julianday(?) AND julianday(a.failed_at)<julianday(?)"
		args = append(args, formatHubTime(w.From), formatHubTime(w.To))
	}
	query += " ORDER BY julianday(a.failed_at) DESC,a.id DESC LIMIT ?"
	args = append(args, maxAnalyticsPopulation+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	failures := []nativeFailure{}
	observed := 0
	for rows.Next() {
		if observed == maxAnalyticsPopulation {
			return failures, true, rows.Err()
		}
		observed++
		var f nativeFailure
		var status, from, to, at, raw string
		if err := rows.Scan(&f.attemptID, &f.workItem, &status, &from, &to, &at, &raw, &f.cost, &f.tokens); err != nil {
			return nil, false, err
		}
		var data tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, false, err
		}
		f.example, f.class = failureFromAttempt(data, status)
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
	return failures, false, rows.Err()
}

func groupNativeFailures(failures []nativeFailure) []nativeFailureSignature {
	type group struct {
		value nativeFailureSignature
		items map[string][]time.Time
	}
	groups := map[string]*group{}
	for _, f := range failures {
		signature := normalizeFailureSignature(f.example)
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
		example := boundedFailureText(failureFirstLine(f.example), maxFailureExampleLength)
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
	signature := normalizeFailureSignature(latest.example)
	out := &explain.FailureSignature{Signature: signature, Partial: partial}
	for _, f := range failures {
		if normalizeFailureSignature(f.example) == signature {
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
