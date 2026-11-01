package schedulehealth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	AdmissionID     = "backlog_admission"
	MissedIntervals = 2
	storeRetryDelay = time.Minute
)

var ErrMissingStore = errors.New("scheduled run store is required")

type Definition struct {
	ID       string
	Schedule string
}

type Run struct {
	ProjectID    string
	ScheduleID   string
	ScheduledFor time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Error        string
}

type Store interface {
	LatestScheduledRun(context.Context, string, string) (Run, bool, error)
	RecordScheduledRun(context.Context, Run) error
}

type Recorder interface {
	RecordScheduledRun(context.Context, Run) error
}

type Dependencies struct {
	Now       func() time.Time
	OnFault   func(error, time.Time)
	OnHealthy func()
}

type scheduledDefinition struct {
	definition Definition
	schedule   cron.Schedule
	baseline   time.Time
}

type Monitor struct {
	projectID string
	store     Store
	now       func() time.Time
	onFault   func(error, time.Time)
	onHealthy func()

	mu          sync.RWMutex
	definitions map[string]scheduledDefinition
	updates     chan struct{}
}

func New(projectID string, definitions []Definition, store Store, deps Dependencies) (*Monitor, error) {
	if len(definitions) > 0 && store == nil {
		return nil, ErrMissingStore
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	monitor := &Monitor{
		projectID:   strings.TrimSpace(projectID),
		store:       store,
		now:         now,
		onFault:     deps.OnFault,
		onHealthy:   deps.OnHealthy,
		definitions: map[string]scheduledDefinition{},
		updates:     make(chan struct{}, 1),
	}
	if err := monitor.Update(definitions); err != nil {
		return nil, err
	}
	return monitor, nil
}

func RoutineID(name string) string {
	return "routine:" + strings.ToLower(strings.TrimSpace(name))
}

func IntakeID(name string) string {
	return "intake:" + strings.ToLower(strings.TrimSpace(name))
}

func (m *Monitor) Update(definitions []Definition) error {
	if m == nil {
		return nil
	}
	now := m.now()
	prepared := make(map[string]scheduledDefinition, len(definitions))
	m.mu.RLock()
	previous := m.definitions
	m.mu.RUnlock()
	for _, definition := range definitions {
		definition.ID = strings.TrimSpace(definition.ID)
		definition.Schedule = strings.TrimSpace(definition.Schedule)
		if definition.ID == "" {
			return errors.New("schedule id is required")
		}
		parsed, err := cron.ParseStandard(definition.Schedule)
		if err != nil {
			return fmt.Errorf("parse schedule %s: %w", definition.ID, err)
		}
		baseline := now
		if prior, ok := previous[definition.ID]; ok && prior.definition.Schedule == definition.Schedule {
			baseline = prior.baseline
		}
		prepared[definition.ID] = scheduledDefinition{definition: definition, schedule: parsed, baseline: baseline}
	}
	m.mu.Lock()
	m.definitions = prepared
	m.mu.Unlock()
	m.signalUpdate()
	return nil
}

func (m *Monitor) RecordScheduledRun(ctx context.Context, run Run) error {
	if m == nil || m.store == nil {
		return ErrMissingStore
	}
	run.ProjectID = m.projectID
	run.ScheduleID = strings.TrimSpace(run.ScheduleID)
	if err := m.store.RecordScheduledRun(ctx, run); err != nil {
		return err
	}
	m.signalUpdate()
	return nil
}

func (m *Monitor) Run(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.resetBaselines(m.now())
	for {
		next := m.Check(ctx, m.now())
		if ctx.Err() != nil {
			return nil
		}
		var timer <-chan time.Time
		var value *time.Timer
		if !next.IsZero() {
			value = time.NewTimer(max(next.Sub(m.now()), 0))
			timer = value.C
		}
		select {
		case <-ctx.Done():
			if value != nil {
				value.Stop()
			}
			return nil
		case <-m.updates:
			if value != nil {
				value.Stop()
			}
		case <-timer:
		}
	}
}

func (m *Monitor) resetBaselines(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, definition := range m.definitions {
		definition.baseline = at
		m.definitions[id] = definition
	}
}

func (m *Monitor) Check(ctx context.Context, now time.Time) time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.RLock()
	definitions := make([]scheduledDefinition, 0, len(m.definitions))
	for _, definition := range m.definitions {
		definitions = append(definitions, definition)
	}
	m.mu.RUnlock()
	var next time.Time
	var faults []string
	for _, definition := range definitions {
		last, found, err := m.store.LatestScheduledRun(ctx, m.projectID, definition.definition.ID)
		if err != nil {
			faults = append(faults, fmt.Sprintf("%s ledger lookup failed: %v", definition.definition.ID, err))
			retryAt := now.Add(storeRetryDelay)
			if next.IsZero() || retryAt.Before(next) {
				next = retryAt
			}
			continue
		}
		after := definition.baseline.In(now.Location())
		if found && last.CompletedAt.After(after) {
			after = last.CompletedAt.In(now.Location())
		}
		deadline := after
		for range MissedIntervals {
			deadline = definition.schedule.Next(deadline)
		}
		if !now.Before(deadline) {
			faults = append(faults, fmt.Sprintf("%s produced no run by %s", definition.definition.ID, deadline.UTC().Format(time.RFC3339)))
			continue
		}
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	if len(faults) > 0 {
		if m.onFault != nil {
			m.onFault(fmt.Errorf("scheduled work stalled after %d expected intervals: %s", MissedIntervals, strings.Join(faults, "; ")), now)
		}
		return next
	}
	if m.onHealthy != nil {
		m.onHealthy()
	}
	return next
}

func (m *Monitor) signalUpdate() {
	select {
	case m.updates <- struct{}{}:
	default:
	}
}
