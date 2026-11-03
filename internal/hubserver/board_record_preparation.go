package hubserver

import (
	"context"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardRecordPreparation struct {
	cancel  context.CancelFunc
	workers sync.WaitGroup
	ready   []chan struct{}
	cards   []tracker.NativeBoardCard
	errors  []error
}

func prepareBoardRecordPage(ctx context.Context, records []viewstore.BoardRecord, prepare func(context.Context, viewstore.BoardRecord) (tracker.NativeBoardCard, error)) *boardRecordPreparation {
	ctx, cancel := context.WithCancel(ctx)
	p := &boardRecordPreparation{cancel: cancel, ready: make([]chan struct{}, len(records)), cards: make([]tracker.NativeBoardCard, len(records)), errors: make([]error, len(records))}
	for index := range p.ready {
		p.ready[index] = make(chan struct{})
	}
	var mu sync.Mutex
	next := 0
	for range min(8, len(records)) {
		p.workers.Go(func() {
			for {
				mu.Lock()
				if ctx.Err() != nil || next == len(records) {
					mu.Unlock()
					return
				}
				index := next
				next++
				mu.Unlock()
				p.cards[index], p.errors[index] = prepare(ctx, records[index])
				close(p.ready[index])
			}
		})
	}
	return p
}
func (p *boardRecordPreparation) card(ctx context.Context, index int) (tracker.NativeBoardCard, error) {
	select {
	case <-p.ready[index]:
		return p.cards[index], p.errors[index]
	case <-ctx.Done():
		return tracker.NativeBoardCard{}, ctx.Err()
	}
}
func (p *boardRecordPreparation) close() { p.cancel(); p.workers.Wait() }

func (s *Service) prepareBoardRecord(ctx context.Context, scope nativeScope, record viewstore.BoardRecord, now time.Time, currentChange bool) (tracker.NativeBoardCard, error) {
	card, err := viewstore.NewContent(s.runDetailStore()).BoardRecord(ctx, record)
	if err != nil {
		return card, err
	}
	if err := s.hydrateWorkItemRelationshipFacts(ctx, scope, &card.Issue); err != nil {
		return card, err
	}
	if currentChange && card.Change != nil {
		summary, err := s.selectedCurrentChangeSummary(ctx, scope, record.WorkItemID, card.Change.ID, now)
		if err != nil {
			return card, err
		}
		card.Change.Status = summary.Status
	}
	return card, nil
}
