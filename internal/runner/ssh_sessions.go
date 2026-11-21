package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// SSHCallbacks shares the existing session lifecycle with the central store.
// If the channel ends before FinishSession arrives, the owner closes precisely
// the sessions it opened on that channel using usage already received.
type SSHCallbacks struct {
	local                 *Runner
	localStartNamespace   string
	localSessionMu        sync.Mutex
	issue                 connector.Issue
	publicationMu         sync.Mutex
	publicationCallbackMu sync.Mutex
	publicationSequence   uint64
	publicationAuthorize  func(uint64, string, workspace.GitHubPublication) error
	publicationEffect     func(uint64, string, workspace.GitHubPublication, string, string, workspace.GitHubPublication) error
	handle                func(context.Context, string, []json.RawMessage) (any, error)
	store                 SessionStore
	execution             Execution
	mu                    sync.Mutex
	wg                    sync.WaitGroup
	closed                bool
	active                map[int64]callbackSession
	uncertainLocalStarts  map[string]store.SessionStart
}

type callbackSession struct {
	UsageUpdate
	combined bool
}

func (r *Runner) LocalRunCallbacks(request RunRequest) *SSHCallbacks {
	callbacks := r.SSHRunCallbacks(request)
	callbacks.local, callbacks.issue = r, request.Issue
	callbacks.localStartNamespace = uuid.NewString()
	return callbacks
}

func (r *Runner) SSHRunCallbacks(request RunRequest) *SSHCallbacks {
	return &SSHCallbacks{handle: r.SSHCallbackHandler(request), store: r.store, execution: request.Execution, active: make(map[int64]callbackSession)}
}

func (c *SSHCallbacks) Handle(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	ctx = bindSessionExecution(ctx, c.store, c.execution)
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
	switch method {
	case "local.start", "local.finish":
		c.localSessionMu.Lock()
		defer c.localSessionMu.Unlock()
		result, err = c.handleLocalProcess(ctx, method, args)
	case "local.processes", "local.register", "local.reaped":
		result, err = c.handleLocalProcess(ctx, method, args)
	case "publication.authorize", "publication.effect":
		c.publicationCallbackMu.Lock()
		c.mu.Lock()
		authorize, effect := c.publicationAuthorize, c.publicationEffect
		if c.closed {
			err = errors.New("SSH publication channel is closed")
		}
		c.mu.Unlock()
		if err == nil {
			if method == "publication.authorize" {
				result, err = invokeSSHMethod(ctx, authorize, "", args)
			} else {
				result, err = invokeSSHMethod(ctx, effect, "", args)
			}
		}
		c.publicationCallbackMu.Unlock()
	default:
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
			c.active[id] = callbackSession{UsageUpdate: UsageUpdate{DetentSessionID: id}}
		}
	case "local.start":
		if start, ok := result.(localSessionStartResult); ok && start.ID > 0 {
			c.active[start.ID] = callbackSession{UsageUpdate: UsageUpdate{DetentSessionID: start.ID}, combined: true}
		}
	case "store.FinishSession", "store.ReconcileSessionFinalization", "local.finish":
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
				if previous, ok := c.active[update.DetentSessionID]; ok && !update.LastEventAt.Before(previous.LastEventAt) {
					previous.UsageUpdate = update
					c.active[update.DetentSessionID] = previous
				}
			}
		}
	}
	return result, nil
}

func (c *SSHCallbacks) Finish(ctx context.Context, at time.Time) error {
	ctx = bindSessionExecution(ctx, c.store, c.execution)
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	cleanupCtx := ctx
	if c.local != nil {
		var cancel context.CancelFunc
		cleanupCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for key, start := range c.uncertainLocalStarts {
			id, err := c.store.StartSession(withWorkerSessionStartKey(cleanupCtx, key), start)
			if id <= 0 {
				if err == nil {
					err = ErrExecutionAuthorityUnavailable
				}
				errs = append(errs, err)
				continue
			}
			c.active[id] = callbackSession{UsageUpdate: UsageUpdate{DetentSessionID: id}, combined: true}
			delete(c.uncertainLocalStarts, key)
		}
	}
	const reason = "execution worker exited"
	for id, update := range c.active {
		tokens := update.Tokens
		finish := store.SessionFinish{CompletedAt: at, FinalState: FinalStateFailed, Turns: int64(update.TurnCount), InputTokens: tokens.InputTokens, CachedInputTokens: tokens.CachedInputTokens, OutputTokens: tokens.OutputTokens, ReasoningOutputTokens: tokens.ReasoningOutputTokens, TotalTokens: tokens.TotalTokens, ProviderThreadID: update.ProviderThreadID, RuntimeIdentity: update.RuntimeIdentity}
		finishCtx := ctx
		if c.local != nil {
			var err error
			if update.combined {
				finishCtx = cleanupCtx
				err = c.local.reapSessionWorkerProcessWithRecorder(cleanupCtx, id, c.issue, reason, nil, func(_ context.Context, reap store.WorkerProcessReap) error {
					finish.Reap = &reap
					return nil
				})
			} else {
				err = c.local.reapSessionWorkerProcess(ctx, id, c.issue, reason)
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := c.store.FinishSession(finishCtx, id, finish); err != nil {
			errs = append(errs, err)
		} else {
			delete(c.active, id)
		}
	}
	return errors.Join(errs...)
}

func (c *SSHCallbacks) RegisterOverlappingWorker(ctx context.Context, request RunRequest, worker store.WorkerProcessRegistration) (func(context.Context) error, error) {
	owner, ok := c.store.(workerRegistrationStore)
	if !ok {
		return nil, ErrExecutionAuthorityUnavailable
	}
	ctx = bindSessionExecution(ctx, c.store, c.execution)
	id, reconcile, err := startWorkerSession(ctx, owner, store.SessionStart{
		ProjectID: request.ProjectID, IssueID: request.Issue.ID, Identifier: request.Issue.Identifier,
		IssueURL: request.Issue.URL, WorkAttemptID: request.WorkAttemptID, StartedAt: worker.StartedAt,
		AgentBackendKind: "host", AgentRole: "execution_worker", Worker: &worker,
	})
	if id <= 0 && reconcile == nil {
		return nil, errors.Join(err, ErrExecutionAuthorityUnavailable)
	}
	state := FinalStateCompleted
	if err != nil {
		state = FinalStateFailed
	}
	return func(ctx context.Context) error {
		ctx = bindSessionExecution(ctx, c.store, c.execution)
		if reconcile != nil {
			var err error
			id, err = reconcile(ctx)
			if id <= 0 {
				return err
			}
			reconcile = nil
		}
		now := time.Now().UTC()
		err := owner.FinishSession(ctx, id, store.SessionFinish{
			CompletedAt: now, FinalState: state,
			Reap: &store.WorkerProcessReap{ReapedAt: now, Outcome: "already_exited", Reason: "execution worker exited"},
		})
		if errors.Is(err, ErrExecutionAuthorityUnavailable) {
			return nil
		}
		return err
	}, err
}
