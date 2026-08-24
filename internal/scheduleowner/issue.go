package scheduleowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/intake"
)

const (
	effectSchema   = 1
	effectReserved = "reserved"
	effectCreating = "creating"
	effectComplete = "complete"
)

var (
	ErrMissingIssueBackend  = errors.New("coordinated issue backend is required")
	ErrMissingMarker        = errors.New("coordinated issue marker is required")
	ErrIssueCreateUncertain = errors.New("coordinated issue creation outcome is uncertain")
)

type IssueBackend interface {
	FindIntakeIssue(context.Context, string) (intake.Issue, bool, error)
	CreateIntakeIssue(context.Context, intake.IssueDraft) (intake.Issue, error)
}

type IssueCoordinator struct {
	config Config
	store  coordination.Store
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
	token  func() (string, error)
}

type effectState struct {
	Schema int          `json:"schema"`
	Status string       `json:"status"`
	Token  string       `json:"token"`
	Issue  intake.Issue `json:"issue,omitempty"`
}

func NewIssueCoordinator(config Config, store coordination.Store, deps Dependencies) (*IssueCoordinator, error) {
	if store == nil {
		return nil, ErrMissingStore
	}
	if problems := config.Validate("schedule_ownership"); len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	wait := deps.Wait
	if wait == nil {
		wait = waitContext
	}
	token := deps.Token
	if token == nil {
		token = randomToken
	}
	return &IssueCoordinator{config: config, store: store, now: now, wait: wait, token: token}, nil
}

func (c *IssueCoordinator) Ensure(ctx context.Context, marker string, draft intake.IssueDraft, backend IssueBackend) (intake.Issue, bool, error) {
	if backend == nil {
		return intake.Issue{}, false, ErrMissingIssueBackend
	}
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return intake.Issue{}, false, ErrMissingMarker
	}
	reservationToken, err := c.token()
	if err != nil {
		return intake.Issue{}, false, fmt.Errorf("create issue reservation token: %w", err)
	}
	key := effectPath(c.config.Key, marker)
	for ctx.Err() == nil {
		record, found, err := c.store.Get(ctx, key)
		if err != nil {
			return intake.Issue{}, false, err
		}
		if !found {
			reserved, swapped, err := c.reserve(ctx, key, record.Version, reservationToken)
			if err != nil {
				return intake.Issue{}, false, err
			}
			if !swapped {
				continue
			}
			return c.createReserved(ctx, key, marker, draft, backend, reserved, reservationToken)
		}
		effect, err := decodeEffect(record.Value)
		if err != nil {
			return intake.Issue{}, false, err
		}
		switch effect.Status {
		case effectComplete:
			return effect.Issue, false, nil
		case effectReserved:
			if c.now().Before(record.ModifiedAt.Add(c.config.LeaseTTL() + c.config.MaxClockSkew())) {
				if err := c.wait(ctx, c.config.RetryInterval()); err != nil {
					return intake.Issue{}, false, err
				}
				continue
			}
			reserved, swapped, err := c.reserve(ctx, key, record.Version, reservationToken)
			if err != nil {
				return intake.Issue{}, false, err
			}
			if !swapped {
				continue
			}
			return c.createReserved(ctx, key, marker, draft, backend, reserved, reservationToken)
		case effectCreating:
			issue, exists, findErr := backend.FindIntakeIssue(ctx, marker)
			if findErr != nil {
				return intake.Issue{}, false, fmt.Errorf("reconcile coordinated issue: %w", findErr)
			}
			if exists {
				completed, completeErr := c.completeDurably(ctx, key, effect.Token, issue)
				return completed, false, completeErr
			}
			if c.now().Before(record.ModifiedAt.Add(c.config.LeaseTTL() + c.config.MaxClockSkew())) {
				if err := c.wait(ctx, c.config.RetryInterval()); err != nil {
					return intake.Issue{}, false, err
				}
				continue
			}
			return intake.Issue{}, false, ErrIssueCreateUncertain
		default:
			return intake.Issue{}, false, ErrIssueCreateUncertain
		}
	}
	return intake.Issue{}, false, ctx.Err()
}

func (c *IssueCoordinator) reserve(ctx context.Context, key string, version string, token string) (coordination.Record, bool, error) {
	value, err := json.Marshal(effectState{Schema: effectSchema, Status: effectReserved, Token: token})
	if err != nil {
		return coordination.Record{}, false, err
	}
	return c.store.CompareAndSwap(ctx, key, version, value)
}

func (c *IssueCoordinator) createReserved(
	ctx context.Context,
	key string,
	marker string,
	draft intake.IssueDraft,
	backend IssueBackend,
	record coordination.Record,
	token string,
) (intake.Issue, bool, error) {
	creatingValue, err := json.Marshal(effectState{Schema: effectSchema, Status: effectCreating, Token: token})
	if err != nil {
		return intake.Issue{}, false, err
	}
	_, swapped, err := c.store.CompareAndSwap(ctx, key, record.Version, creatingValue)
	if err != nil {
		return intake.Issue{}, false, err
	}
	if !swapped {
		return c.Ensure(ctx, marker, draft, backend)
	}
	issue, found, err := backend.FindIntakeIssue(ctx, marker)
	if err != nil {
		return intake.Issue{}, false, fmt.Errorf("reconcile coordinated issue before create: %w", err)
	}
	if found {
		completed, completeErr := c.completeDurably(ctx, key, token, issue)
		return completed, false, completeErr
	}
	issue, createErr := backend.CreateIntakeIssue(ctx, draft)
	if createErr != nil {
		reconcileCtx, reconcileCancel := context.WithTimeout(context.WithoutCancel(ctx), min(c.config.RetryInterval(), 30*time.Second))
		reconciled, exists, findErr := backend.FindIntakeIssue(reconcileCtx, marker)
		reconcileCancel()
		if findErr == nil && exists {
			completed, completeErr := c.completeDurably(ctx, key, token, reconciled)
			return completed, false, completeErr
		}
		return intake.Issue{}, false, errors.Join(ErrIssueCreateUncertain, createErr, findErr)
	}
	completed, err := c.completeDurably(ctx, key, token, issue)
	return completed, true, err
}

func (c *IssueCoordinator) completeDurably(ctx context.Context, key string, token string, issue intake.Issue) (intake.Issue, error) {
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(c.config.HeartbeatInterval(), 30*time.Second))
	defer cancel()
	return c.complete(completeCtx, key, token, issue)
}

func (c *IssueCoordinator) complete(ctx context.Context, key string, token string, issue intake.Issue) (intake.Issue, error) {
	for range maxCASAttempts {
		record, found, err := c.store.Get(ctx, key)
		if err != nil {
			return intake.Issue{}, err
		}
		if !found {
			return intake.Issue{}, ErrIssueCreateUncertain
		}
		effect, err := decodeEffect(record.Value)
		if err != nil {
			return intake.Issue{}, err
		}
		if effect.Status == effectComplete {
			return effect.Issue, nil
		}
		if effect.Status != effectCreating || effect.Token != token {
			return intake.Issue{}, ErrIssueCreateUncertain
		}
		completed, swapped, err := c.completeFromRecord(ctx, key, record, token, issue)
		if err != nil {
			return intake.Issue{}, err
		}
		if swapped {
			return completed, nil
		}
	}
	return intake.Issue{}, ErrCASLimitExceeded
}

func (c *IssueCoordinator) completeFromRecord(
	ctx context.Context,
	key string,
	record coordination.Record,
	token string,
	issue intake.Issue,
) (intake.Issue, bool, error) {
	value, err := json.Marshal(effectState{Schema: effectSchema, Status: effectComplete, Token: token, Issue: issue})
	if err != nil {
		return intake.Issue{}, false, err
	}
	_, swapped, err := c.store.CompareAndSwap(ctx, key, record.Version, value)
	return issue, swapped, err
}

func decodeEffect(value []byte) (effectState, error) {
	var effect effectState
	if err := json.Unmarshal(value, &effect); err != nil {
		return effectState{}, fmt.Errorf("decode coordinated issue effect: %w", err)
	}
	if effect.Schema != effectSchema || effect.Token == "" {
		return effectState{}, ErrIssueCreateUncertain
	}
	if effect.Status != effectReserved && effect.Status != effectCreating && effect.Status != effectComplete {
		return effectState{}, ErrIssueCreateUncertain
	}
	if effect.Status == effectComplete && strings.TrimSpace(effect.Issue.ID) == "" {
		return effectState{}, ErrIssueCreateUncertain
	}
	return effect, nil
}
