package project

import (
	"slices"
	"sync"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type HealthStatus string

const (
	HealthStatusInitializing HealthStatus = "initializing"
	HealthStatusReady        HealthStatus = "ready"
	HealthStatusDegraded     HealthStatus = "degraded"
	HealthStatusPaused       HealthStatus = "paused"
)

type Health struct {
	Project      globalconfig.Project
	Status       HealthStatus
	LastError    string
	LastErrorAt  time.Time
	NextRetryAt  time.Time
	RetryStopped bool
}

type pendingProject struct {
	config       globalconfig.Project
	runtimeError RuntimeError
}

type Registry struct {
	mu       sync.RWMutex
	projects map[ID]*Project
	pending  map[ID]pendingProject
}

func NewRegistry() *Registry {
	return &Registry{
		projects: map[ID]*Project{},
		pending:  map[ID]pendingProject{},
	}
}

func (r *Registry) Set(project *Project) error {
	if project == nil {
		return ErrMissingProject
	}

	id := normalizeProjectID(project.ID())
	if id == "" {
		return ErrMissingProjectID
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.projects[id] = project
	delete(r.pending, id)
	return nil
}

func (r *Registry) SetPending(cfg globalconfig.Project, runtimeErr RuntimeError) error {
	id := normalizeProjectID(ID(cfg.ID))
	if id == "" {
		return ErrMissingProjectID
	}
	cfg.ID = string(id)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.projects[id]; ok {
		return ErrProjectExists
	}
	r.pending[id] = pendingProject{config: cfg, runtimeError: runtimeErr}
	return nil
}

func (r *Registry) Pending(id ID) (Health, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pending, ok := r.pending[normalizeProjectID(id)]
	if !ok {
		return Health{}, false
	}
	return pendingHealth(pending), true
}

func (r *Registry) Get(id ID) (*Project, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	project, ok := r.projects[normalizeProjectID(id)]
	if project == nil {
		return nil, false
	}
	return project, ok
}

func (r *Registry) Delete(id ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = normalizeProjectID(id)
	_, projectExists := r.projects[id]
	_, pendingExists := r.pending[id]
	if !projectExists && !pendingExists {
		return false
	}

	delete(r.projects, id)
	delete(r.pending, id)
	return true
}

func (r *Registry) List() []*Project {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]ID, 0, len(r.projects))
	for id := range r.projects {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	projects := make([]*Project, 0, len(ids))
	for _, id := range ids {
		projects = append(projects, r.projects[id])
	}
	return projects
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.projects)
}

func (r *Registry) Health() []Health {
	r.mu.RLock()
	projects := make(map[ID]*Project, len(r.projects))
	for id, trackedProject := range r.projects {
		projects[id] = trackedProject
	}
	pending := make(map[ID]pendingProject, len(r.pending))
	for id, pendingProject := range r.pending {
		pending[id] = pendingProject
	}
	r.mu.RUnlock()

	ids := make([]ID, 0, len(projects)+len(pending))
	for id := range projects {
		ids = append(ids, id)
	}
	for id := range pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	health := make([]Health, 0, len(ids))
	for _, id := range ids {
		if trackedProject := projects[id]; trackedProject != nil {
			health = append(health, projectHealth(trackedProject))
			continue
		}
		health = append(health, pendingHealth(pending[id]))
	}
	return health
}

func projectHealth(trackedProject *Project) Health {
	status := HealthStatusInitializing
	if trackedProject.Paused() {
		status = HealthStatusPaused
	} else if trackedProject.Running() {
		status = HealthStatusReady
	}
	runtimeErr := trackedProject.RuntimeError()
	if status == HealthStatusInitializing && runtimeErr.Message != "" {
		status = HealthStatusDegraded
	}
	return Health{
		Project:      trackedProject.Config(),
		Status:       status,
		LastError:    runtimeErr.Message,
		LastErrorAt:  runtimeErr.At,
		NextRetryAt:  runtimeErr.NextRetryAt,
		RetryStopped: runtimeErr.Terminal,
	}
}

func pendingHealth(pending pendingProject) Health {
	return Health{
		Project:      pending.config,
		Status:       HealthStatusDegraded,
		LastError:    pending.runtimeError.Message,
		LastErrorAt:  pending.runtimeError.At,
		NextRetryAt:  pending.runtimeError.NextRetryAt,
		RetryStopped: pending.runtimeError.Terminal,
	}
}
