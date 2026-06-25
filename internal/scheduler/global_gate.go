package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	DispatchGateReasonGranted                   = "granted"
	DispatchGateReasonGlobalCapacityFull        = "global_capacity_full"
	DispatchGateReasonReservedForHigherPriority = "reserved_for_higher_priority_state"
	DispatchGateReasonSelectedProjectWaiting    = "selected_project_waiting"
)

type ProjectDispatchGate interface {
	MarkReady(ProjectCandidate)
	MarkIdle(string)
	TryAcquire(context.Context, ProjectCandidate, SlotRequest, time.Time) (Slot, bool, error)
	SetPreempt(Slot, func())
	Release(Slot) error
}

type DispatchGateDecision struct {
	ProjectID            string
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

type GlobalDispatchGate struct {
	global GlobalScheduler

	mu          sync.Mutex
	ready       map[string]readyProjectSlot
	running     map[uint64]runningProjectSlot
	selected    string
	selectedReq SlotRequest
	preemptions []RunningProject
}

func NewGlobalDispatchGate(global GlobalScheduler) *GlobalDispatchGate {
	return &GlobalDispatchGate{
		global:  global,
		ready:   map[string]readyProjectSlot{},
		running: map[uint64]runningProjectSlot{},
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

	ready := g.ready[project.ID]
	ready.ProjectCandidate = project
	g.ready[project.ID] = ready
}

func (g *GlobalDispatchGate) MarkIdle(projectID string) {
	if g == nil {
		return
	}
	projectID = normalizeProjectID(projectID)
	if projectID == "" {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	delete(g.ready, projectID)
	if g.selected == projectID {
		g.selected = ""
		g.selectedReq = SlotRequest{}
		g.preemptions = nil
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
		return Slot{}, true, DispatchGateDecision{Reason: DispatchGateReasonGranted}, nil
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

	g.ready[project.ID] = readyProjectSlot{ProjectCandidate: project, request: req}
	if g.selected != "" {
		selected, ok := g.ready[g.selected]
		if !ok {
			g.clearSelectionLocked()
		} else {
			g.selectedReq = selected.request
			if g.hasHigherPriorityReadyProjectLocked(selected.request.Priority) {
				g.clearSelectionLocked()
			}
		}
	}
	if g.selected == "" {
		selection, selectedReq, err := g.selectReadyProjectLocked(ctx, now, true)
		if err != nil {
			if errors.Is(err, ErrNoSlots) {
				decision := g.decisionLocked(project.ID, req, DispatchGateReasonGlobalCapacityFull)
				return Slot{}, false, decision, nil
			}
			return Slot{}, false, DispatchGateDecision{}, err
		}
		g.selected = selection.Project.ID
		g.selectedReq = selectedReq
		g.preemptions = selection.Preemptions
	}
	if g.selected != project.ID {
		reason := DispatchGateReasonSelectedProjectWaiting
		if selected, ok := g.ready[g.selected]; ok && selected.request.Priority < req.Priority {
			reason = DispatchGateReasonReservedForHigherPriority
		}
		return Slot{}, false, g.decisionLocked(project.ID, req, reason), nil
	}
	if err := g.preemptProjectsLocked(g.preemptions); err != nil {
		g.clearSelectionLocked()
		return Slot{}, false, DispatchGateDecision{}, err
	}

	slot, err := g.global.RequestSlot(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNoSlots) {
			decision := g.decisionLocked(project.ID, req, DispatchGateReasonGlobalCapacityFull)
			return Slot{}, false, decision, nil
		}
		g.clearSelectionLocked()
		return Slot{}, false, DispatchGateDecision{}, err
	}
	if err := g.global.RecordProjectDispatch(ctx, ProjectDispatch{
		ProjectID:    project.ID,
		Weight:       project.Weight,
		DispatchedAt: now,
	}); err != nil {
		g.clearSelectionLocked()
		return Slot{}, false, DispatchGateDecision{}, errors.Join(err, g.global.ReleaseSlot(slot))
	}

	decision := g.decisionLocked(project.ID, req, DispatchGateReasonGranted)
	delete(g.ready, project.ID)
	g.clearSelectionLocked()
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

	if _, ok := g.running[slot.token]; !ok {
		return nil
	}
	if err := g.global.ReleaseSlot(slot); err != nil {
		return err
	}
	delete(g.running, slot.token)
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

func (g *GlobalDispatchGate) selectReadyProjectLocked(
	ctx context.Context,
	now time.Time,
	reserveWhenFull bool,
) (ProjectSelection, SlotRequest, error) {
	projects, requests := g.readyProjectsForSelectionLocked()
	if len(projects) == 0 {
		return ProjectSelection{}, SlotRequest{}, ErrNoCandidates
	}

	selection, err := g.global.SelectProject(ctx, ProjectSelectionRequest{
		Projects: projects,
		Running:  g.runningProjectsLocked(),
		Now:      now,
	})
	if err == nil {
		return selection, requests[selection.Project.ID], nil
	}
	if !reserveWhenFull || !errors.Is(err, ErrNoSlots) {
		return ProjectSelection{}, SlotRequest{}, err
	}

	selection, reserveErr := g.global.SelectProject(ctx, ProjectSelectionRequest{
		Projects: projects,
		Now:      now,
	})
	if reserveErr != nil {
		return ProjectSelection{}, SlotRequest{}, err
	}
	return selection, requests[selection.Project.ID], nil
}

func (g *GlobalDispatchGate) readyProjectsForSelectionLocked() ([]ProjectCandidate, map[string]SlotRequest) {
	if len(g.ready) == 0 {
		return nil, nil
	}

	bestPriority := 0
	first := true
	for _, ready := range g.ready {
		if first || ready.request.Priority < bestPriority {
			bestPriority = ready.request.Priority
			first = false
		}
	}

	projects := make([]ProjectCandidate, 0, len(g.ready))
	requests := make(map[string]SlotRequest, len(g.ready))
	for _, ready := range g.ready {
		if ready.request.Priority != bestPriority {
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
	selectedProjectID := g.selected
	selectedState := g.selectedReq.State
	if selected, ok := g.ready[selectedProjectID]; ok {
		selectedState = selected.request.State
	}
	if reason == "" {
		reason = DispatchGateReasonSelectedProjectWaiting
	}

	return DispatchGateDecision{
		ProjectID:            projectID,
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

func (g *GlobalDispatchGate) hasHigherPriorityReadyProjectLocked(priority int) bool {
	for _, ready := range g.ready {
		if ready.request.Priority < priority {
			return true
		}
	}
	return false
}

func (g *GlobalDispatchGate) clearSelectionLocked() {
	g.selected = ""
	g.selectedReq = SlotRequest{}
	g.preemptions = nil
}

func (g *GlobalDispatchGate) runningProjectsLocked() []RunningProject {
	projects := make([]RunningProject, 0, len(g.running))
	for _, project := range g.running {
		projects = append(projects, project.RunningProject)
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].ProjectID != projects[j].ProjectID {
			return projects[i].ProjectID < projects[j].ProjectID
		}
		return projects[i].Priority < projects[j].Priority
	})
	return projects
}

func normalizeSlotRequest(req SlotRequest) (SlotRequest, error) {
	slot, err := normalizeRequest(req)
	if err != nil {
		return SlotRequest{}, err
	}
	return SlotRequest{
		State:    slot.State,
		Host:     slot.Host,
		Weight:   slot.Weight,
		Priority: slot.Priority,
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
