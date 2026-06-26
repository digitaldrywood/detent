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

type selectedProjectSlot struct {
	ProjectCandidate
	request     SlotRequest
	preemptions []RunningProject
}

type GlobalDispatchGate struct {
	global GlobalScheduler

	mu       sync.Mutex
	ready    map[string]readyProjectSlot
	running  map[uint64]runningProjectSlot
	selected map[string]selectedProjectSlot
}

func NewGlobalDispatchGate(global GlobalScheduler) *GlobalDispatchGate {
	return &GlobalDispatchGate{
		global:   global,
		ready:    map[string]readyProjectSlot{},
		running:  map[uint64]runningProjectSlot{},
		selected: map[string]selectedProjectSlot{},
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
	delete(g.selected, projectID)
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
	g.reconcileSelectionsLocked()
	if err := g.fillSelectionsLocked(ctx, now, true); err != nil {
		if errors.Is(err, ErrNoSlots) {
			decision := g.decisionLocked(project.ID, req, DispatchGateReasonGlobalCapacityFull)
			return Slot{}, false, decision, nil
		}
		return Slot{}, false, DispatchGateDecision{}, err
	}

	selected, selectedOK := g.selected[project.ID]
	if !selectedOK {
		reason := g.waitReasonLocked(req)
		return Slot{}, false, g.decisionLocked(project.ID, req, reason), nil
	}
	if err := g.preemptProjectsLocked(selected.preemptions); err != nil {
		delete(g.selected, project.ID)
		return Slot{}, false, DispatchGateDecision{}, err
	}

	slot, err := g.global.RequestSlot(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNoSlots) {
			decision := g.decisionLocked(project.ID, req, DispatchGateReasonGlobalCapacityFull)
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

func (g *GlobalDispatchGate) readyProjectsForSelectionLocked(excluded map[string]struct{}, maxWeight int) ([]ProjectCandidate, map[string]SlotRequest) {
	if len(g.ready) == 0 {
		return nil, nil
	}

	bestPriority := 0
	first := true
	for _, ready := range g.ready {
		if _, ok := excluded[ready.ID]; ok {
			continue
		}
		if maxWeight >= 0 && ready.request.Weight > maxWeight {
			continue
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
		if maxWeight >= 0 && ready.request.Weight > maxWeight {
			continue
		}
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
	selectedProjectID, selectedReq, _ := g.decisionSelectionLocked(projectID)
	selectedState := selectedReq.State
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
