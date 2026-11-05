package hubserver

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
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

type boardPreparedRecord struct {
	record viewstore.BoardRecord
	card   tracker.NativeBoardCard
	err    error
}

func (s *Service) hydrateBoardRecords(ctx context.Context, scope nativeScope, records []viewstore.BoardRecord) map[tracker.NativeWorkItemID]boardPreparedRecord {
	refs := make([]attachment.ArchiveReference, 0, len(records))
	for _, record := range records {
		refs = append(refs, record.Payload)
	}
	finishPayloads := timeSpan(ctx, "board_payloads")
	s.prefetchDetails(ctx, string(scope.organization), refs)
	decoded := prepareBoardRecordPage(ctx, records, func(ctx context.Context, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
		return viewstore.NewContent(s.runDetailStoreFor(ctx)).BoardRecord(ctx, row)
	})
	cards := make([]tracker.NativeBoardCard, len(records))
	errors := make([]error, len(records))
	issues := []*tracker.NativeIssue{}
	for index := range records {
		cards[index], errors[index] = decoded.card(ctx, index)
		if errors[index] == nil {
			issues = append(issues, &cards[index].Issue)
		}
	}
	decoded.close()
	finishPayloads()
	finishRelationships := timeSpan(ctx, "board_relationships")
	relationshipErr := s.hydrateWorkItemRelationshipFactsPage(ctx, scope, issues)
	finishRelationships()
	prepared := make(map[tracker.NativeWorkItemID]boardPreparedRecord, len(records))
	for index, row := range records {
		if errors[index] == nil {
			errors[index] = relationshipErr
		}
		prepared[row.WorkItemID] = boardPreparedRecord{record: row, card: cards[index], err: errors[index]}
	}
	return prepared
}

func (s *Service) prepareBoardPage(ctx context.Context, scope nativeScope, records []viewstore.BoardRecord, now time.Time, currentChange bool, prepared map[tracker.NativeWorkItemID]boardPreparedRecord) *boardRecordPreparation {
	missing := make([]viewstore.BoardRecord, 0, len(records))
	for _, row := range records {
		prior, exists := prepared[row.WorkItemID]
		if !exists || !reflect.DeepEqual(prior.record, row) {
			missing = append(missing, row)
		}
	}
	if len(missing) > 0 {
		for id, record := range s.hydrateBoardRecords(ctx, scope, missing) {
			prepared[id] = record
		}
	}
	var changes boardChangePreparation
	supported := false
	if currentChange {
		valid := make([]tracker.NativeBoardCard, 0, len(records))
		for _, row := range records {
			if value := prepared[row.WorkItemID]; value.err == nil {
				valid = append(valid, value.card)
			}
		}
		changes, supported = s.prepareBoardChangeRows(ctx, scope, valid)
	}
	return prepareBoardRecordPage(ctx, records, func(ctx context.Context, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
		value := prepared[row.WorkItemID]
		card := value.card
		if value.err != nil {
			return card, value.err
		}
		if currentChange && card.Change != nil {
			var summary tracker.ChangeSummary
			var err error
			if supported {
				summary, err = changes.summary(ctx, s, scope, row.WorkItemID, card.Change.ID, now)
			} else {
				summary, err = s.selectedCurrentChangeSummary(ctx, scope, row.WorkItemID, card.Change.ID, now)
			}
			if err != nil {
				return card, err
			}
			change := *card.Change
			change.Status = summary.Status
			card.Change = &change
		}
		return card, nil
	})
}
