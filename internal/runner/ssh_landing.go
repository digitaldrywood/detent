package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/workspace"
)

type BatchLandingExecution interface {
	BatchLandingEnabled() bool
	LandBatch(context.Context, workspace.BatchLander, workspace.LandRequest) (workspace.LandResult, error)
	FinishLandingBatch(context.Context) error
}

type sshLandingOutcome struct {
	Result  workspace.LandResult
	Failure *sshError
}

func newSSHLandingOutcome(result workspace.LandResult, err error) sshLandingOutcome {
	return sshLandingOutcome{Result: result, Failure: encodeSSHError(err)}
}

func (wire sshLandingOutcome) outcome() workspace.LandOutcome {
	return workspace.LandOutcome{Result: wire.Result, Err: wire.Failure.err()}
}

type sshBatchLander struct {
	peer      *SSHPeer
	callbacks *SSHCallbacks
}

func (l *sshBatchLander) LandChanges(ctx context.Context, requests []workspace.LandRequest) []workspace.LandOutcome {
	l.callbacks.mu.Lock()
	l.callbacks.batchValidations = make([]func(context.Context) error, len(requests))
	for i, request := range requests {
		l.callbacks.batchValidations[i] = request.Validate
	}
	l.callbacks.mu.Unlock()
	var results []sshLandingOutcome
	err := l.peer.Call(ctx, "source.landing_batch", &results, requests)
	outcomes := make([]workspace.LandOutcome, len(requests))
	for i := range outcomes {
		if err != nil {
			outcomes[i].Err = err
		} else if i >= len(results) {
			outcomes[i].Err = errors.New("SSH landing batch omitted a member")
		} else {
			outcomes[i] = results[i].outcome()
		}
	}
	return outcomes
}

func (c *SSHCallbacks) handleLandingBatch(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	if c.landingBatch == nil {
		return nil, errors.New("SSH landing batch is unavailable")
	}
	switch method {
	case "landing.finish":
		c.landingBatch.Finish()
		return struct{}{}, nil
	case "landing.validate":
		var index int
		if len(args) != 1 || json.Unmarshal(args[0], &index) != nil {
			return nil, errors.New("invalid SSH landing member")
		}
		c.mu.Lock()
		if index < 0 || index >= len(c.batchValidations) {
			c.mu.Unlock()
			return nil, errors.New("unknown SSH landing member")
		}
		validate := c.batchValidations[index]
		c.mu.Unlock()
		if validate == nil {
			return nil, errors.New("SSH landing member has no authority check")
		}
		return nil, validate(ctx)
	case "landing.submit":
		var request workspace.LandRequest
		if len(args) != 1 || json.Unmarshal(args[0], &request) != nil {
			return nil, errors.New("invalid SSH landing request")
		}
		landing, ok := c.execution.(LandingExecution)
		if !ok {
			return nil, errors.New("SSH execution cannot land a version")
		}
		target, err := landing.LandingTarget(ctx)
		if err != nil {
			return nil, err
		}
		request.Validate = func(ctx context.Context) error {
			if err := c.execution.Validate(ctx); err != nil {
				return err
			}
			current, err := landing.LandingTarget(ctx)
			if err != nil {
				return err
			}
			if current.ChangeID != target.ChangeID || current.VersionID != target.VersionID || current.HeadSHA != request.Options.HeadSHA || current.Repository != request.Options.Repository || current.Method != request.Options.Method || current.GitHubPullRequest || !request.Options.Native {
				return errors.New("SSH batch differs from the reviewed native version")
			}
			return nil
		}
		if err := request.Validate(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		lander := c.batchLander
		c.mu.Unlock()
		if lander == nil {
			return nil, errors.New("SSH batch source is unavailable")
		}
		result, err := c.landingBatch.Land(lander, request)
		return newSSHLandingOutcome(result, err), nil
	}
	return nil, errors.New("unsupported SSH landing callback")
}

func (e *sshNativeExecution) BatchLandingEnabled() bool { return e.batchedLanding }

func (e *sshNativeExecution) FinishLandingBatch(ctx context.Context) error {
	return e.peer.Call(ctx, "landing.finish", nil)
}

func (e *sshNativeExecution) LandBatch(ctx context.Context, lander workspace.BatchLander, request workspace.LandRequest) (workspace.LandResult, error) {
	e.sources.mu.Lock()
	e.sources.landingBatch = func(ctx context.Context, requests []workspace.LandRequest) []sshLandingOutcome {
		for i := range requests {
			requests[i].Validate = func(ctx context.Context) error { return e.peer.Call(ctx, "landing.validate", nil, i) }
		}
		outcomes := lander.LandChanges(ctx, requests)
		results := make([]sshLandingOutcome, len(outcomes))
		for i, outcome := range outcomes {
			results[i] = newSSHLandingOutcome(outcome.Result, outcome.Err)
		}
		return results
	}
	e.sources.mu.Unlock()
	var wire sshLandingOutcome
	if err := e.peer.Call(ctx, "landing.submit", &wire, request); err != nil {
		return workspace.LandResult{}, err
	}
	outcome := wire.outcome()
	return outcome.Result, outcome.Err
}
