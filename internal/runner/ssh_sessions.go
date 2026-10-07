package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// SSHCallbacks shares the existing session lifecycle with the central store.
// If the channel ends before FinishSession arrives, the owner closes precisely
// the sessions it opened on that channel using usage already received.
type SSHCallbacks struct {
	landingBatch     *workspace.LandingBatchTicket
	batchLander      *sshBatchLander
	batchValidations []func(context.Context) error
	handle           func(context.Context, string, []json.RawMessage) (any, error)
	store            SessionStore
	execution        Execution
	mu               sync.Mutex
	wg               sync.WaitGroup
	closed           bool
	active           map[int64]UsageUpdate
}

func (r *Runner) SSHRunCallbacks(request RunRequest) *SSHCallbacks {
	return &SSHCallbacks{landingBatch: request.LandingBatch, handle: r.SSHCallbackHandler(request), store: r.store, execution: request.Execution, active: make(map[int64]UsageUpdate)}
}

func (c *SSHCallbacks) Handle(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("SSH callback channel is closed")
	}
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()
	var result any
	var err error
	if method == "landing.submit" || method == "landing.finish" || method == "landing.validate" {
		result, err = c.handleLandingBatch(ctx, method, args)
	} else {
		result, err = c.handle(ctx, method, args)
	}
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "store.StartSession":
		if id, ok := result.(int64); ok && id > 0 {
			c.active[id] = UsageUpdate{DetentSessionID: id}
		}
	case "store.FinishSession":
		if len(args) > 0 {
			var id int64
			if json.Unmarshal(args[0], &id) == nil {
				delete(c.active, id)
			}
		}
	case "usage":
		if len(args) > 0 {
			var update UsageUpdate
			if json.Unmarshal(args[0], &update) == nil {
				if _, ok := c.active[update.DetentSessionID]; ok {
					c.active[update.DetentSessionID] = update
				}
			}
		}
	}
	return result, nil
}

func (c *SSHCallbacks) Finish(ctx context.Context, at time.Time) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for id, update := range c.active {
		tokens := update.Tokens
		if err := c.store.FinishSession(ctx, id, store.SessionFinish{CompletedAt: at, FinalState: FinalStateFailed, Turns: int64(update.TurnCount), InputTokens: tokens.InputTokens, CachedInputTokens: tokens.CachedInputTokens, OutputTokens: tokens.OutputTokens, ReasoningOutputTokens: tokens.ReasoningOutputTokens, TotalTokens: tokens.TotalTokens, ProviderThreadID: update.SessionID, RuntimeIdentity: update.RuntimeIdentity}); err != nil {
			errs = append(errs, err)
		} else {
			delete(c.active, id)
		}
	}
	return errors.Join(errs...)
}
