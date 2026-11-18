package fakehubscheduling

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

type Source struct {
	Connector  connector.Connector
	Factory    func(string) (connector.Connector, bool)
	connectors map[string]connector.Connector
	mu         sync.Mutex
	claims     map[string]orchestrator.Claimed
}

func (s *Source) ConnectorForProject(id string) (connector.Connector, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Factory == nil {
		return s.Connector, s.Connector != nil
	}
	if conn, ok := s.connectors[id]; ok {
		return conn, true
	}
	conn, ok := s.Factory(id)
	if !ok {
		return nil, false
	}
	if s.connectors == nil {
		s.connectors = make(map[string]connector.Connector)
	}
	s.connectors[id] = conn
	return conn, true
}
func (*Source) HeartbeatInterval() time.Duration { return time.Second }
func (s *Source) FetchCandidateIssues(ctx context.Context, request orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	conn, ok := s.ConnectorForProject(request.ProjectID)
	if !ok {
		return nil, nil
	}
	discoveryCtx := ctx
	if request.BeginDiscovery != nil {
		var finish context.CancelFunc
		discoveryCtx, finish = request.BeginDiscovery(ctx)
		defer finish()
	}
	issues, err := conn.FetchCandidateIssues(discoveryCtx)
	if discoveryCtx.Err() != nil && ctx.Err() == nil {
		return nil, nil
	}
	if err != nil || len(request.WorkflowStates) == 0 {
		return issues, err
	}
	filtered := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if slices.ContainsFunc(request.WorkflowStates, func(state string) bool {
			return strings.EqualFold(strings.TrimSpace(issue.State), strings.TrimSpace(state))
		}) {
			filtered = append(filtered, issue)
		}
	}
	return filtered, nil
}
func (s *Source) AdoptClaim(_ context.Context, issue connector.Issue, now time.Time) (orchestrator.Claimed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims == nil {
		s.claims = make(map[string]orchestrator.Claimed)
	}
	claim := orchestrator.Claimed{Issue: issue, ClaimedAt: now, LeaseRenewedAt: now, LeaseExpiresAt: now.Add(time.Hour), Owner: "fixture"}
	s.claims[issue.ID] = claim
	return claim, nil
}
func (s *Source) RenewClaim(_ context.Context, id string, now time.Time) (orchestrator.Claimed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.claims[id]
	if !ok {
		return orchestrator.Claimed{}, connector.ErrNotImplemented
	}
	claim.LeaseRenewedAt, claim.LeaseExpiresAt = now, now.Add(time.Hour)
	s.claims[id] = claim
	return claim, nil
}
func (s *Source) ReleaseClaim(_ context.Context, id, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, id)
	return nil
}
