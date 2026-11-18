package sshexec

import (
	"context"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/policy"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type SessionStore struct{ Peer Caller }

func (s SessionStore) StartSession(ctx context.Context, input store.SessionStart) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.StartSession", &result, input)
	return result, err
}
func (s SessionStore) FinishSession(ctx context.Context, id int64, input store.SessionFinish) error {
	return s.Peer.Call(ctx, "store.FinishSession", nil, id, input)
}
func (s SessionStore) ReconcileSessionFinalization(ctx context.Context, id int64, input *sessionmodel.Finalization) error {
	return s.Peer.Call(ctx, "store.ReconcileSessionFinalization", nil, id, input)
}
func (s SessionStore) RecordUsageEvent(ctx context.Context, input store.UsageEvent) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.RecordUsageEvent", &result, input)
	return result, err
}

func (s SessionStore) UpdateSessionIdentity(ctx context.Context, id int64, input agentidentity.Identity) error {
	return s.Peer.Call(ctx, "store.UpdateSessionIdentity", nil, id, input)
}

func (s SessionStore) UpdateSessionProviderIdentity(ctx context.Context, id int64, input store.SessionProviderIdentity) error {
	return s.Peer.Call(ctx, "store.UpdateSessionProviderIdentity", nil, id, input)
}

func (s SessionStore) UpdateSessionResumeState(ctx context.Context, id int64, input store.SessionResumeState) error {
	return s.Peer.Call(ctx, "store.UpdateSessionResumeState", nil, id, input)
}

func (s SessionStore) RecordWorkflowPhaseEvent(ctx context.Context, input store.WorkflowPhaseEvent) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.RecordWorkflowPhaseEvent", &result, input)
	return result, err
}

func (s SessionStore) SaveWorkflowActivityProfile(ctx context.Context, id int64, input store.WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.SaveWorkflowActivityProfile", &result, id, input, profile)
	return result, err
}

func (s SessionStore) SessionProgress(ctx context.Context, id int64) (store.SessionProgress, error) {
	var result store.SessionProgress
	err := s.Peer.Call(ctx, "store.SessionProgress", &result, id)
	return result, err
}

func (s SessionStore) SaveSessionProgress(ctx context.Context, id int64, input store.SessionProgress) error {
	return s.Peer.Call(ctx, "store.SaveSessionProgress", nil, id, input)
}

func (s SessionStore) SessionPolicy(ctx context.Context, project string, id int64) (policy.Descriptor, error) {
	var result policy.Descriptor
	err := s.Peer.Call(ctx, "store.SessionPolicy", &result, project, id)
	return result, err
}

func (s SessionStore) LatestAgentResumeState(ctx context.Context, lookup store.AgentResumeLookup) (store.AgentResumeState, error) {
	var result store.AgentResumeState
	err := s.Peer.Call(ctx, "store.LatestAgentResumeState", &result, lookup)
	return result, err
}
func (s SessionStore) LatestIssueAgentResumeState(ctx context.Context, issue store.IssueIdentity) (store.AgentResumeState, error) {
	var result store.AgentResumeState
	err := s.Peer.Call(ctx, "store.LatestIssueAgentResumeState", &result, issue)
	return result, err
}
func (s SessionStore) ListSecurityAuditDispositions(ctx context.Context, id int64) ([]securityaudit.Disposition, error) {
	var result []securityaudit.Disposition
	err := s.Peer.Call(ctx, "store.ListSecurityAuditDispositions", &result, id)
	return result, err
}
