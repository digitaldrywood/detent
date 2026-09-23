package connector

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// RefreshScanSerializer queues a complete tracker fetch, including hydration,
// behind other projects using the same credential. The caller must release it.
type RefreshScanSerializer interface {
	BeginRefreshScan(context.Context) (release func(), err error)
}

type graphQLPointsKey struct{}

// GraphQLPoints collects observed GraphQL cost and operation detail for one refresh.
type GraphQLPoints struct {
	total      atomic.Int64
	mu         sync.Mutex
	operations map[string]GraphQLOperation
	cache      map[string]map[string]int64
}

// GraphQLOperation is one operation's contribution to a single refresh.
type GraphQLOperation struct {
	Name           string
	Requests       int64
	Points         int64
	NodesRequested int64
	NodesReturned  int64
	WallTime       time.Duration
	Cache          map[string]int64
}

func WithGraphQLPoints(ctx context.Context) (context.Context, *GraphQLPoints) {
	points := &GraphQLPoints{}
	return context.WithValue(ctx, graphQLPointsKey{}, points), points
}

func HasGraphQLPoints(ctx context.Context) bool {
	_, ok := ctx.Value(graphQLPointsKey{}).(*GraphQLPoints)
	return ok
}

func RecordGraphQLPoints(ctx context.Context, cost int64) {
	if points, ok := ctx.Value(graphQLPointsKey{}).(*GraphQLPoints); ok && cost > 0 {
		points.total.Add(cost)
	}
}

func (p *GraphQLPoints) Total() int64 {
	if p == nil {
		return 0
	}
	return p.total.Load()
}

func RecordGraphQLOperation(ctx context.Context, operation GraphQLOperation) {
	points, ok := ctx.Value(graphQLPointsKey{}).(*GraphQLPoints)
	if !ok || operation.Name == "" {
		return
	}
	points.mu.Lock()
	defer points.mu.Unlock()
	if points.operations == nil {
		points.operations = make(map[string]GraphQLOperation)
	}
	current := points.operations[operation.Name]
	current.Name = operation.Name
	current.Requests += operation.Requests
	current.Points += operation.Points
	current.NodesRequested += operation.NodesRequested
	current.NodesReturned += operation.NodesReturned
	current.WallTime += operation.WallTime
	points.operations[operation.Name] = current
}

func RecordGraphQLCache(ctx context.Context, operation, outcome string) {
	points, ok := ctx.Value(graphQLPointsKey{}).(*GraphQLPoints)
	if !ok || operation == "" || outcome == "" {
		return
	}
	points.mu.Lock()
	defer points.mu.Unlock()
	if points.cache == nil {
		points.cache = make(map[string]map[string]int64)
	}
	if points.cache[operation] == nil {
		points.cache[operation] = make(map[string]int64)
	}
	points.cache[operation][outcome]++
}

func (p *GraphQLPoints) Operations() []GraphQLOperation {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.operations)+len(p.cache))
	seen := make(map[string]bool)
	for name := range p.operations {
		names = append(names, name)
		seen[name] = true
	}
	for name := range p.cache {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]GraphQLOperation, 0, len(names))
	for _, name := range names {
		operation := p.operations[name]
		operation.Name = name
		operation.Cache = make(map[string]int64, len(p.cache[name]))
		for outcome, count := range p.cache[name] {
			operation.Cache[outcome] = count
		}
		result = append(result, operation)
	}
	return result
}
