package scheduleowner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
)

const (
	leaseSchema    = 1
	maxCASAttempts = 16
)

var (
	ErrLeaseLost        = errors.New("schedule ownership lease lost")
	ErrInvalidLease     = errors.New("schedule ownership lease is invalid")
	ErrMissingStore     = errors.New("schedule ownership store is required")
	ErrMissingOwner     = errors.New("schedule ownership owner is required")
	ErrCASLimitExceeded = errors.New("schedule ownership compare-and-swap retry limit exceeded")
)

type Dependencies struct {
	Now    func() time.Time
	Wait   func(context.Context, time.Duration) error
	Token  func() (string, error)
	Closer io.Closer
	Logger *slog.Logger
}

type Lease struct {
	Owner      string
	Token      string
	Generation uint64
	RenewedAt  time.Time
	ExpiresAt  time.Time
}

type Status struct {
	Owner      string
	Generation uint64
	RenewedAt  time.Time
	ExpiresAt  time.Time
	Active     bool
}

type Manager struct {
	config Config
	owner  string
	path   string
	store  coordination.Store
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
	token  func() (string, error)
	closer io.Closer
	logger *slog.Logger
}

type leaseState struct {
	Schema     int    `json:"schema"`
	Owner      string `json:"owner,omitempty"`
	Token      string `json:"token,omitempty"`
	Generation uint64 `json:"generation"`
}

func New(config Config, owner string, store coordination.Store, deps Dependencies) (*Manager, error) {
	if store == nil {
		return nil, ErrMissingStore
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, ErrMissingOwner
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
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		config: config,
		owner:  owner,
		path:   leasePath(config.Key),
		store:  store,
		now:    now,
		wait:   wait,
		token:  token,
		closer: deps.Closer,
		logger: logger,
	}, nil
}

func (m *Manager) Current(ctx context.Context) (Status, error) {
	record, found, err := m.store.Get(ctx, m.path)
	if err != nil {
		return Status{}, err
	}
	if !found {
		return Status{}, nil
	}
	state, err := decodeLeaseState(record.Value)
	if err != nil {
		return Status{}, err
	}
	expiresAt := record.ModifiedAt.Add(m.config.LeaseTTL() + m.config.MaxClockSkew())
	return Status{
		Owner:      state.Owner,
		Generation: state.Generation,
		RenewedAt:  record.ModifiedAt,
		ExpiresAt:  expiresAt,
		Active:     state.Owner != "" && m.now().Before(expiresAt),
	}, nil
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	if m.closer == nil {
		return nil
	}
	return m.closer.Close()
}

func (m *Manager) Acquire(ctx context.Context) (Lease, bool, error) {
	token, err := m.token()
	if err != nil {
		return Lease{}, false, fmt.Errorf("create schedule ownership token: %w", err)
	}
	for range maxCASAttempts {
		record, found, err := m.store.Get(ctx, m.path)
		if err != nil {
			return Lease{}, false, err
		}
		state := leaseState{Schema: leaseSchema}
		if found {
			state, err = decodeLeaseState(record.Value)
			if err != nil {
				return Lease{}, false, err
			}
			if state.Owner != "" && m.now().Before(record.ModifiedAt.Add(m.config.LeaseTTL()+m.config.MaxClockSkew())) {
				return Lease{}, false, nil
			}
		}
		state.Owner = m.owner
		state.Token = token
		state.Generation++
		value, err := json.Marshal(state)
		if err != nil {
			return Lease{}, false, err
		}
		updated, swapped, err := m.store.CompareAndSwap(ctx, m.path, record.Version, value)
		if err != nil {
			return Lease{}, false, err
		}
		if !swapped {
			continue
		}
		return m.leaseFromRecord(state, updated), true, nil
	}
	return Lease{}, false, ErrCASLimitExceeded
}

func (m *Manager) Release(ctx context.Context, lease Lease) error {
	for range maxCASAttempts {
		record, found, err := m.store.Get(ctx, m.path)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		state, err := decodeLeaseState(record.Value)
		if err != nil {
			return err
		}
		if !leaseMatches(state, lease) {
			return nil
		}
		state.Owner = ""
		state.Token = ""
		value, err := json.Marshal(state)
		if err != nil {
			return err
		}
		_, swapped, err := m.store.CompareAndSwap(ctx, m.path, record.Version, value)
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
	}
	return ErrCASLimitExceeded
}

func (m *Manager) Run(ctx context.Context, work func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if work == nil {
		return nil
	}
	for ctx.Err() == nil {
		lease, acquired, err := m.Acquire(ctx)
		if err != nil {
			m.logger.WarnContext(ctx, "schedule ownership acquisition failed", "owner", m.owner, "key", m.config.Key, "error", err)
			if waitErr := m.wait(ctx, m.config.RetryInterval()); waitErr != nil {
				return nil
			}
			continue
		}
		if !acquired {
			if waitErr := m.wait(ctx, m.config.RetryInterval()); waitErr != nil {
				return nil
			}
			continue
		}
		m.logger.InfoContext(ctx, "schedule ownership acquired", "owner", lease.Owner, "key", m.config.Key, "generation", lease.Generation)
		err = m.hold(ctx, lease, work)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrLeaseLost) {
			m.logger.WarnContext(ctx, "schedule ownership lost", "owner", lease.Owner, "key", m.config.Key, "generation", lease.Generation)
			if waitErr := m.wait(ctx, m.config.RetryInterval()); waitErr != nil {
				return nil
			}
			continue
		}
		return err
	}
	return nil
}

func (m *Manager) hold(ctx context.Context, lease Lease, work func(context.Context) error) error {
	ownedCtx, cancel := context.WithCancel(ctx)
	workDone := make(chan error, 1)
	go func() {
		workDone <- work(ownedCtx)
	}()
	validUntil := m.now().Add(m.config.LeaseTTL() - m.config.MaxClockSkew())
	timer := time.NewTimer(nextRenewalDelay(m.now(), validUntil, m.config.HeartbeatInterval()))
	defer timer.Stop()

	finish := func(result error, release bool) error {
		cancel()
		workErr := <-workDone
		if release {
			releaseTimeout := min(m.config.HeartbeatInterval(), 30*time.Second)
			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			releaseErr := m.Release(releaseCtx, lease)
			releaseCancel()
			if releaseErr != nil {
				m.logger.Warn("schedule ownership release failed", "owner", lease.Owner, "key", m.config.Key, "generation", lease.Generation, "error", releaseErr)
			}
		}
		if result != nil {
			return result
		}
		if workErr != nil && !errors.Is(workErr, context.Canceled) {
			return workErr
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return finish(nil, true)
		case workErr := <-workDone:
			cancel()
			releaseTimeout := min(m.config.HeartbeatInterval(), 30*time.Second)
			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			releaseErr := m.Release(releaseCtx, lease)
			releaseCancel()
			return errors.Join(workErr, releaseErr)
		case <-timer.C:
			if !m.now().Before(validUntil) {
				return finish(ErrLeaseLost, false)
			}
			renewCtx, renewCancel := context.WithDeadline(ctx, validUntil)
			renewed, err := m.renew(renewCtx, lease)
			renewCancel()
			if err == nil {
				lease = renewed
				validUntil = m.now().Add(m.config.LeaseTTL() - m.config.MaxClockSkew())
				timer.Reset(nextRenewalDelay(m.now(), validUntil, m.config.HeartbeatInterval()))
				continue
			}
			if errors.Is(err, ErrLeaseLost) || !m.now().Before(validUntil) {
				return finish(ErrLeaseLost, false)
			}
			m.logger.WarnContext(ctx, "schedule ownership renewal failed", "owner", lease.Owner, "key", m.config.Key, "generation", lease.Generation, "error", err)
			timer.Reset(nextRenewalDelay(m.now(), validUntil, m.config.RetryInterval()))
		}
	}
}

func (m *Manager) renew(ctx context.Context, lease Lease) (Lease, error) {
	for range maxCASAttempts {
		record, found, err := m.store.Get(ctx, m.path)
		if err != nil {
			return Lease{}, err
		}
		if !found {
			return Lease{}, ErrLeaseLost
		}
		state, err := decodeLeaseState(record.Value)
		if err != nil {
			return Lease{}, err
		}
		if !leaseMatches(state, lease) {
			return Lease{}, ErrLeaseLost
		}
		value, err := json.Marshal(state)
		if err != nil {
			return Lease{}, err
		}
		updated, swapped, err := m.store.CompareAndSwap(ctx, m.path, record.Version, value)
		if err != nil {
			return Lease{}, err
		}
		if swapped {
			return m.leaseFromRecord(state, updated), nil
		}
	}
	return Lease{}, ErrCASLimitExceeded
}

func (m *Manager) leaseFromRecord(state leaseState, record coordination.Record) Lease {
	return Lease{
		Owner:      state.Owner,
		Token:      state.Token,
		Generation: state.Generation,
		RenewedAt:  record.ModifiedAt,
		ExpiresAt:  record.ModifiedAt.Add(m.config.LeaseTTL() + m.config.MaxClockSkew()),
	}
}

func decodeLeaseState(value []byte) (leaseState, error) {
	var state leaseState
	if err := json.Unmarshal(value, &state); err != nil {
		return leaseState{}, fmt.Errorf("%w: %w", ErrInvalidLease, err)
	}
	if state.Schema != leaseSchema || (state.Owner == "") != (state.Token == "") {
		return leaseState{}, ErrInvalidLease
	}
	return state, nil
}

func leaseMatches(state leaseState, lease Lease) bool {
	return state.Owner == lease.Owner && state.Token == lease.Token && state.Generation == lease.Generation
}

func leasePath(key string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return ".detent/schedules/" + hex.EncodeToString(digest[:]) + "/lease.json"
}

func effectPath(key string, marker string) string {
	projectDigest := sha256.Sum256([]byte(strings.TrimSpace(key)))
	effectDigest := sha256.Sum256([]byte(strings.TrimSpace(marker)))
	return ".detent/schedules/" + hex.EncodeToString(projectDigest[:]) + "/effects/" + hex.EncodeToString(effectDigest[:]) + ".json"
}

func nextRenewalDelay(now time.Time, validUntil time.Time, interval time.Duration) time.Duration {
	remaining := validUntil.Sub(now)
	if remaining <= 0 {
		return time.Nanosecond
	}
	if interval <= 0 || interval > remaining {
		return remaining
	}
	return interval
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
