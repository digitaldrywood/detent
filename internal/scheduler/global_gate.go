package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxPriorityLaneBypasses                            = 1
	DispatchGateReasonGranted                          = "granted"
	DispatchGateReasonGlobalCapacityFull               = "global_capacity_full"
	DispatchGateReasonOutsideActiveWindow              = "outside_active_window"
	DispatchGateReasonPaused                           = "dispatch_paused"
	DispatchGateReasonReservedForHigherPriority        = "reserved_for_higher_priority_state"
	DispatchGateReasonReservedForHigherPriorityProject = "reserved_for_higher_priority_project"
	DispatchGateReasonSelectedProjectWaiting           = "selected_project_waiting"
	DispatchGateReasonPressureCapacityFull             = "pressure_capacity_full"
)

type ProjectDispatchGate interface {
	MarkReady(ProjectCandidate)
	MarkIdle(ProjectCandidate)
	TryAcquire(context.Context, ProjectCandidate, SlotRequest, time.Time) (Slot, bool, error)
	SetPreempt(Slot, func())
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
	allow    func(int, int, SlotRequest) string
	complete func(int)
}

type readyProjectSlot struct {
	ProjectCandidate
	request SlotRequest
}

type runningProjectSlot struct {
	RunningProject
	slot    Slot
	preempt func()
}

type selectedProjectSlot struct {
	ProjectCandidate
	request     SlotRequest
	preemptions []RunningProject
}

type projectCycleState struct {
	idle           bool
	observedDemand bool
}

type GlobalDispatchGate struct {
	poolName string
	global   GlobalScheduler
	admit    *poolCapacityAdmission

	mu               sync.Mutex
	ready            map[string]readyProjectSlot
	running          map[uint64]runningProjectSlot
	selected         map[string]selectedProjectSlot
	projects         map[string]ProjectCandidate
	pausedProjects   map[string]struct{}
	projectCycles    map[string]projectCycleState
	priorityBypasses map[string]int
	dispatchPauses   int
}

func NewGlobalDispatchGate(global GlobalScheduler, projects ...ProjectCandidate) *GlobalDispatchGate {
	return newGlobalDispatchGate(DefaultPoolName, global, projects...)
}

func newGlobalDispatchGate(poolName string, global GlobalScheduler, projects ...ProjectCandidate) *GlobalDispatchGate {
	gate := &GlobalDispatchGate{
		poolName:         normalizePoolName(poolName),
		global:           global,
		ready:            map[string]readyProjectSlot{},
		running:          map[uint64]runningProjectSlot{},
		selected:         map[string]selectedProjectSlot{},
		projects:         map[string]ProjectCandidate{},
		pausedProjects:   map[string]struct{}{},
		projectCycles:    map[string]projectCycleState{},
		priorityBypasses: map[string]int{},
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
			delete(g.selected, project.ID)
			delete(g.projectCycles, project.ID)
			delete(g.priorityBypasses, project.ID)
			continue
		}
		next[project.ID] = project
	}
	for projectID := range g.projects {
		if _, ok := next[projectID]; ok {
			continue
		}
		delete(g.ready, projectID)
		delete(g.selected, projectID)
		delete(g.projectCycles, projectID)
		delete(g.priorityBypasses, projectID)
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
	clear(g.selected)
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

	previousMode := g.global.Mode()
	if err := g.global.Reconfigure(cfg); err != nil {
		return err
	}
	clear(g.selected)
	if previousMode != g.global.Mode() {
		clear(g.projectCycles)
		clear(g.priorityBypasses)
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
		delete(g.selected, project.ID)
		delete(g.priorityBypasses, project.ID)
		g.projectCycles[project.ID] = projectCycleState{idle: true}
		return
	}

	g.projects[project.ID] = project
	delete(g.selected, project.ID)
	if g.global.Mode() == ModeStrictPriority {
		delete(g.ready, project.ID)
	} else if ready, readyOK := g.ready[project.ID]; readyOK {
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
	_, waiting := g.ready[projectID]
	if g.global.Mode() == ModeStrictPriority {
		g.projectCycles[projectID] = projectCycleState{idle: !waiting}
	} else {
		if cycleOK && !cycle.observedDemand {
			delete(g.ready, projectID)
			delete(g.selected, projectID)
			waiting = false
		}
		delete(g.projectCycles, projectID)
	}
	if !waiting {
		delete(g.priorityBypasses, projectID)
	}
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
		delete(g.selected, project.ID)
		delete(g.priorityBypasses, project.ID)
		g.projectCycles[project.ID] = projectCycleState{idle: true}
		return
	}

	g.projects[project.ID] = project
	ready := g.ready[project.ID]
	ready.ProjectCandidate = project
	g.ready[project.ID] = ready
	g.observeProjectCycleDemandLocked(project.ID)
	if g.global.Mode() == ModeStrictPriority {
		g.projectCycles[project.ID] = projectCycleState{}
	}
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
	delete(g.selected, projectID)
	delete(g.priorityBypasses, projectID)
	if g.global != nil && g.global.Mode() == ModeStrictPriority {
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

func (g *GlobalDispatchGate) TryAcquireWithDecision(
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

	g.mu.Lock()
	defer g.mu.Unlock()

	select {
	case <-ctx.Done():
		return Slot{}, false, DispatchGateDecision{}, ctx.Err()
	default:
	}
	if _, paused := g.pausedProjects[project.ID]; paused {
		delete(g.ready, project.ID)
		delete(g.selected, project.ID)
		delete(g.priorityBypasses, project.ID)
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
		delete(g.selected, project.ID)
		delete(g.priorityBypasses, project.ID)
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
	// Priority orders who wins a contended slot; it never holds an idle one.
	// This previously denied a slot to a lower-priority project whenever any
	// higher-priority project was mid-cycle, even with capacity free and even
	// when that project could never dispatch -- e.g. every candidate declined
	// by its own authorization selector. That starved the fleet: 8 of 10 slots
	// idle while two projects waited on a project structurally unable to run.
	// Free capacity is now always offered; contention is resolved by ranking in
	// fillSelectionsLocked, and genuine exhaustion still reports
	// reserved_for_higher_priority_project via ErrNoSlots below.
	if g.global.Mode() == ModeStrictPriority {
		g.projectCycles[project.ID] = projectCycleState{}
	}
	g.reconcileSelectionsLocked()
	if err := g.fillSelectionsLocked(ctx, now, true); err != nil {
		if errors.Is(err, ErrNoSlots) {
			reason := DispatchGateReasonGlobalCapacityFull
			if g.hasHigherPriorityRunningProjectLocked(project) {
				reason = DispatchGateReasonReservedForHigherPriorityProject
			}
			decision := g.decisionLocked(project.ID, req, reason)
			return Slot{}, false, decision, nil
		}
		return Slot{}, false, DispatchGateDecision{}, err
	}

	// Not being the selected project is not grounds to refuse a free slot. A
	// selection only records who should win a CONTENDED slot; a project that
	// holds a selection but is not currently asking must not keep capacity
	// idle. If real capacity remains, this request takes it and the unclaimed
	// selection simply wins the next contended round. When capacity is truly
	// gone, RequestSlot below returns ErrNoSlots and reports the real reason.
	selected, selectedOK := g.selected[project.ID]
	strictFreeCapacity := g.global.Mode() == ModeStrictPriority && g.unreservedCapacityLocked() >= req.Weight
	if !selectedOK && !strictFreeCapacity {
		reason := g.waitReasonLocked(req)
		return Slot{}, false, g.decisionLocked(project.ID, req, reason), nil
	}
	currentUsed := g.capacitySnapshotLocked("").globalUsed
	if req.PressureCapacity > 0 && currentUsed+req.Weight > req.PressureCapacity {
		return Slot{}, false, g.decisionLocked(project.ID, req, DispatchGateReasonPressureCapacityFull), nil
	}
	projectedUsed := currentUsed - g.preemptionWeightLocked(selected.preemptions) + req.Weight
	if g.admit != nil {
		if reason := g.admit.allow(currentUsed, projectedUsed, req); reason != "" {
			return Slot{}, false, g.decisionLocked(project.ID, req, reason), nil
		}
	}
	if err := g.preemptProjectsLocked(selected.preemptions); err != nil {
		delete(g.selected, project.ID)
		return Slot{}, false, DispatchGateDecision{}, err
	}

	slot, err := g.global.RequestSlot(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNoSlots) {
			reason := DispatchGateReasonGlobalCapacityFull
			if g.hasHigherPriorityRunningProjectLocked(project) {
				reason = DispatchGateReasonReservedForHigherPriorityProject
			}
			decision := g.decisionLocked(project.ID, req, reason)
			return Slot{}, false, decision, nil
		}
		delete(g.selected, project.ID)
		return Slot{}, false, DispatchGateDecision{}, err
	}
	if err := g.global.RecordProjectDispatch(ctx, ProjectDispatch{
		ProjectID:    project.ID,
		Weight:       project.Weight,
		DispatchedAt: now,
	}); err != nil {
		delete(g.selected, project.ID)
		return Slot{}, false, DispatchGateDecision{}, errors.Join(err, g.global.ReleaseSlot(slot))
	}
	if g.admit != nil {
		g.admit.complete(g.capacitySnapshotLocked("").globalUsed)
	}
	g.recordPriorityLaneDispatchLocked(project.ID, req.Priority)

	decision := g.decisionLocked(project.ID, req, DispatchGateReasonGranted)
	delete(g.ready, project.ID)
	delete(g.selected, project.ID)
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

func (g *GlobalDispatchGate) preemptionWeightLocked(preemptions []RunningProject) int {
	used := make(map[uint64]struct{}, len(preemptions))
	weight := 0
	for _, preemption := range preemptions {
		for token, running := range g.running {
			if running.ProjectID != preemption.ProjectID ||
				running.Priority != preemption.Priority ||
				running.preempt == nil {
				continue
			}
			if _, ok := used[token]; ok {
				continue
			}
			used[token] = struct{}{}
			weight += running.slot.Weight
			break
		}
	}
	return weight
}

func (g *GlobalDispatchGate) hasReadyProjects() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.ready) > 0
}

func (g *GlobalDispatchGate) SetPreempt(slot Slot, preempt func()) {
	if g == nil || slot == (Slot{}) {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	running, ok := g.running[slot.token]
	if !ok {
		return
	}
	running.preempt = preempt
	g.running[slot.token] = running
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

func (g *GlobalDispatchGate) preemptProjectsLocked(preemptions []RunningProject) error {
	for _, preemption := range preemptions {
		if err := g.preemptProjectLocked(preemption); err != nil {
			return err
		}
	}
	return nil
}

func (g *GlobalDispatchGate) preemptProjectLocked(preemption RunningProject) error {
	for token, running := range g.running {
		if running.ProjectID != preemption.ProjectID || running.Priority != preemption.Priority || running.preempt == nil {
			continue
		}
		running.preempt()
		if err := g.global.ReleaseSlot(running.slot); err != nil && !errors.Is(err, ErrSlotNotHeld) {
			return err
		}
		delete(g.running, token)
		return nil
	}
	return nil
}

func (g *GlobalDispatchGate) reconcileSelectionsLocked() {
	for projectID, selected := range g.selected {
		ready, ok := g.ready[projectID]
		if !ok {
			delete(g.selected, projectID)
			continue
		}
		selected.ProjectCandidate = ready.ProjectCandidate
		selected.request = ready.request
		g.selected[projectID] = selected
	}
	if g.global.Mode() != ModeStrictPriority && g.hasPriorityRescueReadyLocked() {
		if g.hasUnselectedPriorityRescueReadyLocked() {
			for projectID := range g.selected {
				if g.priorityBypasses[projectID] < maxPriorityLaneBypasses {
					delete(g.selected, projectID)
				}
			}
		}
		return
	}

	bestPriority, ok := g.bestReadyPriorityLocked()
	if !ok {
		return
	}
	for projectID, selected := range g.selected {
		if selected.request.Priority > bestPriority {
			delete(g.selected, projectID)
		}
	}
}

func (g *GlobalDispatchGate) fillSelectionsLocked(ctx context.Context, now time.Time, reserveWhenFull bool) error {
	if err := g.pruneClosedReadyProjectsLocked(now); err != nil {
		return err
	}
	for {
		excluded := g.selectedProjectIDsLocked()
		projects, requests := g.readyProjectsForSelectionLocked(excluded, g.unreservedCapacityLocked())
		if len(projects) == 0 {
			if !reserveWhenFull || len(g.selected) > 0 {
				return nil
			}
			projects, requests = g.readyProjectsForSelectionLocked(excluded, -1)
			if len(projects) == 0 {
				return nil
			}
		}

		selection, err := g.global.SelectProject(ctx, ProjectSelectionRequest{
			Projects: projects,
			Running:  g.runningProjectsWithSelectionsLocked(),
			Now:      now,
		})
		if err != nil {
			if !reserveWhenFull || !errors.Is(err, ErrNoSlots) || len(g.selected) > 0 {
				if errors.Is(err, ErrNoSlots) {
					return nil
				}
				return err
			}
			reserved, reserveErr := g.global.SelectProject(ctx, ProjectSelectionRequest{
				Projects: projects,
				Now:      now,
			})
			if reserveErr != nil {
				return err
			}
			selection = reserved
		}

		req := requests[selection.Project.ID]
		g.selected[selection.Project.ID] = selectedProjectSlot{
			ProjectCandidate: selection.Project,
			request:          req,
			preemptions:      selection.Preemptions,
		}
	}
}

func (g *GlobalDispatchGate) bestStrictReadyProjectRankLocked() (int, bool) {
	best := 0
	found := false
	for _, ready := range g.ready {
		rank := priorityRank(ready.Priority)
		if !found || rank < best {
			best = rank
			found = true
		}
	}
	return best, found && g.global.Mode() == ModeStrictPriority
}

func (g *GlobalDispatchGate) readyProjectsForSelectionLocked(excluded map[string]struct{}, maxWeight int) ([]ProjectCandidate, map[string]SlotRequest) {
	if len(g.ready) == 0 {
		return nil, nil
	}

	// Priority orders selection; it does not gate it. Filtering the candidate
	// set down to only the best strict rank meant a merely-ready higher-priority
	// project excluded every lower-priority project from selection entirely,
	// even with capacity free. The selection loop already excludes projects it
	// has picked, so ranking alone fills slots highest-priority-first and then
	// keeps going down the list until capacity runs out.
	strictProjectRank, strict := g.bestStrictReadyProjectRankLocked()
	priorityRescue := false
	bestPriority := 0
	first := true
	for _, ready := range g.ready {
		if _, ok := excluded[ready.ID]; ok {
			continue
		}
		if strict && priorityRank(ready.Priority) != strictProjectRank {
			continue
		}
		if maxWeight >= 0 && ready.request.Weight > maxWeight {
			continue
		}
		if !strict && g.priorityBypasses[ready.ID] >= maxPriorityLaneBypasses {
			priorityRescue = true
		}
		if first || ready.request.Priority < bestPriority {
			bestPriority = ready.request.Priority
			first = false
		}
	}
	if first {
		return nil, nil
	}

	projects := make([]ProjectCandidate, 0, len(g.ready))
	requests := make(map[string]SlotRequest, len(g.ready))
	for _, ready := range g.ready {
		if _, ok := excluded[ready.ID]; ok {
			continue
		}
		if strict && priorityRank(ready.Priority) != strictProjectRank {
			continue
		}
		if maxWeight >= 0 && ready.request.Weight > maxWeight {
			continue
		}
		if priorityRescue && g.priorityBypasses[ready.ID] < maxPriorityLaneBypasses {
			continue
		}
		if !priorityRescue && ready.request.Priority != bestPriority {
			continue
		}
		projects = append(projects, ready.ProjectCandidate)
		requests[ready.ID] = ready.request
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].ID < projects[j].ID
	})
	return projects, requests
}

func (g *GlobalDispatchGate) decisionLocked(projectID string, req SlotRequest, reason string) DispatchGateDecision {
	stats := g.capacitySnapshotLocked(req.State)
	selectedProjectID, selectedReq, _ := g.decisionSelectionLocked(projectID)
	selectedState := selectedReq.State
	if reason == "" {
		reason = DispatchGateReasonSelectedProjectWaiting
	}

	return DispatchGateDecision{
		ProjectID:            projectID,
		PoolName:             g.poolName,
		Holders:              g.holderProjectIDsLocked(),
		State:                req.State,
		SelectedProjectID:    selectedProjectID,
		SelectedState:        selectedState,
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

func (g *GlobalDispatchGate) pruneClosedReadyProjectsLocked(now time.Time) error {
	for projectID, ready := range g.ready {
		status, err := ready.ActiveHoursStatus(now)
		if err != nil {
			return fmt.Errorf("evaluate project %q active hours: %w", projectID, err)
		}
		if status.Open {
			continue
		}
		delete(g.ready, projectID)
		delete(g.selected, projectID)
		delete(g.priorityBypasses, projectID)
		g.projectCycles[projectID] = projectCycleState{idle: true}
	}
	return nil
}

func (g *GlobalDispatchGate) recordPriorityLaneDispatchLocked(projectID string, priority int) {
	delete(g.priorityBypasses, projectID)
	if g.global.Mode() == ModeStrictPriority {
		return
	}
	for waitingID, ready := range g.ready {
		if waitingID == projectID || ready.request.Priority <= priority {
			continue
		}
		if _, selected := g.selected[waitingID]; selected {
			continue
		}
		if g.priorityBypasses[waitingID] < maxPriorityLaneBypasses {
			g.priorityBypasses[waitingID]++
		}
	}
}

func (g *GlobalDispatchGate) hasPriorityRescueReadyLocked() bool {
	for projectID := range g.ready {
		if g.priorityBypasses[projectID] >= maxPriorityLaneBypasses {
			return true
		}
	}
	return false
}

func (g *GlobalDispatchGate) hasUnselectedPriorityRescueReadyLocked() bool {
	for projectID := range g.ready {
		if g.priorityBypasses[projectID] < maxPriorityLaneBypasses {
			continue
		}
		if _, selected := g.selected[projectID]; !selected {
			return true
		}
	}
	return false
}

func (g *GlobalDispatchGate) hasHigherPriorityRunningProjectLocked(project ProjectCandidate) bool {
	projectRank := priorityRank(project.Priority)
	for _, running := range g.running {
		if running.ProjectID != project.ID && priorityRank(running.Priority) < projectRank {
			return true
		}
	}
	return false
}

func (g *GlobalDispatchGate) waitReasonLocked(req SlotRequest) string {
	_, selectedReq, ok := g.decisionSelectionLocked("")
	if ok && selectedReq.Priority < req.Priority {
		return DispatchGateReasonReservedForHigherPriority
	}
	return DispatchGateReasonSelectedProjectWaiting
}

func (g *GlobalDispatchGate) decisionSelectionLocked(projectID string) (string, SlotRequest, bool) {
	if projectID != "" {
		if selected, ok := g.selected[projectID]; ok {
			return projectID, selected.request, true
		}
	}
	if len(g.selected) == 0 {
		return "", SlotRequest{}, false
	}

	selectedIDs := make([]string, 0, len(g.selected))
	for projectID := range g.selected {
		selectedIDs = append(selectedIDs, projectID)
	}
	sort.Slice(selectedIDs, func(i, j int) bool {
		left := g.selected[selectedIDs[i]]
		right := g.selected[selectedIDs[j]]
		if left.request.Priority != right.request.Priority {
			return left.request.Priority < right.request.Priority
		}
		return selectedIDs[i] < selectedIDs[j]
	})
	selected := g.selected[selectedIDs[0]]
	return selectedIDs[0], selected.request, true
}

func (g *GlobalDispatchGate) capacitySnapshotLocked(state string) capacitySnapshot {
	if snapshotter, ok := g.global.(interface{ capacitySnapshot(string) capacitySnapshot }); ok {
		return snapshotter.capacitySnapshot(state)
	}
	return capacitySnapshot{}
}

func (g *GlobalDispatchGate) unreservedCapacityLocked() int {
	stats := g.capacitySnapshotLocked("")
	return nonNegativeInt(stats.globalCapacity - stats.globalUsed - g.selectedReservationWeightLocked())
}

func (g *GlobalDispatchGate) selectedReservationWeightLocked() int {
	weight := 0
	for _, selected := range g.selected {
		weight += selected.request.Weight
	}
	return weight
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

func (g *GlobalDispatchGate) bestReadyPriorityLocked() (int, bool) {
	bestPriority := 0
	found := false
	for _, ready := range g.ready {
		if !found || ready.request.Priority < bestPriority {
			bestPriority = ready.request.Priority
			found = true
		}
	}
	return bestPriority, found
}

func (g *GlobalDispatchGate) selectedProjectIDsLocked() map[string]struct{} {
	if len(g.selected) == 0 {
		return nil
	}
	ids := make(map[string]struct{}, len(g.selected))
	for projectID := range g.selected {
		ids[projectID] = struct{}{}
	}
	return ids
}

func (g *GlobalDispatchGate) runningProjectsWithSelectionsLocked() []RunningProject {
	projects := g.runningProjectsLocked()
	for _, selected := range g.selected {
		projects = appendRunningProjectWeight(projects, RunningProject{
			ProjectID:    selected.ID,
			Priority:     selected.Priority,
			State:        selected.request.State,
			SlotPriority: selected.request.Priority,
		}, selected.request.Weight)
	}
	sortRunningProjects(projects)
	return projects
}

func (g *GlobalDispatchGate) runningProjectsLocked() []RunningProject {
	projects := make([]RunningProject, 0, len(g.running))
	for _, project := range g.running {
		projects = appendRunningProjectWeight(projects, project.RunningProject, project.slot.Weight)
	}
	sortRunningProjects(projects)
	return projects
}

func appendRunningProjectWeight(projects []RunningProject, project RunningProject, weight int) []RunningProject {
	if weight <= 0 {
		weight = 1
	}
	for range weight {
		projects = append(projects, project)
	}
	return projects
}

func sortRunningProjects(projects []RunningProject) {
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].ProjectID != projects[j].ProjectID {
			return projects[i].ProjectID < projects[j].ProjectID
		}
		return projects[i].Priority < projects[j].Priority
	})
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
