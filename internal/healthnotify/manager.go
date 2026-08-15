package healthnotify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/notify"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	SchemaVersion           = 1
	EventName               = "detent.health.transition"
	DefaultDebounce         = 5 * time.Minute
	defaultMaxAttempts      = 5
	defaultRetryBase        = 30 * time.Second
	defaultRetryMax         = 15 * time.Minute
	maxDeliveriesPerCycle   = 10
	healthNotifierUserAgent = "detent-health-notifier"
)

type Config struct {
	Webhook     notify.WebhookConfig
	Instance    string
	Host        string
	Debounce    time.Duration
	MaxAttempts int
	RetryBase   time.Duration
	RetryMax    time.Duration
}

type Dependencies struct {
	Store  store.HealthNotificationStateStore
	Sender notify.Sender
	Logger *slog.Logger
}

type Event struct {
	Schema                  int                        `json:"schema"`
	Event                   string                     `json:"event"`
	ID                      string                     `json:"id"`
	Identity                string                     `json:"identity"`
	Transition              string                     `json:"transition"`
	Instance                string                     `json:"instance"`
	Host                    string                     `json:"host"`
	Scope                   string                     `json:"scope"`
	ProjectID               string                     `json:"project_id,omitempty"`
	State                   string                     `json:"state"`
	Causes                  []string                   `json:"causes,omitempty"`
	WaitReasons             []string                   `json:"wait_reasons,omitempty"`
	FailureBreakers         []telemetry.FailureBreaker `json:"failure_breakers,omitempty"`
	BackendOutages          []telemetry.BackendOutage  `json:"backend_outages,omitempty"`
	EnteredAt               time.Time                  `json:"entered_at"`
	NeedsAttentionEnteredAt time.Time                  `json:"needs_attention_entered_at"`
	ObservedAt              time.Time                  `json:"observed_at"`
}

type Failure struct {
	EventID       string     `json:"event_id"`
	Identity      string     `json:"identity"`
	Scope         string     `json:"scope"`
	ProjectID     string     `json:"project_id,omitempty"`
	Transition    string     `json:"transition"`
	Attempts      int        `json:"attempts"`
	MaxAttempts   int        `json:"max_attempts"`
	LastError     string     `json:"last_error"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	FailedAt      *time.Time `json:"failed_at,omitempty"`
}

type FailureReader interface {
	Failures(context.Context) ([]Failure, error)
}

type Manager struct {
	cfg    Config
	store  store.HealthNotificationStateStore
	sender notify.Sender
	logger *slog.Logger
}

type durableState struct {
	Schema                  int                        `json:"schema"`
	Identity                string                     `json:"identity"`
	Scope                   string                     `json:"scope"`
	ProjectID               string                     `json:"project_id,omitempty"`
	StableActive            bool                       `json:"stable_active"`
	Pending                 *pendingState              `json:"pending,omitempty"`
	NeedsAttentionEnteredAt time.Time                  `json:"needs_attention_entered_at,omitzero"`
	Causes                  []string                   `json:"causes,omitempty"`
	WaitReasons             []string                   `json:"wait_reasons,omitempty"`
	FailureBreakers         []telemetry.FailureBreaker `json:"failure_breakers,omitempty"`
	BackendOutages          []telemetry.BackendOutage  `json:"backend_outages,omitempty"`
	Deliveries              []deliveryState            `json:"deliveries,omitempty"`
}

type pendingState struct {
	Active          bool                       `json:"active"`
	State           string                     `json:"state"`
	Causes          []string                   `json:"causes,omitempty"`
	WaitReasons     []string                   `json:"wait_reasons,omitempty"`
	FailureBreakers []telemetry.FailureBreaker `json:"failure_breakers,omitempty"`
	BackendOutages  []telemetry.BackendOutage  `json:"backend_outages,omitempty"`
	Since           time.Time                  `json:"since"`
}

type deliveryState struct {
	Event         Event      `json:"event"`
	Attempts      int        `json:"attempts"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	FailedAt      *time.Time `json:"failed_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

func NewManager(cfg Config, deps Dependencies) (*Manager, error) {
	if strings.TrimSpace(cfg.Webhook.URL) == "" {
		return &Manager{}, nil
	}
	if deps.Store == nil {
		return nil, errors.New("health notification state store is required")
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = defaultRetryBase
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = defaultRetryMax
	}
	sender := deps.Sender
	if sender == nil {
		cfg.Webhook.UserAgent = healthNotifierUserAgent
		webhook, err := notify.NewWebhook(cfg.Webhook)
		if err != nil {
			return nil, fmt.Errorf("configure health notification webhook: %w", err)
		}
		sender = webhook
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{cfg: cfg, store: deps.Store, sender: sender, logger: logger}, nil
}

func (m *Manager) Enabled() bool {
	return m != nil && m.sender != nil && m.store != nil
}

func (m *Manager) Run(ctx context.Context, snapshots *hub.Hub[telemetry.Snapshot], healthSource func() []project.Health) {
	if !m.Enabled() || snapshots == nil {
		return
	}
	subscription, err := snapshots.Subscribe(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Error("subscribe health notifications failed", "error", err)
		}
		return
	}
	defer subscription.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-subscription.C():
			if !ok {
				return
			}
			if snapshot.LastKnown || snapshot.Seq == 0 {
				continue
			}
			health := []project.Health(nil)
			if healthSource != nil {
				health = healthSource()
			}
			now := snapshot.GeneratedAt.UTC()
			if now.IsZero() {
				now = time.Now().UTC()
			}
			if err := m.Reconcile(ctx, snapshot, health, now); err != nil && ctx.Err() == nil {
				m.logger.Error("reconcile health notifications failed", "error", err)
			}
		}
	}
}

func (m *Manager) Reconcile(ctx context.Context, snapshot telemetry.Snapshot, health []project.Health, now time.Time) error {
	if !m.Enabled() {
		return nil
	}
	now = now.UTC()
	states, err := m.loadStates(ctx)
	if err != nil {
		return err
	}
	observed := map[string]struct{}{}
	changed := map[string]*durableState{}
	for _, current := range observations(snapshot, health) {
		observed[current.Identity] = struct{}{}
		state := states[current.Identity]
		if state == nil {
			state = &durableState{
				Schema:    SchemaVersion,
				Identity:  current.Identity,
				Scope:     current.Scope,
				ProjectID: current.ProjectID,
			}
			states[current.Identity] = state
			changed[current.Identity] = state
		}
		if m.applyObservation(state, current, now) {
			changed[current.Identity] = state
		}
	}
	for identity, state := range states {
		if _, ok := observed[identity]; ok || (!state.StableActive && (state.Pending == nil || !state.Pending.Active)) {
			continue
		}
		if m.applyObservation(state, observation{
			Identity:  identity,
			Scope:     state.Scope,
			ProjectID: state.ProjectID,
			State:     unknownProjectRecoveryStatus,
		}, now) {
			changed[identity] = state
		}
	}
	if err := m.saveStates(ctx, changed, now); err != nil {
		return err
	}
	return m.deliverDue(ctx, states, now)
}

func (m *Manager) Failures(ctx context.Context) ([]Failure, error) {
	if !m.Enabled() {
		return nil, nil
	}
	states, err := m.loadStates(ctx)
	if err != nil {
		return nil, err
	}
	failures := []Failure{}
	for _, state := range states {
		for _, delivery := range state.Deliveries {
			if delivery.DeliveredAt != nil || strings.TrimSpace(delivery.LastError) == "" {
				continue
			}
			failures = append(failures, Failure{
				EventID:       delivery.Event.ID,
				Identity:      delivery.Event.Identity,
				Scope:         delivery.Event.Scope,
				ProjectID:     delivery.Event.ProjectID,
				Transition:    delivery.Event.Transition,
				Attempts:      delivery.Attempts,
				MaxAttempts:   m.cfg.MaxAttempts,
				LastError:     delivery.LastError,
				NextAttemptAt: cloneTime(delivery.NextAttemptAt),
				FailedAt:      cloneTime(delivery.FailedAt),
			})
		}
	}
	slices.SortFunc(failures, func(left Failure, right Failure) int {
		return strings.Compare(left.EventID, right.EventID)
	})
	return failures, nil
}

func (m *Manager) applyObservation(state *durableState, current observation, now time.Time) bool {
	deliveryCount := len(state.Deliveries)
	state.Deliveries = slices.DeleteFunc(state.Deliveries, func(delivery deliveryState) bool {
		return delivery.DeliveredAt != nil
	})
	changed := len(state.Deliveries) != deliveryCount
	if current.Active == state.StableActive {
		changed = changed || state.Pending != nil
		state.Pending = nil
		return changed
	}
	if state.Pending == nil || state.Pending.Active != current.Active {
		state.Pending = &pendingState{
			Active:          current.Active,
			State:           current.State,
			Causes:          compactSorted(current.Causes),
			WaitReasons:     compactSorted(current.WaitReasons),
			FailureBreakers: append([]telemetry.FailureBreaker(nil), current.FailureBreakers...),
			BackendOutages:  append([]telemetry.BackendOutage(nil), current.BackendOutages...),
			Since:           now,
		}
		return true
	}
	causes := compactSorted(current.Causes)
	waitReasons := compactSorted(current.WaitReasons)
	if state.Pending.State != current.State || !slices.Equal(state.Pending.Causes, causes) || !slices.Equal(state.Pending.WaitReasons, waitReasons) ||
		!reflect.DeepEqual(state.Pending.FailureBreakers, current.FailureBreakers) || !reflect.DeepEqual(state.Pending.BackendOutages, current.BackendOutages) {
		changed = true
		state.Pending.State = current.State
		state.Pending.Causes = causes
		state.Pending.WaitReasons = waitReasons
		state.Pending.FailureBreakers = append([]telemetry.FailureBreaker(nil), current.FailureBreakers...)
		state.Pending.BackendOutages = append([]telemetry.BackendOutage(nil), current.BackendOutages...)
	}
	if now.Before(state.Pending.Since.Add(m.cfg.Debounce)) {
		return changed
	}
	pending := *state.Pending
	state.Pending = nil
	state.StableActive = pending.Active
	if pending.Active {
		state.NeedsAttentionEnteredAt = pending.Since
		state.Causes = append([]string(nil), pending.Causes...)
		state.WaitReasons = append([]string(nil), pending.WaitReasons...)
		state.FailureBreakers = append([]telemetry.FailureBreaker(nil), pending.FailureBreakers...)
		state.BackendOutages = append([]telemetry.BackendOutage(nil), pending.BackendOutages...)
		state.Deliveries = append(state.Deliveries, deliveryState{Event: m.event(state, TransitionEntry, pending.State, pending.Since, pending.Since, now)})
		return true
	}
	state.Deliveries = append(state.Deliveries, deliveryState{Event: m.event(state, TransitionRecovery, pending.State, pending.Since, state.NeedsAttentionEnteredAt, now)})
	state.NeedsAttentionEnteredAt = time.Time{}
	state.Causes = nil
	state.WaitReasons = nil
	state.FailureBreakers = nil
	state.BackendOutages = nil
	return true
}

func (m *Manager) event(state *durableState, transition string, enteredState string, enteredAt time.Time, attentionEnteredAt time.Time, observedAt time.Time) Event {
	return Event{
		Schema:                  SchemaVersion,
		Event:                   EventName,
		ID:                      eventID(state.Identity, transition, attentionEnteredAt),
		Identity:                state.Identity,
		Transition:              transition,
		Instance:                strings.TrimSpace(m.cfg.Instance),
		Host:                    strings.TrimSpace(m.cfg.Host),
		Scope:                   state.Scope,
		ProjectID:               state.ProjectID,
		State:                   strings.TrimSpace(enteredState),
		Causes:                  append([]string(nil), state.Causes...),
		WaitReasons:             append([]string(nil), state.WaitReasons...),
		FailureBreakers:         append([]telemetry.FailureBreaker(nil), state.FailureBreakers...),
		BackendOutages:          append([]telemetry.BackendOutage(nil), state.BackendOutages...),
		EnteredAt:               enteredAt.UTC(),
		NeedsAttentionEnteredAt: attentionEnteredAt.UTC(),
		ObservedAt:              observedAt.UTC(),
	}
}

func eventID(identity string, transition string, enteredAt time.Time) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + transition + "\x00" + enteredAt.UTC().Format(time.RFC3339Nano)))
	return "health-" + hex.EncodeToString(sum[:12])
}

func (m *Manager) deliverDue(ctx context.Context, states map[string]*durableState, now time.Time) error {
	type queued struct {
		state    *durableState
		delivery *deliveryState
	}
	queue := []queued{}
	for _, state := range states {
		for index := range state.Deliveries {
			delivery := &state.Deliveries[index]
			if delivery.DeliveredAt != nil || delivery.FailedAt != nil || (delivery.NextAttemptAt != nil && now.Before(*delivery.NextAttemptAt)) {
				continue
			}
			queue = append(queue, queued{state: state, delivery: delivery})
		}
	}
	slices.SortFunc(queue, func(left queued, right queued) int {
		if compared := left.delivery.Event.EnteredAt.Compare(right.delivery.Event.EnteredAt); compared != 0 {
			return compared
		}
		return strings.Compare(left.delivery.Event.ID, right.delivery.Event.ID)
	})
	if len(queue) > maxDeliveriesPerCycle {
		queue = queue[:maxDeliveriesPerCycle]
	}
	changed := map[string]*durableState{}
	for _, item := range queue {
		changed[item.state.Identity] = item.state
		delivery := item.delivery
		delivery.Attempts++
		attemptedAt := now
		delivery.LastAttemptAt = &attemptedAt
		if err := m.sender.Send(ctx, delivery.Event); err != nil {
			delivery.LastError = err.Error()
			if delivery.Attempts >= m.cfg.MaxAttempts {
				delivery.FailedAt = &attemptedAt
				delivery.NextAttemptAt = nil
				m.logger.Error("health notification delivery exhausted", "event_id", delivery.Event.ID, "identity", delivery.Event.Identity, "attempts", delivery.Attempts, "error", err)
			} else {
				nextAttemptAt := now.Add(m.retryDelay(delivery.Attempts))
				delivery.NextAttemptAt = &nextAttemptAt
				m.logger.Warn("health notification delivery failed", "event_id", delivery.Event.ID, "identity", delivery.Event.Identity, "attempt", delivery.Attempts, "next_attempt_at", nextAttemptAt, "error", err)
			}
			continue
		}
		delivery.DeliveredAt = &attemptedAt
		delivery.NextAttemptAt = nil
		delivery.FailedAt = nil
		delivery.LastError = ""
	}
	return m.saveStates(ctx, changed, now)
}

func (m *Manager) retryDelay(attempts int) time.Duration {
	delay := m.cfg.RetryBase
	for attempt := 1; attempt < attempts && delay < m.cfg.RetryMax; attempt++ {
		delay *= 2
	}
	if delay > m.cfg.RetryMax {
		return m.cfg.RetryMax
	}
	return delay
}

func (m *Manager) loadStates(ctx context.Context) (map[string]*durableState, error) {
	records, err := m.store.ListHealthNotificationStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("load health notification states: %w", err)
	}
	states := make(map[string]*durableState, len(records))
	for _, record := range records {
		var state durableState
		if err := json.Unmarshal(record.StateJSON, &state); err != nil {
			return nil, fmt.Errorf("decode health notification state %s: %w", record.Identity, err)
		}
		if state.Schema != SchemaVersion {
			return nil, fmt.Errorf("health notification state %s has unsupported schema %d", record.Identity, state.Schema)
		}
		if strings.TrimSpace(state.Identity) != strings.TrimSpace(record.Identity) {
			return nil, fmt.Errorf("health notification state identity %q does not match record %q", state.Identity, record.Identity)
		}
		states[state.Identity] = &state
	}
	return states, nil
}

func (m *Manager) saveStates(ctx context.Context, states map[string]*durableState, now time.Time) error {
	if len(states) == 0 {
		return nil
	}
	identities := make([]string, 0, len(states))
	for identity := range states {
		identities = append(identities, identity)
	}
	slices.Sort(identities)
	records := make([]store.HealthNotificationState, 0, len(identities))
	for _, identity := range identities {
		encoded, err := json.Marshal(states[identity])
		if err != nil {
			return fmt.Errorf("encode health notification state %s: %w", identity, err)
		}
		records = append(records, store.HealthNotificationState{
			Identity:  identity,
			StateJSON: encoded,
			UpdatedAt: now,
		})
	}
	if err := m.store.SaveHealthNotificationStates(ctx, records); err != nil {
		return fmt.Errorf("save health notification states: %w", err)
	}
	return nil
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
