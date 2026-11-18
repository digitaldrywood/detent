package runner

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
)

type nativeSessionOwner struct {
	policy    policy.Descriptor
	snapshots map[int64]*runnersession.Snapshot
	attempts  map[string]int64
}

type nativeSessionClient struct {
	fakeSessionStore
	owner   *nativeSessionOwner
	attempt string
}

func newNativeSessionOwner(approved policy.Descriptor) *nativeSessionOwner {
	return &nativeSessionOwner{policy: approved, snapshots: make(map[int64]*runnersession.Snapshot), attempts: make(map[string]int64)}
}

func (o *nativeSessionOwner) client(attempt string) *nativeSessionClient {
	return &nativeSessionClient{owner: o, attempt: attempt}
}

func (o *nativeSessionOwner) seed(attempt string, start store.SessionStart, resume AgentResume, finish store.SessionFinish) int64 {
	id := int64(len(o.snapshots) + 1)
	o.snapshots[id] = &runnersession.Snapshot{ID: id, Start: start, Identity: start.RuntimeIdentity, Provider: store.SessionProviderIdentity{ThreadID: resume.ThreadID, SessionID: resume.SessionID}, Finish: &finish, Policy: o.policy}
	o.attempts[attempt] = id
	return id
}

func (o *nativeSessionOwner) latest(t *testing.T, attempt string) runnersession.Snapshot {
	t.Helper()
	snapshot := o.snapshots[o.attempts[attempt]]
	if snapshot == nil {
		t.Fatalf("Hub attempt %q has no session", attempt)
	}
	return *snapshot
}

func (s *nativeSessionClient) StartSession(ctx context.Context, start store.SessionStart) (int64, error) {
	id := int64(len(s.owner.snapshots) + 1)
	s.sessionID = id
	_, err := s.fakeSessionStore.StartSession(ctx, start)
	if err != nil {
		return 0, err
	}
	s.owner.snapshots[id] = &runnersession.Snapshot{ID: id, Start: start, Identity: start.RuntimeIdentity, Provider: store.SessionProviderIdentity{ThreadID: start.ProviderThreadID, SessionID: start.ProviderSessionID}, Policy: s.owner.policy}
	s.owner.attempts[s.attempt] = id
	return id, nil
}

func (s *nativeSessionClient) FinishSession(ctx context.Context, id int64, finish store.SessionFinish) error {
	s.owner.snapshots[id].Finish = &finish
	return s.fakeSessionStore.FinishSession(ctx, id, finish)
}

func (s *nativeSessionClient) UpdateSessionIdentity(ctx context.Context, id int64, identity agentidentity.Identity) error {
	s.owner.snapshots[id].Identity = identity
	return s.fakeSessionStore.UpdateSessionIdentity(ctx, id, identity)
}

func (s *nativeSessionClient) UpdateSessionProviderIdentity(ctx context.Context, id int64, identity store.SessionProviderIdentity) error {
	snapshot := s.owner.snapshots[id]
	if identity.ThreadID != "" {
		snapshot.Provider.ThreadID = identity.ThreadID
	}
	if identity.SessionID != "" {
		snapshot.Provider.SessionID = identity.SessionID
	}
	return s.fakeSessionStore.UpdateSessionProviderIdentity(ctx, id, identity)
}

func (s *nativeSessionClient) LatestAgentResumeState(ctx context.Context, lookup store.AgentResumeLookup) (store.AgentResumeState, error) {
	s.resumeLookup = lookup
	s.resumeLookups++
	snapshot := s.owner.snapshots[s.owner.attempts[lookup.NativeAttemptID]]
	if lookup.NativeAttemptID == "" || lookup.WorkAttemptID != 0 || snapshot == nil {
		return store.AgentResumeState{}, store.ErrNotFound
	}
	start := snapshot.Start
	state := store.AgentResumeState{DetentSessionID: snapshot.ID, RuntimeIdentity: snapshot.Identity, RequestedModel: start.RequestedModel, Model: snapshot.Identity.ResolvedModel.Value, AgentBackendID: start.AgentBackendID, AgentBackendKind: start.AgentBackendKind, AgentRole: start.AgentRole, ProviderThreadID: snapshot.Provider.ThreadID, ProviderSessionID: snapshot.Provider.SessionID, Orphaned: true}
	if state.RequestedModel != lookup.RequestedModel || state.AgentBackendID != lookup.AgentBackendID || state.AgentBackendKind != lookup.AgentBackendKind || state.AgentRole != lookup.AgentRole {
		return store.AgentResumeState{}, store.ErrNotFound
	}
	return state, nil
}

func (s *nativeSessionClient) SessionPolicy(_ context.Context, _ string, id int64) (policy.Descriptor, error) {
	snapshot := s.owner.snapshots[id]
	if snapshot == nil {
		return policy.Descriptor{}, store.ErrNotFound
	}
	return snapshot.Policy, nil
}

func (s *nativeSessionClient) SaveSessionProgress(_ context.Context, id int64, progress store.SessionProgress) error {
	s.owner.snapshots[id].Progress = &progress
	return nil
}

func (s *nativeSessionClient) SessionProgress(_ context.Context, id int64) (store.SessionProgress, error) {
	snapshot := s.owner.snapshots[id]
	if snapshot == nil || snapshot.Progress == nil {
		return store.SessionProgress{}, store.ErrNotFound
	}
	return *snapshot.Progress, nil
}
