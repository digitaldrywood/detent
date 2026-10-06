package hubclient

import (
	"context"
	"maps"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) nativeProject(name string) *NativeConnector {
	s.nativeProjectsMu.RLock()
	defer s.nativeProjectsMu.RUnlock()
	return s.nativeProjects[name]
}

func (s *Scheduler) nativeProjectSnapshot() map[string]*NativeConnector {
	s.nativeProjectsMu.RLock()
	defer s.nativeProjectsMu.RUnlock()
	return maps.Clone(s.nativeProjects)
}

func (s *Scheduler) SetNativeProjects(projects map[string]string) error {
	s.nativeProjectsMu.Lock()
	defer s.nativeProjectsMu.Unlock()
	next := make(map[string]*NativeConnector, len(projects))
	for name, id := range projects {
		if current := s.nativeProjects[name]; current != nil && current.client.project == tracker.ProjectID(id) {
			next[name] = current
			continue
		}
		native, err := s.client.Native(s.organizationID, tracker.ProjectID(id))
		if err != nil {
			return err
		}
		native.githubBatch = func(ctx context.Context, task tracker.GitHubBatchTask) error {
			return s.processGitHubBatch(ctx, native, task)
		}
		next[name] = &NativeConnector{client: native}
	}
	s.nativeProjects = next
	return nil
}
