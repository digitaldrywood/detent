package reports

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	maxFailureSignatureLength = 256
	MaxFailureExampleLength   = 512
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

type NativeFailureSignature struct {
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

type NativeFailure struct {
	Signature string    `json:"-"`
	AttemptID string    `json:"-"`
	WorkItem  string    `json:"-"`
	At        time.Time `json:"-"`
	Started   time.Time `json:"-"`
	Finished  time.Time `json:"-"`
	Example   string    `json:"-"`
	Class     string    `json:"-"`
	Cost      float64   `json:"-"`
	Tokens    int64     `json:"-"`
}

func FailureFirstLine(value string) string {
	first, _, _ := strings.Cut(value, "\n")
	return strings.TrimSpace(first)
}

func NormalizeFailureSignature(value string) string {
	value = FailureFirstLine(value)
	for _, replacement := range failureSignatureReplacements {
		switch replacement.value {
		case "<path>":
			value = replacement.pattern.ReplaceAllStringFunc(value, func(match string) string {
				return failurePathDigits.ReplaceAllString(match, "<n>")
			})
		case "<hash>":
			value = replacement.pattern.ReplaceAllStringFunc(value, func(match string) string {
				if strings.ContainsAny(match, "abcdefABCDEF") || len(match) == 40 || len(match) == 64 {
					return replacement.value
				}
				return match
			})
		default:
			value = replacement.pattern.ReplaceAllString(value, replacement.value)
		}
	}
	return hubapi.BoundedFailureText(value, maxFailureSignatureLength)
}

func FailureFromAttempt(a tracker.NativeRunData, status string) (string, string) {
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

func GroupNativeFailures(failures []NativeFailure) []NativeFailureSignature {
	type group struct {
		value NativeFailureSignature
		items map[string][]time.Time
	}
	groups := map[string]*group{}
	for _, f := range failures {
		signature := f.Signature
		if signature == "" {
			signature = NormalizeFailureSignature(f.Example)
		}
		g := groups[signature]
		if g == nil {
			g = &group{value: NativeFailureSignature{Signature: signature, Class: f.Class, FirstSeen: f.At, LastSeen: f.At}, items: map[string][]time.Time{}}
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
		example := hubapi.BoundedFailureText(FailureFirstLine(f.Example), MaxFailureExampleLength)
		if f.At.After(v.LastSeen) || f.At.Equal(v.LastSeen) && (v.Example == "" || example < v.Example) {
			v.LastSeen, v.Example = f.At, example
		}
		if f.Class == "merge" || f.Class == "infrastructure" && v.Class != "merge" {
			v.Class = f.Class
		}
		g.items[f.WorkItem] = append(g.items[f.WorkItem], f.At)
	}
	out := []NativeFailureSignature{}
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

func LatestNativeFailureSignature(failures []NativeFailure, partial bool) *explain.FailureSignature {
	if len(failures) == 0 {
		return nil
	}
	latest := failures[0]
	for _, f := range failures[1:] {
		if f.At.After(latest.At) || f.At.Equal(latest.At) && f.AttemptID > latest.AttemptID {
			latest = f
		}
	}
	signature := NormalizeFailureSignature(latest.Example)
	out := &explain.FailureSignature{Signature: signature, Partial: partial}
	for _, f := range failures {
		if NormalizeFailureSignature(f.Example) == signature {
			out.Count++
		}
	}
	return out
}

func NativeFailureSignaturePage(signatures []NativeFailureSignature, offset, limit int) operatortool.ReadPage[NativeFailureSignature] {
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

func DiagnosticNativeFailures(snapshot diagnosticstore.Snapshot, values map[string]tracker.NativeRunData, w operatortool.AnalyticsWindow) []NativeFailure {
	usage := make(map[string]diagnosticstore.Usage)
	for _, row := range snapshot.Usage {
		sum := usage[row.AttemptID]
		sum.Cost += row.Cost
		sum.Tokens += row.Tokens
		usage[row.AttemptID] = sum
	}
	failures := []NativeFailure{}
	for _, attempt := range snapshot.AttemptRecords {
		value, ok := values[attempt.ID]
		if !ok {
			continue
		}
		example, class := FailureFromAttempt(value, attempt.Status)
		if example == "" {
			continue
		}
		at := attempt.Event.RecordedAt
		if attempt.FailureAt != nil && class != "merge" {
			at = *attempt.FailureAt
		}
		if at.Before(w.From) || !at.Before(w.To) {
			continue
		}
		cost := usage[attempt.ID]
		failures = append(failures, NativeFailure{AttemptID: attempt.ID, WorkItem: string(attempt.WorkItemID), At: at, Started: attempt.StartedAt, Finished: attempt.UpdatedAt, Example: example, Class: class, Cost: cost.Cost, Tokens: cost.Tokens})
	}
	return failures
}
