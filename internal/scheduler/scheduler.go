package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultCapacity = 1

const (
	ModeCountingSemaphore Mode = "counting_semaphore"
	ModeWeightedFair      Mode = "weighted_fair"
	ModeStrictPriority    Mode = "strict_priority"
	ModeRoundRobin        Mode = "round_robin"
	ModeFairShare         Mode = "fair_share"
)

var (
	ErrInvalidWeight         = errors.New("scheduler slot weight must be positive")
	ErrNoSlots               = errors.New("scheduler slot unavailable")
	ErrSlotNotHeld           = errors.New("scheduler slot is not held")
	ErrUnsupportedBackend    = errors.New("unsupported scheduler backend")
	ErrWeightExceedsCapacity = errors.New("scheduler slot weight exceeds capacity")
)

type Mode string

type Scheduler interface {
	RequestSlot(context.Context, SlotRequest) (Slot, error)
	ReleaseSlot(Slot) error
	Mode() Mode
}

type Config struct {
	Kind            string
	Capacity        int
	CapacityByState map[string]int
	CapacityPerHost int
	DecayHalfLife   time.Duration
	FairShareStore  FairShareStore
}

type SlotRequest struct {
	State  string
	Host   string
	Weight int
}

type Slot struct {
	State  string
	Host   string
	Weight int
	token  uint64
}

type Counters struct {
	Used        int
	UsedByState map[string]int
	UsedByHost  map[string]int
}

type CountingSemaphore struct {
	mu              sync.Mutex
	capacity        int
	capacityByState map[string]int
	capacityPerHost int
	used            int
	usedByState     map[string]int
	usedByHost      map[string]int
	active          map[uint64]Slot
	nextToken       uint64
}

var _ Scheduler = (*CountingSemaphore)(nil)

func NewFromConfig(cfg Config) (Scheduler, error) {
	switch normalizeKind(cfg.Kind) {
	case "", "weighted", "weighted_fair", "weightedfair":
		return NewWeightedFair(cfg), nil
	case "strict", "strict_priority", "strictpriority":
		return NewStrictPriority(cfg), nil
	case "round_robin", "roundrobin":
		return NewRoundRobin(cfg), nil
	case "fair_share", "fairshare":
		return NewFairShare(cfg), nil
	case "counting_semaphore", "semaphore":
		return NewCountingSemaphore(cfg), nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedBackend, strings.TrimSpace(cfg.Kind))
	}
}

func NewCountingSemaphore(cfg Config) *CountingSemaphore {
	capacity := cfg.Capacity
	if capacity <= 0 {
		capacity = defaultCapacity
	}

	return &CountingSemaphore{
		capacity:        capacity,
		capacityByState: normalizedCapacities(cfg.CapacityByState),
		capacityPerHost: normalizedCapacity(cfg.CapacityPerHost),
		usedByState:     map[string]int{},
		usedByHost:      map[string]int{},
		active:          map[uint64]Slot{},
	}
}

func (s *CountingSemaphore) Mode() Mode {
	return ModeCountingSemaphore
}

func (s *CountingSemaphore) RequestSlot(ctx context.Context, req SlotRequest) (Slot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return Slot{}, ctx.Err()
	default:
	}

	slot, err := normalizeRequest(req)
	if err != nil {
		return Slot{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-ctx.Done():
		return Slot{}, ctx.Err()
	default:
	}

	if err := s.checkCapacity(slot); err != nil {
		return Slot{}, err
	}
	if !s.canGrant(slot) {
		return Slot{}, ErrNoSlots
	}

	slot.token = s.nextSlotToken()
	s.active[slot.token] = slot
	s.used += slot.Weight
	increment(s.usedByState, slot.State, slot.Weight)
	increment(s.usedByHost, slot.Host, slot.Weight)

	return slot, nil
}

func (s *CountingSemaphore) ReleaseSlot(slot Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	held, ok := s.active[slot.token]
	if !ok || slot.token == 0 {
		return ErrSlotNotHeld
	}

	delete(s.active, slot.token)
	s.used -= held.Weight
	decrement(s.usedByState, held.State, held.Weight)
	decrement(s.usedByHost, held.Host, held.Weight)

	return nil
}

func (s *CountingSemaphore) Counters() Counters {
	s.mu.Lock()
	defer s.mu.Unlock()

	return Counters{
		Used:        s.used,
		UsedByState: cloneIntMap(s.usedByState),
		UsedByHost:  cloneIntMap(s.usedByHost),
	}
}

func (s *CountingSemaphore) checkCapacity(slot Slot) error {
	if slot.Weight > s.capacity {
		return fmt.Errorf("%w: global capacity %d", ErrWeightExceedsCapacity, s.capacity)
	}
	if capacity, ok := s.capacityByState[slot.State]; ok && slot.Weight > capacity {
		return fmt.Errorf("%w: state %q capacity %d", ErrWeightExceedsCapacity, slot.State, capacity)
	}
	if slot.Host != "" && s.capacityPerHost > 0 && slot.Weight > s.capacityPerHost {
		return fmt.Errorf("%w: host %q capacity %d", ErrWeightExceedsCapacity, slot.Host, s.capacityPerHost)
	}

	return nil
}

func (s *CountingSemaphore) canGrant(slot Slot) bool {
	if s.used+slot.Weight > s.capacity {
		return false
	}
	if capacity, ok := s.capacityByState[slot.State]; ok && s.usedByState[slot.State]+slot.Weight > capacity {
		return false
	}
	if slot.Host != "" && s.capacityPerHost > 0 && s.usedByHost[slot.Host]+slot.Weight > s.capacityPerHost {
		return false
	}

	return true
}

func (s *CountingSemaphore) nextSlotToken() uint64 {
	s.nextToken++
	if s.nextToken == 0 {
		s.nextToken++
	}
	return s.nextToken
}

func normalizeRequest(req SlotRequest) (Slot, error) {
	weight := req.Weight
	if weight == 0 {
		weight = 1
	}
	if weight < 0 {
		return Slot{}, ErrInvalidWeight
	}

	return Slot{
		State:  normalizeState(req.State),
		Host:   normalizeHost(req.Host),
		Weight: weight,
	}, nil
}

func normalizedCapacities(values map[string]int) map[string]int {
	normalized := make(map[string]int, len(values))
	for key, value := range values {
		key = normalizeState(key)
		value = normalizedCapacity(value)
		if key == "" || value == 0 {
			continue
		}
		normalized[key] = value
	}
	return normalized
}

func normalizedCapacity(value int) int {
	if value <= 0 {
		return 0
	}
	return value
}

func increment(values map[string]int, key string, value int) {
	if key == "" {
		return
	}
	values[key] += value
}

func decrement(values map[string]int, key string, value int) {
	if key == "" {
		return
	}

	values[key] -= value
	if values[key] <= 0 {
		delete(values, key)
	}
}

func cloneIntMap(values map[string]int) map[string]int {
	cloned := make(map[string]int, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func normalizeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.ReplaceAll(kind, "-", "_")
	return kind
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func normalizeHost(host string) string {
	return strings.TrimSpace(host)
}
