package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const DefaultPoolName = "default"

type PoolConfig struct {
	Name      string
	BurstTo   int
	Scheduler Config
}

type PoolSnapshot struct {
	Name       string
	Capacity   int
	Guaranteed int
	BurstTo    int
	Used       int
	Borrowed   int
	Available  int
	Mode       Mode
	Draining   bool
	Reclaiming bool
	Generation uint64
	Holders    []string
}

type PoolSnapshotter interface {
	PoolSnapshot() PoolSnapshot
}

type ProjectPoolSnapshotter interface {
	PoolSnapshotFor(string) PoolSnapshot
}

type poolRuntime struct {
	name       string
	generation uint64
	gate       *GlobalDispatchGate
	guaranteed int
	burstTo    int
	retired    bool
}

type poolPause struct {
	releases map[uint64]func()
}

type PoolRegistry struct {
	reconfigureMu sync.Mutex

	mu             sync.RWMutex
	active         map[string]*poolRuntime
	byGeneration   map[uint64]*poolRuntime
	projectPools   map[string]string
	pauses         map[uint64]poolPause
	nextGeneration uint64
	nextPause      uint64
	elastic        elasticPoolState
}

var _ ProjectDispatchGate = (*PoolRegistry)(nil)

func NewPoolRegistry(pools []PoolConfig, projects []ProjectCandidate) (*PoolRegistry, error) {
	registry := &PoolRegistry{
		active:       map[string]*poolRuntime{},
		byGeneration: map[uint64]*poolRuntime{},
		projectPools: map[string]string{},
		pauses:       map[uint64]poolPause{},
		elastic:      newElasticPoolState(),
	}
	if err := registry.Reconfigure(pools, projects); err != nil {
		return nil, err
	}
	return registry, nil
}

func (r *PoolRegistry) GateFor(projectID string) ProjectDispatchGate {
	return &projectPoolGate{
		registry:  r,
		projectID: normalizeProjectID(projectID),
	}
}

func (r *PoolRegistry) SetProjects(projects []ProjectCandidate) {
	if r == nil {
		return
	}
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	r.mu.Lock()
	r.setProjectsLocked(projects)
	r.mu.Unlock()
	r.cleanupElasticWaiters()
}

func (r *PoolRegistry) Reconfigure(pools []PoolConfig, projects []ProjectCandidate) error {
	if r == nil {
		return nil
	}
	configs, err := normalizePoolConfigs(pools)
	if err != nil {
		return err
	}

	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	r.mu.RLock()
	current := make(map[string]*poolRuntime, len(r.active))
	for name, runtime := range r.active {
		current[name] = runtime
	}
	r.mu.RUnlock()

	next := make(map[string]*poolRuntime, len(configs))
	for name, cfg := range configs {
		if runtime, ok := current[name]; ok {
			if err := reconfigurePoolRuntime(runtime, cfg); err != nil {
				return fmt.Errorf("reconfigure agent pool %q: %w", name, err)
			}
			next[name] = runtime
			continue
		}
		runtime, err := newPoolRuntime(cfg)
		if err != nil {
			return err
		}
		r.setCapacityAdmission(runtime)
		next[name] = runtime
	}

	r.mu.Lock()
	for name, runtime := range current {
		if _, ok := next[name]; ok {
			continue
		}
		runtime.retired = true
		runtime.gate.SetProjects(nil)
		_ = runtime.gate.PauseDispatch()
		r.elastic.remove(runtime.generation)
	}
	for _, runtime := range next {
		if runtime.generation != 0 {
			continue
		}
		r.nextGeneration++
		if r.nextGeneration == 0 {
			r.nextGeneration++
		}
		runtime.generation = r.nextGeneration
		r.byGeneration[runtime.generation] = runtime
		for pauseID, pause := range r.pauses {
			pause.releases[runtime.generation] = runtime.gate.PauseDispatch()
			r.pauses[pauseID] = pause
		}
	}
	r.active = next
	r.setProjectsLocked(projects)
	r.cleanupRetiredLocked()
	r.mu.Unlock()
	r.cleanupElasticWaiters()
	return nil
}

func (r *PoolRegistry) PauseDispatch() func() {
	if r == nil {
		return func() {}
	}

	r.reconfigureMu.Lock()
	r.mu.Lock()
	r.nextPause++
	if r.nextPause == 0 {
		r.nextPause++
	}
	pauseID := r.nextPause
	pause := poolPause{releases: make(map[uint64]func(), len(r.byGeneration))}
	for generation, runtime := range r.byGeneration {
		pause.releases[generation] = runtime.gate.PauseDispatch()
	}
	r.pauses[pauseID] = pause
	r.mu.Unlock()
	r.reconfigureMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			pause, ok := r.pauses[pauseID]
			if ok {
				delete(r.pauses, pauseID)
			}
			r.mu.Unlock()
			if !ok {
				return
			}
			for _, release := range pause.releases {
				release()
			}
			r.cleanupRetired()
		})
	}
}

func (r *PoolRegistry) PoolSnapshotFor(projectID string) PoolSnapshot {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForProject(projectID)
	if runtime == nil {
		return PoolSnapshot{Name: DefaultPoolName}
	}
	return r.elasticPoolSnapshot(runtime)
}

func (r *PoolRegistry) PoolSnapshots() []PoolSnapshot {
	if r == nil {
		return nil
	}
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	r.mu.RLock()
	runtimes := make([]*poolRuntime, 0, len(r.byGeneration))
	for _, runtime := range r.byGeneration {
		runtimes = append(runtimes, runtime)
	}
	r.mu.RUnlock()
	snapshots := make([]PoolSnapshot, 0, len(runtimes))
	for _, runtime := range runtimes {
		snapshots = append(snapshots, r.elasticPoolSnapshot(runtime))
	}
	slices.SortFunc(snapshots, func(left PoolSnapshot, right PoolSnapshot) int {
		if compared := strings.Compare(left.Name, right.Name); compared != 0 {
			return compared
		}
		switch {
		case left.Generation < right.Generation:
			return -1
		case left.Generation > right.Generation:
			return 1
		default:
			return 0
		}
	})
	return snapshots
}

func (r *PoolRegistry) MarkReady(project ProjectCandidate) {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForCandidate(project)
	if runtime != nil {
		runtime.gate.MarkReady(project)
	}
}

func (r *PoolRegistry) MarkIdle(projectID string) {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForProject(projectID)
	if runtime != nil {
		runtime.gate.MarkIdle(projectID)
		if !runtime.gate.hasReadyProjects() {
			r.elastic.remove(runtime.generation)
		}
	}
}

func (r *PoolRegistry) BeginProjectCycle(project ProjectCandidate) {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForCandidate(project)
	if runtime != nil {
		runtime.gate.BeginProjectCycle(project)
	}
}

func (r *PoolRegistry) EndProjectCycle(projectID string) {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForProject(projectID)
	if runtime != nil {
		runtime.gate.EndProjectCycle(projectID)
	}
}

func (r *PoolRegistry) TryAcquire(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, error) {
	slot, ok, _, err := r.TryAcquireWithDecision(ctx, project, req, now)
	return slot, ok, err
}

func (r *PoolRegistry) TryAcquireWithDecision(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, DispatchGateDecision, error) {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForCandidate(project)
	if runtime == nil {
		return Slot{}, false, DispatchGateDecision{}, ErrNoCandidates
	}
	slot, ok, decision, err := runtime.gate.TryAcquireWithDecision(ctx, project, req, now)
	decision = r.elasticDecision(runtime, decision)
	if !ok || err != nil {
		return slot, ok, decision, err
	}
	slot.poolName = runtime.name
	slot.poolGeneration = runtime.generation
	return slot, true, decision, nil
}

func (r *PoolRegistry) SetPreempt(slot Slot, preempt func()) {
	runtime := r.runtimeForSlot(slot)
	if runtime != nil {
		runtime.gate.SetPreempt(slot, preempt)
	}
}

func (r *PoolRegistry) Release(slot Slot) error {
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	runtime := r.runtimeForSlot(slot)
	if runtime == nil {
		return ErrSlotNotHeld
	}
	if err := runtime.gate.Release(slot); err != nil {
		return err
	}
	r.cleanupElasticWaiters()
	r.mu.Lock()
	r.cleanupRetiredLocked()
	r.mu.Unlock()
	return nil
}

func (r *PoolRegistry) setProjectsLocked(projects []ProjectCandidate) {
	normalized := normalizeProjectCandidates(projects)
	projectPools := make(map[string]string, len(normalized))
	grouped := make(map[string][]ProjectCandidate, len(r.active))
	for _, project := range normalized {
		pool := project.Pool
		if _, ok := r.active[pool]; !ok {
			pool = DefaultPoolName
			project.Pool = pool
		}
		projectPools[project.ID] = pool
		grouped[pool] = append(grouped[pool], project)
	}
	for name, runtime := range r.active {
		runtime.gate.SetProjects(grouped[name])
	}
	r.projectPools = projectPools
}

func (r *PoolRegistry) runtimeForCandidate(project ProjectCandidate) *poolRuntime {
	projectID := normalizeProjectID(project.ID)
	r.mu.RLock()
	pool := r.projectPools[projectID]
	if pool == "" {
		pool = normalizePoolName(project.Pool)
	}
	runtime := r.active[pool]
	if runtime == nil {
		runtime = r.active[DefaultPoolName]
	}
	r.mu.RUnlock()
	return runtime
}

func (r *PoolRegistry) runtimeForProject(projectID string) *poolRuntime {
	projectID = normalizeProjectID(projectID)
	r.mu.RLock()
	pool := r.projectPools[projectID]
	if pool == "" {
		pool = DefaultPoolName
	}
	runtime := r.active[pool]
	if runtime == nil {
		runtime = r.active[DefaultPoolName]
	}
	r.mu.RUnlock()
	return runtime
}

func (r *PoolRegistry) runtimeForSlot(slot Slot) *poolRuntime {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	runtime := r.byGeneration[slot.poolGeneration]
	if runtime == nil && slot.poolName != "" {
		runtime = r.active[slot.poolName]
	}
	r.mu.RUnlock()
	return runtime
}

func (r *PoolRegistry) cleanupRetired() {
	if r == nil {
		return
	}
	r.reconfigureMu.Lock()
	defer r.reconfigureMu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupRetiredLocked()
}

func (r *PoolRegistry) cleanupRetiredLocked() {
	for generation, runtime := range r.byGeneration {
		if !runtime.retired || runtime.gate.PoolSnapshot().Used > 0 {
			continue
		}
		delete(r.byGeneration, generation)
	}
}

func normalizePoolConfigs(pools []PoolConfig) (map[string]PoolConfig, error) {
	configs := make(map[string]PoolConfig, len(pools))
	for index, pool := range pools {
		name := strings.TrimSpace(pool.Name)
		if name == "" {
			return nil, fmt.Errorf("agent pool %d name is required", index)
		}
		if _, exists := configs[name]; exists {
			return nil, fmt.Errorf("agent pool %q is duplicated", name)
		}
		if pool.Scheduler.Capacity <= 0 {
			return nil, fmt.Errorf("agent pool %q capacity must be positive", name)
		}
		if pool.BurstTo != 0 && pool.BurstTo < pool.Scheduler.Capacity {
			return nil, fmt.Errorf("agent pool %q burst capacity must be greater than or equal to capacity", name)
		}
		if _, err := globalModeFromConfig(pool.Scheduler); err != nil {
			return nil, fmt.Errorf("configure agent pool %q: %w", name, err)
		}
		pool.Name = name
		configs[name] = pool
	}
	if _, ok := configs[DefaultPoolName]; !ok {
		return nil, fmt.Errorf("agent pool %q is required", DefaultPoolName)
	}
	return configs, nil
}

func newPoolRuntime(cfg PoolConfig) (*poolRuntime, error) {
	schedulerConfig := cfg.Scheduler
	schedulerConfig.Capacity = effectivePoolBurst(cfg)
	sched, err := NewFromConfig(schedulerConfig)
	if err != nil {
		return nil, fmt.Errorf("create agent pool %q: %w", cfg.Name, err)
	}
	global, ok := sched.(GlobalScheduler)
	if !ok {
		return nil, fmt.Errorf("create agent pool %q: %w", cfg.Name, ErrUnsupportedBackend)
	}
	return &poolRuntime{
		name:       cfg.Name,
		gate:       newGlobalDispatchGate(cfg.Name, global),
		guaranteed: cfg.Scheduler.Capacity,
		burstTo:    effectivePoolBurst(cfg),
	}, nil
}

type projectPoolGate struct {
	registry  *PoolRegistry
	projectID string
}

var _ ProjectDispatchGate = (*projectPoolGate)(nil)

func (g *projectPoolGate) PoolSnapshot() PoolSnapshot {
	if g == nil || g.registry == nil {
		return PoolSnapshot{Name: DefaultPoolName}
	}
	return g.registry.PoolSnapshotFor(g.projectID)
}

func (g *projectPoolGate) MarkReady(project ProjectCandidate) {
	project.ID = g.projectID
	g.registry.MarkReady(project)
}

func (g *projectPoolGate) MarkIdle(string) {
	g.registry.MarkIdle(g.projectID)
}

func (g *projectPoolGate) BeginProjectCycle(project ProjectCandidate) {
	project.ID = g.projectID
	g.registry.BeginProjectCycle(project)
}

func (g *projectPoolGate) EndProjectCycle(string) {
	g.registry.EndProjectCycle(g.projectID)
}

func (g *projectPoolGate) TryAcquire(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, error) {
	project.ID = g.projectID
	return g.registry.TryAcquire(ctx, project, req, now)
}

func (g *projectPoolGate) TryAcquireWithDecision(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, DispatchGateDecision, error) {
	project.ID = g.projectID
	return g.registry.TryAcquireWithDecision(ctx, project, req, now)
}

func (g *projectPoolGate) SetPreempt(slot Slot, preempt func()) {
	g.registry.SetPreempt(slot, preempt)
}

func (g *projectPoolGate) Release(slot Slot) error {
	return g.registry.Release(slot)
}
