package connector

import (
	"context"
	"log/slog"
	"sort"
	"sync"
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
	s.mu.Lock()
	if s.counts == nil {
		s.counts = make(map[RESTScopeKey]int64)
	}
	s.counts[RESTScopeKey{s.stage, s.step, family, outcome}]++
	s.mu.Unlock()
}

func (s *RESTScope) Counts() []RESTScopeCount {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RESTScopeCount, 0, len(s.counts))
	for key, count := range s.counts {
		out = append(out, RESTScopeCount{key, count})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].RESTScopeKey, out[j].RESTScopeKey
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
	})
	return out
}

func (s *RESTScope) Drain() *RESTScope {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.counts) == 0 {
		return nil
	}
	out := &RESTScope{ProjectID: s.ProjectID, RefreshID: s.RefreshID, Name: s.Name, counts: s.counts}
	s.counts = nil
	return out
}

// LogRESTScope emits only fixed outcome and endpoint-family names, never paths.
func LogRESTScope(logger *slog.Logger, scope *RESTScope) {
	if logger == nil || scope == nil {
		return
	}
	counts := scope.Counts()
	if len(counts) == 0 && scope.Name != "refresh" {
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
		"reserve_refused", refused, "steps", counts)
}
