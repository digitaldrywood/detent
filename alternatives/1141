package api

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type RunnerSessions struct {
	*NativeClient
	mu        sync.Mutex
	workersMu sync.Mutex
	keys      map[int64]string
	workers   map[int64]store.WorkerProcessRegistration
}

var _ runner.SessionStore = (*RunnerSessions)(nil)
var _ store.AgentResumeStore = (*RunnerSessions)(nil)
var _ store.SessionProgressStore = (*RunnerSessions)(nil)
var _ store.SessionPolicyStore = (*RunnerSessions)(nil)

func NewRunnerSessions(client *NativeClient) *RunnerSessions {
	return &RunnerSessions{NativeClient: client}
}

func (s *RunnerSessions) BindExecution(ctx context.Context, execution runner.Execution) context.Context {
	if execution == nil {
		return ctx
	}
	return WithMutationAuthority(ctx, s.base(), execution.Recovery().Lease, func() error { return context.Cause(ctx) })
}

func sessionReadError(err error) error {
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		return errors.Join(store.ErrNotFound, err)
	}
	return err
}

func (s *RunnerSessions) snapshot(ctx context.Context, id int64) (runnersession.Snapshot, error) {
	var result runnersession.Snapshot
	err := s.client.Request(ctx, http.MethodGet, s.base()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &result)
	return result, sessionReadError(err)
}

func (s *RunnerSessions) mutate(ctx context.Context, request runnersession.Mutation) (runnersession.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
	if !ok || authority.scope != s.base() || authority.lease.ID == "" {
		return runnersession.Snapshot{}, runner.ErrExecutionAuthorityUnavailable
	}
	request.Key = uuid.NewString()
	request.LeaseID, request.FencingToken = authority.lease.ID, authority.lease.FencingToken
	if request.SessionID > 0 {
		current, err := s.snapshot(ctx, request.SessionID)
		if err != nil {
			return current, err
		}
		request.ExpectedRevision = current.Revision
	}
	var result runnersession.Snapshot
	err := s.client.Request(ctx, http.MethodPost, s.base()+"/runner-sessions", request, &result)
	var api *APIError
	if errors.As(err, &api) && (api.Status == http.StatusUnauthorized || api.Status == http.StatusForbidden || api.Code == "stale_fencing_token" || api.Code == "policy_mismatch") {
		err = errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	return result, err
}

func (s *RunnerSessions) StartSession(ctx context.Context, start store.SessionStart) (int64, error) {
	key := rand.Text() + rand.Text()
	value, err := s.mutate(ctx, runnersession.Mutation{Start: &start, ProcessKey: key})
	if err != nil {
		return value.ID, err
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	if s.keys == nil {
		s.keys = make(map[int64]string)
	}
	s.keys[value.ID] = key
	return value.ID, nil
}

func (s *RunnerSessions) processKey(id int64) string {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	return s.keys[id]
}
func (s *RunnerSessions) FinishSession(ctx context.Context, id int64, finish store.SessionFinish) error {
	_, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Finish: &finish})
	return err
}
func (s *RunnerSessions) RecordUsageEvent(ctx context.Context, usage store.UsageEvent) (int64, error) {
	value, err := s.mutate(ctx, runnersession.Mutation{SessionID: usage.SessionID, Usage: &usage})
	return value.ReceiptID, err
}
func (s *RunnerSessions) UpdateSessionIdentity(ctx context.Context, id int64, identity agentidentity.Identity) error {
	_, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Identity: &identity})
	return err
}
func (s *RunnerSessions) UpdateSessionProviderIdentity(ctx context.Context, id int64, provider store.SessionProviderIdentity) error {
	_, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Provider: &provider})
	return err
}
func (s *RunnerSessions) UpdateSessionResumeState(ctx context.Context, id int64, resume store.SessionResumeState) error {
	_, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Resume: &resume})
	return err
}
func (s *RunnerSessions) SaveSessionProgress(ctx context.Context, id int64, progress store.SessionProgress) error {
	_, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Progress: &progress})
	return err
}
func (s *RunnerSessions) SessionProgress(ctx context.Context, id int64) (store.SessionProgress, error) {
	value, err := s.snapshot(ctx, id)
	if err != nil {
		return store.SessionProgress{}, err
	}
	if value.Progress == nil {
		return store.SessionProgress{}, store.ErrNotFound
	}
	return *value.Progress, nil
}
func (s *RunnerSessions) SessionPolicy(ctx context.Context, _ string, id int64) (policy.Descriptor, error) {
	value, err := s.snapshot(ctx, id)
	return value.Policy, err
}
func (s *RunnerSessions) RecordWorkflowPhaseEvent(ctx context.Context, phase store.WorkflowPhaseEvent) (int64, error) {
	value, err := s.mutate(ctx, runnersession.Mutation{SessionID: phase.SessionID, Phase: &phase})
	return value.ReceiptID, err
}
func (s *RunnerSessions) SaveWorkflowActivityProfile(ctx context.Context, id int64, phase store.WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	value, err := s.mutate(ctx, runnersession.Mutation{SessionID: profile.SessionID, Profile: &runnersession.Activity{Phase: phase, Profile: profile}})
	return value.ReceiptID, err
}
func (s *RunnerSessions) LatestAgentResumeState(ctx context.Context, lookup store.AgentResumeLookup) (store.AgentResumeState, error) {
	return s.latestResume(ctx, lookup, false)
}

func (s *RunnerSessions) latestResume(ctx context.Context, lookup store.AgentResumeLookup, current bool) (store.AgentResumeState, error) {
	var value runnersession.Snapshot
	request := runnersession.ResumeLookup{Lookup: lookup, CurrentAttempt: current}
	if authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority); ok && authority.scope == s.base() {
		request.LeaseID, request.FencingToken = authority.lease.ID, authority.lease.FencingToken
	}
	if err := s.client.Request(ctx, http.MethodPost, s.base()+"/runner-sessions/find", request, &value); err != nil {
		return store.AgentResumeState{}, sessionReadError(err)
	}
	result := store.AgentResumeState{RuntimeIdentity: value.Identity, DetentSessionID: value.ID, ProviderThreadID: value.Provider.ThreadID, ProviderSessionID: value.Provider.SessionID, RequestedModel: value.Start.RequestedModel, Model: value.Start.Model, AgentBackendID: value.Start.AgentBackendID, AgentBackendKind: value.Start.AgentBackendKind, AgentRole: value.Start.AgentRole, Orphaned: value.Finish == nil}
	if value.Finish != nil {
		result.CompletedAt = value.Finish.CompletedAt
	}
	return result, nil
}
func (s *RunnerSessions) LatestIssueAgentResumeState(ctx context.Context, issue store.IssueIdentity) (store.AgentResumeState, error) {
	return s.latestResume(ctx, store.AgentResumeLookup{IssueID: issue.IssueID}, true)
}
func (s *RunnerSessions) UpdateSessionWorkerProcess(ctx context.Context, id int64, process store.WorkerProcessRegistration) error {
	s.workersMu.Lock()
	if s.workers == nil {
		s.workers = make(map[int64]store.WorkerProcessRegistration)
	}
	s.workers[id] = process
	key := s.keys[id]
	s.workersMu.Unlock()
	_, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Worker: &process, ProcessKey: key})
	return err
}
func (s *RunnerSessions) ListActiveWorkerProcesses(ctx context.Context) ([]store.WorkerProcess, error) {
	result, err := s.NativeClient.runnerProcesses(ctx, "")
	if err != nil {
		return nil, err
	}
	if result.More {
		return nil, errors.New("Hub process registrations are incomplete")
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	workers := make([]store.WorkerProcess, 0, len(result.Workers))
	for _, worker := range result.Workers {
		local, ok := s.workers[worker.SessionID]
		if !ok {
			continue
		}
		worker.WorkerProcessIdentity, worker.CleanupRoot, worker.CleanupPath = local.WorkerProcessIdentity, local.CleanupRoot, local.CleanupPath
		workers = append(workers, worker)
	}
	return workers, nil
}
func (s *RunnerSessions) MarkSessionWorkerProcessReaped(ctx context.Context, id int64, reap store.WorkerProcessReap) error {
	if _, err := s.mutate(ctx, runnersession.Mutation{SessionID: id, Reaped: &reap, ProcessKey: s.processKey(id)}); err != nil {
		return err
	}
	s.workersMu.Lock()
	delete(s.workers, id)
	s.workersMu.Unlock()
	return nil
}

func (c *NativeClient) runnerProcesses(ctx context.Context, attempt string) (runnersession.Processes, error) {
	var result runnersession.Processes
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/runner-processes?attempt="+url.QueryEscape(attempt), nil, &result)
	return result, err
}

func (c *NativeClient) ReleasedProvidersStopped(ctx context.Context, attempt string) (bool, error) {
	result, err := c.runnerProcesses(ctx, attempt)
	if err != nil {
		return false, err
	}
	if result.More || result.Unobserved {
		return false, nil
	}
	identities := make([]procgroup.Identity, 0, len(result.Workers))
	for _, worker := range result.Workers {
		identities = append(identities, procgroup.Identity{PID: worker.PID, GroupID: worker.GroupID, StartedAt: worker.StartedAt})
	}
	observations, err := procgroup.Observe(identities)
	if err != nil {
		return false, err
	}
	if len(observations) != len(identities) {
		return false, nil
	}
	for _, observed := range observations {
		if !observed.GroupObserved || observed.Alive || observed.Stale {
			return false, nil
		}
	}
	return true, nil
}
