package hubserver

import (
	"container/heap"
	"context"
	"fmt"
	"slices"
)

type analyticsOrderedPopulation[T any] struct {
	items   []T
	compare func(T, T) int
	limit   uint64
}

func (p analyticsOrderedPopulation[T]) Len() int { return len(p.items) }
func (p analyticsOrderedPopulation[T]) Less(i, j int) bool {
	return p.compare(p.items[i], p.items[j]) > 0
}
func (p analyticsOrderedPopulation[T]) Swap(i, j int) {
	p.items[i], p.items[j] = p.items[j], p.items[i]
}
func (p *analyticsOrderedPopulation[T]) Push(value any) {
	typed, ok := value.(T)
	if !ok {
		panic("invalid analytics population value")
	}
	p.items = append(p.items, typed)
}
func (p *analyticsOrderedPopulation[T]) Pop() any {
	last := len(p.items) - 1
	value := p.items[last]
	p.items = p.items[:last]
	return value
}

func newAnalyticsOrderedPopulation[T any](ctx context.Context, name string, compare func(T, T) int) *analyticsOrderedPopulation[T] {
	return &analyticsOrderedPopulation[T]{compare: compare, limit: uint64(analyticsPopulationOffset(ctx, name)) + maxAnalyticsPopulation + 1}
}

func (p *analyticsOrderedPopulation[T]) add(value T) {
	if uint64(len(p.items)) < p.limit {
		heap.Push(p, value)
	} else if p.compare(value, p.items[0]) < 0 {
		p.items[0] = value
		heap.Fix(p, 0)
	}
}

func (p *analyticsOrderedPopulation[T]) page(ctx context.Context, name string) []T {
	slices.SortFunc(p.items, p.compare)
	offset := analyticsPopulationOffset(ctx, name)
	items := p.items[min(offset, len(p.items)):]
	if len(items) > maxAnalyticsPopulation {
		if paging := analyticsPagingFromContext(ctx); paging != nil {
			paging.next[name] = fmt.Sprintf("%s:%d:%d", name, offset+maxAnalyticsPopulation, paging.issuesOffset)
		}
	}
	return items
}
