package cli

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type memorySession struct {
	start    store.SessionStart
	process  store.WorkerProcessRegistration
	finished bool
	progress *store.SessionProgress
	reap     *store.WorkerProcessReap
}

type memorySessions struct {
	mu       sync.Mutex
	sessions []*memorySession
}

func (s *memorySessions) StartSession(_ context.Context, start store.SessionStart) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, &memorySession{start: start})
	return int64(len(s.sessions)), nil
}

func (s *memorySessions) session(id int64) (*memorySession, error) {
	if id < 1 || id > int64(len(s.sessions)) {
		return nil, store.ErrNotFound
	}
	return s.sessions[id-1], nil
}

func (s *memorySessions) FinishSession(_ context.Context, id int64, _ store.SessionFinish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.session(id)
	if err != nil {
		return err
	}
	session.finished = true
	return nil
}

func (s *memorySessions) RecordUsageEvent(context.Context, store.UsageEvent) (int64, error) {
	return 0, nil
}

func (s *memorySessions) UpdateSessionWorkerProcess(_ context.Context, id int64, process store.WorkerProcessRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.session(id)
	if err != nil {
		return err
	}
	session.process = process
	return nil
}

func (s *memorySessions) ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var processes []store.WorkerProcess
	for index, session := range s.sessions {
		if session.reap != nil || session.process.PID <= 0 && (session.process.CleanupRoot == "" || session.process.CleanupPath == "") {
			continue
		}
		processes = append(processes, store.WorkerProcess{
			Role:                  session.start.AgentRole,
			SessionID:             int64(index + 1),
			IssueID:               session.start.IssueID,
			Identifier:            session.start.Identifier,
			IssueURL:              session.start.IssueURL,
			WorkerProcessIdentity: session.process.WorkerProcessIdentity,
			CleanupRoot:           session.process.CleanupRoot,
			CleanupPath:           session.process.CleanupPath,
		})
	}
	return processes, nil
}

func (s *memorySessions) MarkSessionWorkerProcessReaped(_ context.Context, id int64, reap store.WorkerProcessReap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.session(id)
	if err != nil {
		return err
	}
	session.reap = &reap
	return nil
}

func (s *memorySessions) reaped(id int64) (store.WorkerProcessReap, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.session(id)
	if err != nil || session.reap == nil {
		return store.WorkerProcessReap{}, false
	}
	return *session.reap, true
}

func (s *memorySessions) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := 0
	for _, session := range s.sessions {
		if !session.finished {
			open++
		}
	}
	return open
}

func (s *memorySessions) UpdateSessionIdentity(context.Context, int64, agentidentity.Identity) error {
	return nil
}

func (s *memorySessions) UpdateSessionProviderIdentity(context.Context, int64, store.SessionProviderIdentity) error {
	return nil
}

func (s *memorySessions) UpdateSessionResumeState(context.Context, int64, store.SessionResumeState) error {
	return nil
}

func (s *memorySessions) SaveSessionProgress(_ context.Context, id int64, progress store.SessionProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.session(id)
	if err != nil {
		return err
	}
	session.progress = &progress
	return nil
}

func (s *memorySessions) SessionProgress(_ context.Context, id int64) (store.SessionProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.session(id)
	if err != nil || session.progress == nil {
		return store.SessionProgress{}, store.ErrNotFound
	}
	return *session.progress, nil
}

func (s *memorySessions) RecordWorkflowPhaseEvent(context.Context, store.WorkflowPhaseEvent) (int64, error) {
	return 0, nil
}

func (s *memorySessions) SaveWorkflowActivityProfile(context.Context, int64, store.WorkflowPhaseEvent, workflowmetrics.ActivityProfile) (int64, error) {
	return 0, nil
}

func (s *memorySessions) LatestAgentResumeState(context.Context, store.AgentResumeLookup) (store.AgentResumeState, error) {
	return store.AgentResumeState{}, store.ErrNotFound
}

func (s *memorySessions) LatestIssueAgentResumeState(context.Context, store.IssueIdentity) (store.AgentResumeState, error) {
	return store.AgentResumeState{}, store.ErrNotFound
}
