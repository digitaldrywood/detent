package project

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/store"
)

type policyAttemptStore struct {
	mu       sync.Mutex
	attempts []store.WorkAttempt
}

func (s *policyAttemptStore) StartWorkAttempt(_ context.Context, start store.WorkAttemptStart) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(len(s.attempts) + 1)
	s.attempts = append(s.attempts, store.WorkAttempt{ID: id, ProjectID: start.ProjectID, IssueID: start.IssueID, WorkerType: start.WorkerType, StartedAt: start.StartedAt, LeaseExpiresAt: start.LeaseExpiresAt, Status: store.WorkAttemptStatusActive})
	return id, nil
}

func (s *policyAttemptStore) WorkAttempt(_ context.Context, id int64) (store.WorkAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[id-1], nil
}

func (s *policyAttemptStore) RecordWorkAttemptHeartbeat(context.Context, store.WorkAttemptHeartbeat) error {
	return nil
}

func (s *policyAttemptStore) CompleteWorkAttempt(_ context.Context, completion store.WorkAttemptCompletion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[completion.AttemptID-1].Status = completion.Status
	return nil
}

func (s *policyAttemptStore) ListActiveWorkAttempts(_ context.Context, query store.WorkAttemptQuery) ([]store.WorkAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var active []store.WorkAttempt
	for _, attempt := range s.attempts {
		if attempt.ProjectID == query.ProjectID && attempt.Status == store.WorkAttemptStatusActive {
			active = append(active, attempt)
		}
	}
	return active, nil
}

func (s *policyAttemptStore) ListRecentTerminalWorkAttempts(context.Context, store.WorkAttemptHistoryQuery) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *policyAttemptStore) TimeoutExpiredWorkAttempts(context.Context, store.WorkAttemptTimeout) ([]store.WorkAttempt, error) {
	return nil, nil
}

func (s *policyAttemptStore) RecordSchedulerDecision(context.Context, store.SchedulerDecision) (int64, error) {
	return 1, nil
}

func (s *policyAttemptStore) ListRecentSchedulerDecisions(context.Context, store.SchedulerDecisionQuery) ([]store.SchedulerDecision, error) {
	return nil, nil
}
