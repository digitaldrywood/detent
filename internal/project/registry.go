package project

import (
	"sort"
	"sync"
)

type Registry struct {
	mu       sync.RWMutex
	projects map[ID]*Project
}

func NewRegistry() *Registry {
	return &Registry{
		projects: map[ID]*Project{},
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
	return nil
}

func (r *Registry) Get(id ID) (*Project, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	project, ok := r.projects[normalizeProjectID(id)]
	return project, ok
}

func (r *Registry) Delete(id ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = normalizeProjectID(id)
	if _, ok := r.projects[id]; !ok {
		return false
	}

	delete(r.projects, id)
	return true
}

func (r *Registry) List() []*Project {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]ID, 0, len(r.projects))
	for id := range r.projects {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})

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
