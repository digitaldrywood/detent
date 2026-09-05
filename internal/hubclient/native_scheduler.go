package hubclient

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeClaim struct {
	source *NativeConnector
	lease  tracker.NativeLease
}

func (s *Scheduler) ConnectorForProject(project string) (connector.Connector, bool) {
	source, ok := s.nativeProjects[project]
	if !ok {
		return nil, false
	}
	return source, true
}

func (s *Scheduler) ensureNativeMachine(ctx context.Context, source *NativeConnector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.nativeHeartbeats[source.client.project]
	if !last.IsZero() && s.now().Before(last.Add(s.heartbeatInterval)) {
		return nil
	}
	if last.IsZero() {
		if err := source.client.Negotiate(ctx); err != nil {
			return err
		}
	}
	if err := source.client.RegisterMachine(ctx, s.machine); err != nil {
		return err
	}
	s.nativeHeartbeats[source.client.project] = s.now()
	return nil
}

func (s *Scheduler) fetchNativeCandidate(ctx context.Context, request orchestrator.SchedulingRequest, source *NativeConnector) ([]connector.Issue, error) {
	if err := s.ensureNativeMachine(ctx, source); err != nil {
		return nil, schedulingError(err)
	}
	session, err := s.sessionID()
	if err != nil {
		return nil, err
	}
	lease, err := source.client.Claim(ctx, tracker.NativeClaim{
		MachineID: s.machine.ID, SessionID: session, TTLSeconds: int64(s.leaseTTL / time.Second), ProtocolMajor: 2,
		Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: request.WorkflowStates,
		Authors: request.Filter.Authors, Assignees: request.Filter.Assignees, LabelInclude: request.Filter.LabelInclude, LabelExclude: request.Filter.LabelExclude,
	})
	if errors.Is(err, ErrNoClaimableWork) {
		return []connector.Issue{}, nil
	}
	if err != nil {
		return nil, schedulingError(err)
	}
	item, err := source.client.Issue(ctx, lease.WorkItemID)
	if err != nil {
		return nil, errors.Join(err, source.client.Release(context.WithoutCancel(ctx), lease, "work_item_hydration_failed"))
	}
	issue := issueFromNative(item)
	issue.AssignedToWorker = true
	s.mu.Lock()
	s.claims[issue.ID] = nativeTrackerLease(lease)
	s.nativeClaims[issue.ID] = nativeClaim{source: source, lease: lease}
	s.mu.Unlock()
	return []connector.Issue{issue}, nil
}

func (s *Scheduler) renewNativeClaim(ctx context.Context, issueID string, claim nativeClaim) (orchestrator.Claimed, error) {
	if err := s.ensureNativeMachine(ctx, claim.source); err != nil {
		return orchestrator.Claimed{}, err
	}
	lease, err := claim.source.client.Renew(ctx, claim.lease, int64(s.leaseTTL/time.Second))
	if err != nil {
		if claimLost(err) {
			s.mu.Lock()
			delete(s.claims, issueID)
			delete(s.nativeClaims, issueID)
			s.mu.Unlock()
			return orchestrator.Claimed{}, errors.Join(orchestrator.ErrSchedulingClaimLost, err)
		}
		return orchestrator.Claimed{}, err
	}
	claim.lease = lease
	s.mu.Lock()
	s.nativeClaims[issueID] = claim
	s.claims[issueID] = nativeTrackerLease(lease)
	s.mu.Unlock()
	return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(lease)), nil
}

func nativeTrackerLease(lease tracker.NativeLease) tracker.Lease {
	return tracker.Lease{LeaseSummary: tracker.LeaseSummary{ID: lease.ID, FencingToken: lease.FencingToken,
		Machine: tracker.MachineSummary{ID: lease.MachineID}, SessionID: lease.SessionID, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt}}
}
