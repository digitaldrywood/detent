package connector

import (
	"context"
	"sync/atomic"
)

// RefreshScanSerializer queues a complete tracker fetch, including hydration,
// behind other projects using the same credential. The caller must release it.
type RefreshScanSerializer interface {
	BeginRefreshScan(context.Context) (release func(), err error)
}

type graphQLPointsKey struct{}

// GraphQLPoints counts response-reported points for one refresh context.
type GraphQLPoints struct{ total atomic.Int64 }

func WithGraphQLPoints(ctx context.Context) (context.Context, *GraphQLPoints) {
	points := &GraphQLPoints{}
	return context.WithValue(ctx, graphQLPointsKey{}, points), points
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
