package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	signature string
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
	return boundedFailureText(value, maxFailureSignatureLength)
}

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

func groupNativeFailures(failures []nativeFailure) []nativeFailureSignature {
	type group struct {
		value nativeFailureSignature
		items map[string][]time.Time
	}
	groups := map[string]*group{}
	for _, f := range failures {
		signature := f.signature
		if signature == "" {
			signature = normalizeFailureSignature(f.example)
		}
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

func diagnosticNativeFailures(snapshot diagnosticstore.Snapshot, values map[string]tracker.NativeRunData, w operatortool.AnalyticsWindow) []nativeFailure {
	usage := make(map[string]diagnosticstore.Usage)
	for _, row := range snapshot.Usage {
		sum := usage[row.AttemptID]
		sum.Cost += row.Cost
		sum.Tokens += row.Tokens
		usage[row.AttemptID] = sum
	}
	failures := []nativeFailure{}
	for _, attempt := range snapshot.AttemptRecords {
		value, ok := values[attempt.ID]
		if !ok {
			continue
		}
		example, class := failureFromAttempt(value, attempt.Status)
		if example == "" {
			continue
		}
		at := attempt.Event.RecordedAt
		if attempt.FailureAt != nil {
			at = *attempt.FailureAt
		}
		if at.Before(w.From) || !at.Before(w.To) {
			continue
		}
		cost := usage[attempt.ID]
		failures = append(failures, nativeFailure{attemptID: attempt.ID, workItem: string(attempt.WorkItemID), at: at, started: attempt.StartedAt, finished: attempt.UpdatedAt, example: example, class: class, cost: cost.Cost, tokens: cost.Tokens})
	}
	return failures
}

func (s *Service) readNativeFailuresTenant(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, w operatortool.AnalyticsWindow) ([]nativeFailure, bool, []string, error) {
	out := []nativeFailure{}
	unavailable := []string{}
	after, cursor, key, err := s.analyticsFactSeek(ctx, scope, w, "failures")
	if err != nil {
		return out, false, nil, err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, false, nil, workItemStorageUnavailable()
	}
	var page diagnosticstore.AnalyticsFailurePage
	var usage reportstore.AnalyticsUsagePage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(diagnosticstore.AnalyticsFailureReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		usageOwner, ok := r.(reportstore.AnalyticsUsageReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsFailurePage(ctx, diagnosticstore.AnalyticsFailureQuery{ProjectID: scope.project, WorkItemID: item, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: after.Failure})
		if err != nil {
			return err
		}
		page.Attempts = slices.DeleteFunc(page.Attempts, func(row diagnosticstore.Attempt) bool {
			return !analyticsCohortAllows(ctx, row.WorkItemID) || !analyticsCohortRunnerAllows(ctx, row.RunnerID)
		})
		if len(page.Attempts) == 0 {
			return nil
		}
		usage, err = usageOwner.ReadAnalyticsUsagePage(ctx, reportstore.AnalyticsUsageQuery{ProjectID: scope.project, Principal: principal, From: time.Time{}, To: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), Limit: maxAnalyticsPopulation})
		return err
	})
	if err != nil {
		return out, false, nil, err
	}
	partial := page.Partial || usage.Partial
	if usage.Partial {
		unavailable = append(unavailable, "failure_cost_complete_population")
	}
	if page.ProjectionMissing {
		unavailable = append(unavailable, "recorded_failure_projection_unavailable")
	}
	totals := map[string]diagnosticstore.Usage{}
	for _, row := range usage.Items {
		sum := totals[row.AttemptID]
		sum.Cost += row.Cost
		sum.Tokens += row.Input + row.Output
		totals[row.AttemptID] = sum
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, attempt := range page.Attempts {
		if attempt.Event.ID == "" {
			partial = true
			continue
		}
		value, _, err := content.AnalyticsEvent(ctx, attempt.Event)
		if errors.Is(err, artifact.ErrMissing) {
			partial = true
			unavailable = append(unavailable, "recorded_failure_detail_unavailable")
			continue
		}
		if err != nil {
			return out, false, nil, err
		}
		if value.Data.Run == nil || value.Data.Run.AttemptID != attempt.ID || value.ProjectID != scope.project || value.AggregateID != attempt.WorkItemID {
			return out, false, nil, artifact.ErrIntegrity
		}
		example, class := failureFromAttempt(*value.Data.Run, attempt.Status)
		if example == "" {
			partial = true
			unavailable = append(unavailable, "recorded_failure_reason_unavailable")
			continue
		}
		at := attempt.Event.RecordedAt
		if runtime := value.Data.Run.Runtime; runtime != nil && runtime.Landing != nil && ((runtime.Landing.RefusalKind == "conflict" || runtime.Landing.RefusalKind == "base_protected") && !runtime.Landing.Landed || runtime.Landing.GateFailed) && !runtime.Landing.ObservedAt.IsZero() {
			at = runtime.Landing.ObservedAt
		}
		if at.Before(w.From) || !at.Before(w.To) {
			continue
		}
		cost := totals[attempt.ID]
		out = append(out, nativeFailure{signature: strings.Clone(normalizeFailureSignature(example)), attemptID: attempt.ID, workItem: string(attempt.WorkItemID), at: at, started: attempt.StartedAt, finished: attempt.UpdatedAt, example: strings.Clone(boundedFailureText(failureFirstLine(example), maxFailureExampleLength)), class: class, cost: cost.Cost, tokens: cost.Tokens})
	}
	if page.Next.ID != "" {
		if len(key) == 0 {
			partial = true
			unavailable = append(unavailable, "failures_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsFactCursor(cursor, key, analyticsFactCursor{Failure: page.Next}, "failures")
			if err != nil {
				return out, false, nil, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.next["failures"] = token
			}
		}
	}
	return out, partial, slices.Compact(unavailable), nil
}
