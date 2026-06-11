package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type ProjectDispatchGate interface {
	MarkReady(ProjectCandidate)
	MarkIdle(string)
	TryAcquire(context.Context, ProjectCandidate, SlotRequest, time.Time) (Slot, bool, error)
	SetPreempt(Slot, func())
	Release(Slot) error
}

type runningProjectSlot struct {
	RunningProject
	slot    Slot
	preempt func()
}

type GlobalDispatchGate struct {
	global GlobalScheduler

	mu          sync.Mutex
	ready       map[string]ProjectCandidate
	running     map[uint64]runningProjectSlot
	selected    string
	preemptions []RunningProject
}

func NewGlobalDispatchGate(global GlobalScheduler) *GlobalDispatchGate {
	return &GlobalDispatchGate{
		global:  global,
		ready:   map[string]ProjectCandidate{},
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

	g.ready[project.ID] = project
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
		g.preemptions = nil
	}
}

func (g *GlobalDispatchGate) TryAcquire(
	ctx context.Context,
	project ProjectCandidate,
	req SlotRequest,
	now time.Time,
) (Slot, bool, error) {
	if g == nil || g.global == nil {
		return Slot{}, true, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	project, ok := normalizeSingleProjectCandidate(project)
	if !ok {
		return Slot{}, false, ErrNoCandidates
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	select {
	case <-ctx.Done():
		return Slot{}, false, ctx.Err()
	default:
	}

	g.ready[project.ID] = project
	if g.selected == "" {
		selection, err := g.global.SelectProject(ctx, ProjectSelectionRequest{
			Projects: g.readyProjectsLocked(),
			Running:  g.runningProjectsLocked(),
			Now:      now,
		})
		if err != nil {
			if errors.Is(err, ErrNoSlots) {
				return Slot{}, false, nil
			}
			return Slot{}, false, err
		}
		g.selected = selection.Project.ID
		g.preemptions = selection.Preemptions
	}
	if g.selected != project.ID {
		return Slot{}, false, nil
	}
	if err := g.preemptProjectsLocked(g.preemptions); err != nil {
		g.selected = ""
		g.preemptions = nil
		return Slot{}, false, err
	}

	slot, err := g.global.RequestSlot(ctx, req)
	if err != nil {
		if errors.Is(err, ErrNoSlots) {
			return Slot{}, false, nil
		}
		g.selected = ""
		g.preemptions = nil
		return Slot{}, false, err
	}
	if err := g.global.RecordProjectDispatch(ctx, ProjectDispatch{
		ProjectID:    project.ID,
		Weight:       project.Weight,
		DispatchedAt: now,
	}); err != nil {
		g.selected = ""
		g.preemptions = nil
		return Slot{}, false, errors.Join(err, g.global.ReleaseSlot(slot))
	}

	delete(g.ready, project.ID)
	g.selected = ""
	g.preemptions = nil
	g.running[slot.token] = runningProjectSlot{
		RunningProject: RunningProject{
			ProjectID: project.ID,
			Priority:  project.Priority,
		},
		slot: slot,
	}
	return slot, true, nil
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

func (g *GlobalDispatchGate) readyProjectsLocked() []ProjectCandidate {
	projects := make([]ProjectCandidate, 0, len(g.ready))
	for _, project := range g.ready {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].ID < projects[j].ID
	})
	return projects
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

func normalizeSingleProjectCandidate(project ProjectCandidate) (ProjectCandidate, bool) {
	projects := normalizeProjectCandidates([]ProjectCandidate{project})
	if len(projects) == 0 {
		return ProjectCandidate{}, false
	}
	return projects[0], true
}
