package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type RunnerSessions struct {
	*NativeClient

	mu               sync.Mutex
	workersMu        sync.Mutex
	keys             map[int64]string
	workers          map[int64]store.WorkerProcessRegistration
	acknowledged     map[int64]acknowledgedSessionMetadata
	lastRevision     acknowledgedSessionRevision
	pendingMutations map[int64]chan struct{}

	workerStartSecret string
}

type acknowledgedSessionRevision struct {
	sessionID    int64
	revision     int64
	leaseID      tracker.LeaseID
	fencingToken tracker.FencingToken
}

func (r acknowledgedSessionRevision) matches(id int64, authority nativeMutationAuthority) bool {
	return id > 0 && r.sessionID == id && r.revision > 0 && r.leaseID == authority.lease.ID && r.fencingToken == authority.lease.FencingToken
}

type acknowledgedSessionMetadata struct {
	leaseID      tracker.LeaseID
	fencingToken tracker.FencingToken
	provider     store.SessionProviderIdentity
	worker       *store.WorkerProcessRegistration
	processKey   string
}

func (m acknowledgedSessionMetadata) matches(request runnersession.Mutation, authority nativeMutationAuthority) bool {
	if m.leaseID != authority.lease.ID || m.fencingToken != authority.lease.FencingToken {
		return false
	}
	if request.Start != nil || request.Finish != nil || request.Identity != nil || request.Resume != nil || request.Progress != nil || request.Phase != nil || request.Profile != nil || request.Usage != nil || request.Reaped != nil {
		return false
	}
	if provider := request.Provider; provider != nil && request.Worker == nil {
		return (provider.ThreadID == "" || provider.ThreadID == m.provider.ThreadID) && (provider.SessionID == "" || provider.SessionID == m.provider.SessionID)
	}
	if worker := request.Worker; worker != nil && request.Provider == nil && m.worker != nil && m.processKey == request.ProcessKey {
		return worker.PID == m.worker.PID && worker.GroupID == m.worker.GroupID && worker.StartKey == m.worker.StartKey && worker.StartedAt.Equal(m.worker.StartedAt) && worker.CleanupRoot == m.worker.CleanupRoot && worker.CleanupPath == m.worker.CleanupPath
	}
	return false
}

var _ runner.SessionStore = (*RunnerSessions)(nil)
var _ store.AgentResumeStore = (*RunnerSessions)(nil)
var _ store.SessionProgressStore = (*RunnerSessions)(nil)
var _ store.SessionPolicyStore = (*RunnerSessions)(nil)

func NewRunnerSessions(client *NativeClient) *RunnerSessions {
	return &RunnerSessions{NativeClient: client, workerStartSecret: rand.Text() + rand.Text()}
}

func (s *RunnerSessions) BindExecution(ctx context.Context, execution runner.Execution) context.Context {
	if execution == nil {
		return ctx
	}
	lease, scope := execution.Recovery().Lease, s.base()
	cause := func() error { return context.Cause(ctx) }
	authority := nativeMutationAuthority{scope: scope, lease: lease, cancellation: cause, guard: requestGuard{done: ctx.Done(), check: cause}}
	if inherited, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority); ok &&
		inherited.scope == scope && inherited.lease.WorkItemID == lease.WorkItemID &&
		inherited.lease.ID == lease.ID && inherited.lease.FencingToken == lease.FencingToken {
		authority.guard = inherited.guard
		authority.cancellation = inherited.cancellation
		authority.retry = inherited.retry
		authority.publications = inherited.publications
	}
	if owner, ok := execution.(interface {
		RetryPhasePublication(context.Context, func() error) error
	}); ok {
		authority.retry = owner.RetryPhasePublication
	}
	return context.WithValue(ctx, nativeMutationAuthorityKey{}, authority)
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

func (s *RunnerSessions) beginMutation(ctx context.Context, sessionID int64) (func(), error) {
	if sessionID == 0 {
		return func() {}, nil
	}
	for {
		if err := nativeEffectError(ctx); err != nil {
			return nil, err
		}
		s.mu.Lock()
		current := s.pendingMutations[sessionID]
		if current == nil {
			current = make(chan struct{})
			if s.pendingMutations == nil {
				s.pendingMutations = make(map[int64]chan struct{})
			}
			s.pendingMutations[sessionID] = current
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				delete(s.pendingMutations, sessionID)
				close(current)
			}, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nativeEffectError(ctx)
		case <-current:
		}
	}
}

func (s *RunnerSessions) forgetRevision(sessionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRevision.sessionID == sessionID {
		s.lastRevision = acknowledgedSessionRevision{}
	}
}

func (s *RunnerSessions) mutate(ctx context.Context, request runnersession.Mutation) (runnersession.Snapshot, error) {
	authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
	if !ok || authority.scope != s.base() || authority.lease.ID == "" {
		s.forgetRevision(request.SessionID)
		return runnersession.Snapshot{}, runner.ErrExecutionAuthorityUnavailable
	}
	end, err := s.beginMutation(ctx, request.SessionID)
	if err != nil {
		return runnersession.Snapshot{}, err
	}
	defer end()
	if err := nativeEffectError(ctx); err != nil {
		s.forgetRevision(request.SessionID)
		return runnersession.Snapshot{}, err
	}
	s.mu.Lock()
	metadata := s.acknowledged[request.SessionID]
	last := s.lastRevision
	s.mu.Unlock()
	if metadata.matches(request, authority) {
		return runnersession.Snapshot{}, nil
	}
	if request.Key == "" {
		request.Key = uuid.NewString()
	}
	request.LeaseID, request.FencingToken = authority.lease.ID, authority.lease.FencingToken
	cached := last.matches(request.SessionID, authority)
	if request.SessionID > 0 {
		if cached {
			request.ExpectedRevision = last.revision
		} else {
			current, err := s.snapshot(ctx, request.SessionID)
			if err != nil {
				s.forgetRevision(request.SessionID)
				if NativeAuthorityLost(err) {
					err = errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
				}
				return current, err
			}
			request.ExpectedRevision = current.Revision
		}
	}
	var result runnersession.Snapshot
	send := func() error {
		if err := nativeEffectError(ctx); err != nil {
			return err
		}
		return s.client.Request(ctx, http.MethodPost, s.base()+"/runner-sessions", request, &result)
	}
	err = send()
	var conflict *APIError
	if cached && errors.As(err, &conflict) && conflict.Status == http.StatusConflict && conflict.Code == "revision_conflict" {
		current, readErr := s.snapshot(ctx, request.SessionID)
		err = readErr
		if err == nil {
			if request.Finalization == nil {
				request.Key = uuid.NewString()
			}
			request.ExpectedRevision = current.Revision
			err = send()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.lastRevision = acknowledgedSessionRevision{sessionID: result.ID, revision: result.Revision, leaseID: authority.lease.ID, fencingToken: authority.lease.FencingToken}
	} else if s.lastRevision.sessionID == request.SessionID {
		s.lastRevision = acknowledgedSessionRevision{}
	}

	var api *APIError
	if err != nil && (!errors.As(err, &api) || api == nil || api.Status < 400 || api.Status >= 500) {
		delete(s.acknowledged, request.SessionID)
	}
	if NativeAuthorityLost(err) || api != nil && api.Code == "policy_mismatch" {
		err = errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	if err == nil && request.SessionID > 0 {
		if request.Finish != nil || request.Reaped != nil {
			delete(s.acknowledged, request.SessionID)
		} else if request.Provider != nil || request.Worker != nil {
			if metadata.leaseID != authority.lease.ID || metadata.fencingToken != authority.lease.FencingToken {
				metadata = acknowledgedSessionMetadata{leaseID: authority.lease.ID, fencingToken: authority.lease.FencingToken}
			}
			if request.Provider != nil {
				metadata.provider = result.Provider
			}
			if request.Worker != nil {
				worker := *request.Worker
				metadata.worker, metadata.processKey = &worker, request.ProcessKey
			}
			if s.acknowledged == nil {
				s.acknowledged = make(map[int64]acknowledgedSessionMetadata)
			}
			s.acknowledged[request.SessionID] = metadata
		}
	}
	return result, err
}

func (s *RunnerSessions) StartSession(ctx context.Context, start store.SessionStart) (int64, error) {
	var key string
	worker := start.Worker
	start.Worker = nil
	mutationKey := runner.WorkerSessionStartKey(ctx)
	if worker != nil && mutationKey != "" {
		mac := hmac.New(sha256.New, []byte(s.workerStartSecret))
		_, _ = mac.Write([]byte(mutationKey))
		key = hex.EncodeToString(mac.Sum(nil))
	} else {
		mutationKey = uuid.NewString()
		key = rand.Text() + rand.Text()
	}
	request := runnersession.Mutation{Key: mutationKey, Start: &start, Worker: worker, ProcessKey: key}
	value, err := s.mutate(ctx, request)
	if err != nil {
		if worker == nil || !NativeTransportUnavailable(err) || !errors.Is(err, ErrUnavailable) {
			return value.ID, err
		}
		err = errors.Join(runner.ErrWorkerSessionStartUncertain, err)
		authority, hasAuthority := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
		if ctx.Err() != nil || hasAuthority && authority.retry != nil {
			return 0, err
		}
		var reconcileErr error
		value, reconcileErr = s.mutate(ctx, request)
		if reconcileErr != nil {
			return 0, errors.Join(err, reconcileErr)
		}
		if value.ID <= 0 {
			return 0, err
		}
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	if s.keys == nil {
		s.keys = make(map[int64]string)
	}
	s.keys[value.ID] = key
	if worker != nil {
		if s.workers == nil {
			s.workers = make(map[int64]store.WorkerProcessRegistration)
		}
		s.workers[value.ID] = *worker
	}
	return value.ID, err
}

func (s *RunnerSessions) processKey(id int64) string {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	return s.keys[id]
}
func (s *RunnerSessions) FinishSession(ctx context.Context, id int64, finish store.SessionFinish) error {
	reap := finish.Reap
	finish.Reap = nil
	request := runnersession.Mutation{SessionID: id, Finish: &finish, Reaped: reap}
	if reap != nil {
		request.ProcessKey = s.processKey(id)
	}
	_, err := s.mutate(ctx, request)
	if err == nil && reap != nil {
		s.workersMu.Lock()
		delete(s.workers, id)
		delete(s.keys, id)
		s.workersMu.Unlock()
	}
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
	s.workersMu.Lock()
	ids := make([]int64, 0, len(s.workers))
	for id := range s.workers {
		ids = append(ids, id)
	}
	s.workersMu.Unlock()
	if len(ids) == 0 {
		return nil, nil
	}
	slices.Sort(ids)
	const chunkSize = 200
	var workers []store.WorkerProcess
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		result, err := s.runnerProcessesForSessions(ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		if result.More {
			return nil, errors.New("hub process registrations are incomplete")
		}
		s.workersMu.Lock()
		for _, worker := range result.Workers {
			local, ok := s.workers[worker.SessionID]
			if !ok {
				continue
			}
			worker.WorkerProcessIdentity, worker.CleanupRoot, worker.CleanupPath = local.WorkerProcessIdentity, local.CleanupRoot, local.CleanupPath
			workers = append(workers, worker)
		}
		s.workersMu.Unlock()
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

func (c *NativeClient) runnerProcessesForSessions(ctx context.Context, sessionIDs []int64) (runnersession.Processes, error) {
	parts := make([]string, len(sessionIDs))
	for i, id := range sessionIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	var result runnersession.Processes
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/runner-processes?sessions="+url.QueryEscape(strings.Join(parts, ",")), nil, &result)
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
