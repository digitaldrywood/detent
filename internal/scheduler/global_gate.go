package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DispatchGateReasonGranted              = "granted"
	DispatchGateReasonGlobalCapacityFull   = "global_capacity_full"
	DispatchGateReasonOutsideActiveWindow  = "outside_active_window"
	DispatchGateReasonPaused               = "dispatch_paused"
	DispatchGateReasonPressureCapacityFull = "pressure_capacity_full"
)

type ProjectDispatchGate interface {
	MarkReady(ProjectCandidate)
	MarkIdle(ProjectCandidate)
	TryAcquire(context.Context, ProjectCandidate, SlotRequest, time.Time) (Slot, bool, error)
	Release(Slot) error
}

type DispatchGateDecision struct {
	ProjectID            string
	PoolName             string
	Holders              []string
	State                string
	SelectedProjectID    string
	SelectedState        string
	Reason               string
	GlobalCapacity       int
	GlobalUsed           int
	GlobalAvailable      int
	StateCapacity        int
	StateUsed            int
	StateAvailable       int
	LowerPriorityRunning int
	ReadyProjects        int
	RunningProjects      int
	GuaranteedCapacity   int
	BurstCapacity        int
	BorrowedSlots        int
	SharedCapacity       int
	SharedUsed           int
	SharedAvailable      int
	PressureCapacity     int
	PressureUsed         int
	PressureAvailable    int
}

type poolCapacityAdmission struct {
	allow func(int, int, SlotRequest) string
}

type readyProjectSlot struct {
	ProjectCandidate
	request SlotRequest
}

type runningProjectSlot struct {
	RunningProject
	slot Slot
}

type projectCycleState struct {
	idle           bool
	observedDemand bool
}

type GlobalDispatchGate struct {
	poolName string
	global   GlobalScheduler
	admit    *poolCapacityAdmission

	requestMu      sync.Mutex
	pending        []*dispatchRequest
	mu             sync.Mutex
	ready          map[string]readyProjectSlot
	running        map[uint64]runningProjectSlot
	projects       map[string]ProjectCandidate
	pausedProjects map[string]struct{}
	projectCycles  map[string]projectCycleState
	dispatchPauses int
}

func NewGlobalDispatchGate(global GlobalScheduler, projects ...ProjectCandidate) *GlobalDispatchGate {
	return newGlobalDispatchGate(DefaultPoolName, global, projects...)
}

func newGlobalDispatchGate(poolName string, global GlobalScheduler, projects ...ProjectCandidate) *GlobalDispatchGate {
	gate := &GlobalDispatchGate{
		poolName:       normalizePoolName(poolName),
		global:         global,
		ready:          map[string]readyProjectSlot{},
		running:        map[uint64]runningProjectSlot{},
		projects:       map[string]ProjectCandidate{},
		pausedProjects: map[string]struct{}{},
		projectCycles:  map[string]projectCycleState{},
	}
	gate.SetProjects(projects)
	return gate
}

func (g *GlobalDispatchGate) PoolSnapshot() PoolSnapshot {
	if g == nil || g.global == nil {
		return PoolSnapshot{Name: DefaultPoolName}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	stats := g.capacitySnapshotLocked("")
	holders := g.holderProjectIDsLocked()
	return PoolSnapshot{
		Name:       g.poolName,
		Capacity:   stats.globalCapacity,
		Guaranteed: stats.globalCapacity,
		BurstTo:    stats.globalCapacity,
		Used:       stats.globalUsed,
		Available:  nonNegativeInt(stats.globalCapacity - stats.globalUsed),
		Mode:       g.global.Mode(),
		Draining:   stats.draining,
		Holders:    holders,
	}
}

func (g *GlobalDispatchGate) holderProjectIDsLocked() []string {
	holders := make([]string, 0, len(g.running))
	seen := make(map[string]struct{}, len(g.running))
	for _, running := range g.running {
		projectID := strings.TrimSpace(running.ProjectID)
		if projectID == "" {
			continue
		}
		if _, ok := seen[projectID]; ok {
			continue
		}
		seen[projectID] = struct{}{}
		holders = append(holders, projectID)
	}
	sort.Strings(holders)
	return holders
}

func (g *GlobalDispatchGate) SetProjects(projects []ProjectCandidate) {
	if g == nil {
		return
	}

	configured := normalizeConfiguredProjectCandidates(projects)
	g.mu.Lock()
	defer g.mu.Unlock()

	next := make(map[string]ProjectCandidate, len(configured))
	paused := make(map[string]struct{})
	for _, project := range configured {
		if project.Paused {
			paused[project.ID] = struct{}{}
			delete(g.ready, project.ID)
			delete(g.projectCycles, project.ID)
			continue
		}
		next[project.ID] = project
	}
	for projectID := range g.projects {
		if _, ok := next[projectID]; ok {
			continue
		}
		delete(g.ready, projectID)
		delete(g.projectCycles, projectID)
	}
	for projectID := range g.projectCycles {
		if _, ok := next[projectID]; !ok {
			delete(g.projectCycles, projectID)
		}
	}
	g.projects = next
	g.pausedProjects = paused
}

func (g *GlobalDispatchGate) PauseDispatch() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	g.dispatchPauses++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.dispatchPauses > 0 {
				g.dispatchPauses--
			}
		})
	}
}

func (g *GlobalDispatchGate) Reconfigure(cfg Config) error {
	if g == nil || g.global == nil {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.global.Reconfigure(cfg); err != nil {
		return err
	}
	return nil
}

func (g *GlobalDispatchGate) BeginProjectCycle(project ProjectCandidate) {
	if g == nil || g.global == nil {
		return
	}
	project, ok := normalizeSingleProjectCandidate(project)
	if !ok {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, paused := g.pausedProjects[project.ID]; paused {
		delete(g.ready, project.ID)
		g.projectCycles[project.ID] = projectCycleState{idle: true}
		return
	}

	g.projects[project.ID] = project
	if ready, readyOK := g.ready[project.ID]; readyOK {
		ready.ProjectCandidate = project
		g.ready[project.ID] = ready
	}
	g.projectCycles[project.ID] = projectCycleState{}
}

func (g *GlobalDispatchGate) EndProjectCycle(projectID string) {
	if g == nil || g.global == nil {
		return
	}
	projectID = normalizeProjectID(projectID)
	if projectID == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	cycle, cycleOK := g.projectCycles[projectID]
	if cycleOK && !cycle.observedDemand {
		delete(g.ready, projectID)
	}
	delete(g.projectCycles, projectID)
}

func (g *GlobalDispatchGate) MarkReady(project ProjectCandidate) {
	if g == nil || g.global == nil {
		return
	}
	project, ok := normalizeSingleProjectCandidate(project)
	if !ok {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, paused := g.pausedProjects[project.ID]; paused {
		delete(g.ready, project.ID)
		g.projectCycles[project.ID] = projectCycleState{idle: true}
		return
	}

	g.projects[project.ID] = project
	ready := g.ready[project.ID]
	ready.ProjectCandidate = project
	g.ready[project.ID] = ready
	g.observeProjectCycleDemandLocked(project.ID)
}

func (g *GlobalDispatchGate) MarkIdle(project ProjectCandidate) {
	if g == nil {
		return
	}
	projectID := normalizeProjectID(project.ID)
	if projectID == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.ready, projectID)
	if g.global != nil {
		g.projectCycles[projectID] = projectCycleState{idle: true}
	}
}

func (g *GlobalDispatchGate) TryAcquire(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, error) {
	slot, ok, _, err := g.TryAcquireWithDecision(ctx, project, req, now)
	return slot, ok, err
}

// dispatchRequest lives only for the duration of TryAcquireWithDecision. Its
// caller is present to consume the result; a refused call retains no ownership.
type dispatchRequest struct {
	ctx      context.Context //nolint:containedctx // A synchronous acquisition carries caller cancellation only until that call returns.
	project  ProjectCandidate
	request  SlotRequest
	now      time.Time
	slot     Slot
	granted  bool
	decision DispatchGateDecision
	err      error
}

func (g *GlobalDispatchGate) TryAcquireWithDecision(ctx context.Context, project ProjectCandidate, req SlotRequest, now time.Time) (Slot, bool, DispatchGateDecision, error) {
	if g == nil || g.global == nil {
		return Slot{}, true, DispatchGateDecision{PoolName: DefaultPoolName, Reason: DispatchGateReasonGranted}, nil
	}
	call := &dispatchRequest{ctx: ctx, project: project, request: req, now: now}
	g.requestMu.Lock()
	g.pending = append(g.pending, call)
	g.requestMu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.requestMu.Lock()
	pending := g.pending
	g.pending = nil
	g.requestMu.Unlock()
	g.dispatchLocked(pending)
	return call.slot, call.granted, call.decision, call.err
}

// dispatchLocked ranks the currently calling requests, then attempts real
// acquisition for each. A lane/host/weight ceiling never excludes another
// request from using the remaining capacity. There is no selected owner between
// calls, and no acquisition waits for a future project cycle.
func (g *GlobalDispatchGate) dispatchLocked(pending []*dispatchRequest) {
	pending = slices.Clone(pending)
	for len(pending) > 0 {
		index := 0
		// Lane priority orders requests; strict project priority is an optional
		// leading sort key, sharing the same acquisition lifecycle as all modes.
		for i := 1; i < len(pending); i++ {
			left, right := pending[i], pending[index]
			if g.global.Mode() == ModeStrictPriority && priorityRank(left.project.Priority) != priorityRank(right.project.Priority) {
				if priorityRank(left.project.Priority) < priorityRank(right.project.Priority) {
					index = i
				}
				continue
			}
			if left.request.Priority < right.request.Priority {
				index = i
			}
		}
		best := pending[index]
		projects := make([]ProjectCandidate, 0, len(pending))
		for _, call := range pending {
			if call.request.Priority == best.request.Priority && (g.global.Mode() != ModeStrictPriority || priorityRank(call.project.Priority) == priorityRank(best.project.Priority)) {
				projects = append(projects, call.project)
			}
		}
		// Selection is only an ordering decision. Capacity belongs exclusively
		// to RequestSlot below, including when the semaphore is draining.
		selection, err := g.global.SelectProject(best.ctx, ProjectSelectionRequest{Projects: projects, Now: best.now})
		if err == nil {
			for i, call := range pending {
				if call.project.ID == selection.Project.ID && call.request.Priority == best.request.Priority {
					index = i
					break
				}
			}
		} else if !errors.Is(err, ErrNoSlots) && !errors.Is(err, ErrNoCandidates) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			best.err = err
			pending = append(pending[:index], pending[index+1:]...)
			continue
		}
		call := pending[index]
		call.slot, call.granted, call.decision, call.err = g.acquireLocked(call.ctx, call.project, call.request, call.now)
		pending = append(pending[:index], pending[index+1:]...)
	}
}

func (g *GlobalDispatchGate) acquireLocked(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, DispatchGateDecision, error) {
	if g == nil || g.global == nil {
		return Slot{}, true, DispatchGateDecision{
			PoolName: DefaultPoolName,
			Reason:   DispatchGateReasonGranted,
		}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	project, ok := normalizeSingleProjectCandidate(project)
	if !ok {
		return Slot{}, false, DispatchGateDecision{}, ErrNoCandidates
	}
	req, err := normalizeSlotRequest(req)
	if err != nil {
		return Slot{}, false, DispatchGateDecision{}, err
	}

	select {
	case <-ctx.Done():
		return Slot{}, false, DispatchGateDecision{}, ctx.Err()
	default:
	}
	if _, paused := g.pausedProjects[project.ID]; paused {
		delete(g.ready, project.ID)
		g.projectCycles[project.ID] = projectCycleState{idle: true}
		return Slot{}, false, g.decisionLocked(project.ID, req, DispatchGateReasonPaused), nil
	}

	g.projects[project.ID] = project
	status, err := project.ActiveHoursStatus(now)
	if err != nil {
		return Slot{}, false, DispatchGateDecision{}, fmt.Errorf("evaluate project %q active hours: %w", project.ID, err)
	}
	if !status.Open {
		delete(g.ready, project.ID)
		g.projectCycles[project.ID] = projectCycleState{idle: true}
		return Slot{}, false, g.decisionLocked(project.ID, req, DispatchGateReasonOutsideActiveWindow), nil
	}
	ready := g.ready[project.ID]
	ready.ProjectCandidate = project
	ready.request = req
	g.ready[project.ID] = ready
	g.observeProjectCycleDemandLocked(project.ID)
	if g.dispatchPauses > 0 {
		return Slot{}, false, g.decisionLocked(project.ID, req, DispatchGateReasonPaused), nil
	}
	currentUsed := g.capacitySnapshotLocked("").globalUsed
	if req.PressureCapacity > 0 && currentUsed+req.Weight > req.PressureCapacity {
		return Slot{}, false, g.decisionLocked(project.ID, req, DispatchGateReasonPressureCapacityFull), nil
	}
	projectedUsed := currentUsed + req.Weight
	if g.admit != nil {
		if reason := g.admit.allow(currentUsed, projectedUsed, req); reason != "" {
			return Slot{}, false, g.decisionLocked(project.ID, req, reason), nil
		}
	}

	slot, err := g.global.RequestSlot(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNoSlots) {
			reason := DispatchGateReasonGlobalCapacityFull
			decision := g.decisionLocked(project.ID, req, reason)
			return Slot{}, false, decision, nil
		}
		return Slot{}, false, DispatchGateDecision{}, err
	}
	if err := g.global.RecordProjectDispatch(ctx, ProjectDispatch{
		ProjectID:    project.ID,
		Weight:       project.Weight,
		DispatchedAt: now,
	}); err != nil {
		return Slot{}, false, DispatchGateDecision{}, errors.Join(err, g.global.ReleaseSlot(slot))
	}

	decision := g.decisionLocked(project.ID, req, DispatchGateReasonGranted)
	delete(g.ready, project.ID)
	g.running[slot.token] = runningProjectSlot{
		RunningProject: RunningProject{
			ProjectID:    project.ID,
			Priority:     project.Priority,
			State:        slot.State,
			SlotPriority: slot.Priority,
		},
		slot: slot,
	}
	return slot, true, decision, nil
}

func (g *GlobalDispatchGate) observeProjectCycleDemandLocked(projectID string) {
	cycle, ok := g.projectCycles[projectID]
	if !ok {
		return
	}
	cycle.observedDemand = true
	cycle.idle = false
	g.projectCycles[projectID] = cycle
}

func (g *GlobalDispatchGate) Release(slot Slot) error {
	if g == nil || g.global == nil || slot == (Slot{}) {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	running, ok := g.running[slot.token]
	if !ok {
		return nil
	}
	if err := g.global.ReleaseSlot(slot); err != nil && !errors.Is(err, ErrSlotNotHeld) {
		return err
	}
	delete(g.running, slot.token)
	if cycle, ok := g.projectCycles[running.ProjectID]; ok && cycle.idle {
		g.projectCycles[running.ProjectID] = projectCycleState{}
	}
	return nil
}

func (g *GlobalDispatchGate) decisionLocked(projectID string, req SlotRequest, reason string) DispatchGateDecision {
	stats := g.capacitySnapshotLocked(req.State)

	return DispatchGateDecision{
		ProjectID:            projectID,
		PoolName:             g.poolName,
		Holders:              g.holderProjectIDsLocked(),
		State:                req.State,
		SelectedProjectID:    projectID,
		SelectedState:        req.State,
		Reason:               reason,
		GlobalCapacity:       stats.globalCapacity,
		GlobalUsed:           stats.globalUsed,
		GlobalAvailable:      nonNegativeInt(stats.globalCapacity - stats.globalUsed),
		StateCapacity:        stats.stateCapacity,
		StateUsed:            stats.stateUsed,
		StateAvailable:       nonNegativeInt(stats.stateCapacity - stats.stateUsed),
		PressureCapacity:     req.PressureCapacity,
		PressureUsed:         stats.globalUsed,
		PressureAvailable:    nonNegativeInt(req.PressureCapacity - stats.globalUsed),
		LowerPriorityRunning: g.lowerPriorityRunningLocked(req.Priority),
		ReadyProjects:        len(g.ready),
		RunningProjects:      len(g.running),
	}
}

func (g *GlobalDispatchGate) capacitySnapshotLocked(state string) capacitySnapshot {
	if snapshotter, ok := g.global.(interface{ capacitySnapshot(string) capacitySnapshot }); ok {
		return snapshotter.capacitySnapshot(state)
	}
	return capacitySnapshot{}
}

func (g *GlobalDispatchGate) lowerPriorityRunningLocked(priority int) int {
	count := 0
	for _, running := range g.running {
		if running.slot.Priority > priority {
			count++
		}
	}
	return count
}

func normalizeSlotRequest(req SlotRequest) (SlotRequest, error) {
	slot, err := normalizeRequest(req)
	if err != nil {
		return SlotRequest{}, err
	}
	return SlotRequest{
		State:            slot.State,
		Host:             slot.Host,
		Weight:           slot.Weight,
		Priority:         slot.Priority,
		PressureCapacity: normalizedCapacity(req.PressureCapacity),
	}, nil
}

func nonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func normalizeSingleProjectCandidate(project ProjectCandidate) (ProjectCandidate, bool) {
	projects := normalizeProjectCandidates([]ProjectCandidate{project})
	if len(projects) == 0 {
		return ProjectCandidate{}, false
	}
	return projects[0], true
}
