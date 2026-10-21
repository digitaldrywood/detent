package connector

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// RESTScope records connector attempts for one refresh or named outside activity.
// Stage and step are set by the owner of the scope before each synchronous step.
type RESTScope struct {
	mu        sync.Mutex
	ProjectID string
	RefreshID string
	Name      string
	stage     string
	step      string
	counts    map[RESTScopeKey]int64
	timings   map[GitHubTimingKey]GitHubTiming
}

type RESTScopeKey struct {
	Stage          string
	Step           string
	EndpointFamily string
	Outcome        string
}

type RESTScopeCount struct {
	RESTScopeKey
	Count int64
}

type GitHubTimingKey struct {
	RESTScopeKey
	QueryPurpose string
	Boundary     string
}

type GitHubTiming struct {
	GitHubTimingKey
	AttemptCount    int64
	TimedCount      int64
	ElapsedSumNS    int64
	ElapsedMaxNS    int64
	FirstObservedAt time.Time
	LastObservedAt  time.Time
}

type RESTScopeAttribution struct {
	scope        *RESTScope
	key          RESTScopeKey
	queryPurpose string
}

func (s *RESTScope) Attribution(family, queryPurpose string) RESTScopeAttribution {
	if s == nil {
		return RESTScopeAttribution{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return RESTScopeAttribution{scope: s, key: RESTScopeKey{Stage: s.stage, Step: s.step, EndpointFamily: family}, queryPurpose: queryPurpose}
}

func (a RESTScopeAttribution) Record(outcome string) {
	if a.scope == nil {
		return
	}
	key := a.key
	key.Outcome = outcome
	a.scope.mu.Lock()
	defer a.scope.mu.Unlock()
	if a.scope.counts == nil {
		a.scope.counts = make(map[RESTScopeKey]int64)
	}
	a.scope.counts[key]++
}

func (a RESTScopeAttribution) Observe(boundary, outcome string, elapsed time.Duration) {
	if a.scope == nil {
		return
	}
	key := GitHubTimingKey{RESTScopeKey: a.key, QueryPurpose: a.queryPurpose, Boundary: boundary}
	key.Outcome = outcome
	a.scope.mu.Lock()
	defer a.scope.mu.Unlock()
	if a.scope.timings == nil {
		a.scope.timings = make(map[GitHubTimingKey]GitHubTiming)
	}
	item := a.scope.timings[key]
	item.GitHubTimingKey = key
	observedAt := time.Now().UTC()
	if item.FirstObservedAt.IsZero() {
		item.FirstObservedAt = observedAt
	}
	item.LastObservedAt = observedAt
	item.AttemptCount++
	if elapsed >= 0 {
		item.TimedCount++
		item.ElapsedSumNS += int64(elapsed)
		item.ElapsedMaxNS = max(item.ElapsedMaxNS, int64(elapsed))
	}
	a.scope.timings[key] = item
}

func (s *RESTScope) Timings() []GitHubTiming {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timingsLocked()
}

func (s *RESTScope) timingsLocked() []GitHubTiming {
	out := make([]GitHubTiming, 0, len(s.timings))
	for _, item := range s.timings {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.RESTScopeKey != b.RESTScopeKey {
			return lessRESTScopeKey(a.RESTScopeKey, b.RESTScopeKey)
		}
		if a.QueryPurpose != b.QueryPurpose {
			return a.QueryPurpose < b.QueryPurpose
		}
		return a.Boundary < b.Boundary
	})
	return out
}

func lessRESTScopeKey(a, b RESTScopeKey) bool {
	if a.Stage != b.Stage {
		return a.Stage < b.Stage
	}
	if a.Step != b.Step {
		return a.Step < b.Step
	}
	if a.EndpointFamily != b.EndpointFamily {
		return a.EndpointFamily < b.EndpointFamily
	}
	return a.Outcome < b.Outcome
}

type restScopeContextKey struct{}

func WithRESTScope(ctx context.Context, scope *RESTScope) context.Context {
	return context.WithValue(ctx, restScopeContextKey{}, scope)
}

func RESTScopeFromContext(ctx context.Context) *RESTScope {
	if ctx == nil {
		return nil
	}
	scope, ok := ctx.Value(restScopeContextKey{}).(*RESTScope)
	if !ok {
		return nil
	}
	return scope
}

func (s *RESTScope) Set(stage, step string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stage, s.step = stage, step
	s.mu.Unlock()
}

func (s *RESTScope) Record(family, outcome string) {
	if s == nil {
		return
	}
	s.Attribution(family, "").Record(outcome)
}

func (s *RESTScope) Counts() []RESTScopeCount {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countsLocked()
}

func (s *RESTScope) countsLocked() []RESTScopeCount {
	out := make([]RESTScopeCount, 0, len(s.counts))
	for key, count := range s.counts {
		out = append(out, RESTScopeCount{key, count})
	}
	sort.Slice(out, func(i, j int) bool {
		return lessRESTScopeKey(out[i].RESTScopeKey, out[j].RESTScopeKey)
	})
	return out
}

func (s *RESTScope) Drain() *RESTScope {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.counts) == 0 && len(s.timings) == 0 {
		return nil
	}
	out := &RESTScope{ProjectID: s.ProjectID, RefreshID: s.RefreshID, Name: s.Name, counts: s.counts, timings: s.timings}
	s.counts = nil
	s.timings = nil
	return out
}

// LogRESTScope emits only fixed outcome and endpoint-family names, never paths.
func LogRESTScope(logger *slog.Logger, scope *RESTScope) {
	if logger == nil || scope == nil {
		return
	}
	scope.mu.Lock()
	counts, timings := scope.countsLocked(), scope.timingsLocked()
	scope.mu.Unlock()
	if len(counts) == 0 && len(timings) == 0 && scope.Name != "refresh" {
		return
	}
	var requests, deferred, refused int64
	for _, item := range counts {
		switch item.Outcome {
		case "fanout-deferred":
			deferred += item.Count
		case "reserve-refused":
			refused += item.Count
		default:
			requests += item.Count
		}
	}
	logger.Info("github rest scope usage", "project_id", scope.ProjectID, "refresh_id", scope.RefreshID,
		"scope", scope.Name, "request_count", requests, "fanout_deferred", deferred,
		"reserve_refused", refused, "steps", counts, "timings", timings,
		"timing_unit", "nanoseconds", "timing_coverage", "completed_observed_attempts",
		"http_boundary", "do_body_close", "elapsed_semantics", "overlapping_work",
		"token_elapsed", "inclusive", "unmeasured", "actor_queue_local_work")
}
