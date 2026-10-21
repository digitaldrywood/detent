package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func (o *Orchestrator) modelPermitAcquirer(issueID string) runpkg.ModelPermitAcquirer {
	if o == nil || o.modelPermitRequests == nil {
		return nil
	}
	return func(ctx context.Context) error {
		if ctx == nil {
			ctx = context.Background()
		}
		request := modelPermitRequest{
			issueID: strings.TrimSpace(issueID),
			reply:   make(chan error, 1),
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.done:
			return ErrStopped
		case o.modelPermitRequests <- request:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.done:
			return ErrStopped
		case err := <-request.reply:
			return err
		}
	}
}

func (o *Orchestrator) handleModelPermitRequest(state *State, issueID string) error {
	issueID = strings.TrimSpace(issueID)
	if state == nil {
		return fmt.Errorf("acquire model permit for %s: orchestrator state unavailable", issueID)
	}
	running, ok := state.Running[issueID]
	if !ok {
		return fmt.Errorf("acquire model permit for %s: run unavailable", issueID)
	}
	if !running.ModelPermitExempt {
		return nil
	}
	planner := o.dispatchPlanner()
	planner.now = time.Now()
	if o.now != nil {
		planner.now = o.now()
	}
	if planner.providerModelPermitSlots(state) == 0 {
		return runpkg.ErrModelPermitUnavailable
	}
	running.ModelPermitExempt = false
	state.Running[issueID] = running
	return nil
}
