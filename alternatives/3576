package workspace

import (
	"context"
	"errors"
	"sync"
)

type LandingBatch struct {
	mu      sync.Mutex
	run     func()
	sealed  bool
	started bool
	tickets []*LandingBatchTicket
	done    chan struct{}
}

type LandingBatchTicket struct {
	batch   *LandingBatch
	arrived bool
	request *LandRequest
	lander  BatchLander
	outcome LandOutcome
}

func NewLandingBatch(ctx context.Context) *LandingBatch {
	b := &LandingBatch{done: make(chan struct{})}
	b.run = func() { b.land(ctx) }
	return b
}

func (b *LandingBatch) Add() *LandingBatchTicket {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sealed {
		return nil
	}
	ticket := &LandingBatchTicket{batch: b}
	b.tickets = append(b.tickets, ticket)
	return ticket
}

func (b *LandingBatch) Seal() {
	b.mu.Lock()
	b.sealed = true
	b.start()
	b.mu.Unlock()
}

func (b *LandingBatch) start() {
	if !b.sealed || b.started {
		return
	}
	for _, ticket := range b.tickets {
		if !ticket.arrived {
			return
		}
	}
	b.started = true
	go b.run()
}

func (b *LandingBatch) land(ctx context.Context) {
	defer close(b.done)
	requests := []LandRequest{}
	members := []*LandingBatchTicket{}
	var lander BatchLander
	for _, ticket := range b.tickets {
		if ticket.request == nil {
			continue
		}
		if lander == nil {
			lander = ticket.lander
		}
		requests = append(requests, *ticket.request)
		members = append(members, ticket)
	}
	if lander == nil {
		for _, member := range members {
			member.outcome.Err = errors.New("landing batch has no workspace backend")
		}
		return
	}
	outcomes := lander.LandChanges(ctx, requests)
	for i, member := range members {
		member.outcome = outcomes[i]
	}
}

func (t *LandingBatchTicket) Land(lander BatchLander, request LandRequest) (LandResult, error) {
	b := t.batch
	b.mu.Lock()
	if t.arrived {
		b.mu.Unlock()
		return LandResult{}, errors.New("landing batch ticket already submitted")
	}
	t.lander, t.request, t.arrived = lander, &request, true
	b.start()
	b.mu.Unlock()
	<-b.done
	return t.outcome.Result, t.outcome.Err
}

func (t *LandingBatchTicket) Finish() {
	if t == nil {
		return
	}
	b := t.batch
	b.mu.Lock()
	if !t.arrived {
		t.arrived = true
		b.start()
	}
	b.mu.Unlock()
	<-b.done
}
