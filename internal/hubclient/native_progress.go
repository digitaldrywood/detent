package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type progressOwner struct {
	client   *NativeClient
	request  progresscheck.Request
	retained *progresscheck.Record
	lease    tracker.NativeLease
}

func (s *Scheduler) ReadmitRetryProgress(ctx context.Context, run runner.RunRequest, resolver runner.ProviderCapacityResolver) (runner.Execution, *isolation.Policy, error) {
	s.mu.Lock()
	claim, ok := s.nativeClaims[run.Issue.ID]
	policy := s.claimPolicies[run.Issue.ID]
	s.mu.Unlock()
	if !ok || claim.execution == nil || claim.execution != run.Execution {
		return nil, nil, runner.ErrExecutionAuthorityUnavailable
	}
	if claim.lifecycle != nil {
		claim.lifecycle.Lock()
		defer claim.lifecycle.Unlock()
	}
	session, err := s.sessionID()
	if err != nil {
		return nil, nil, err
	}
	request := tracker.NativeClaim{WorkItemID: claim.lease.WorkItemID, PolicyID: claim.lease.PolicyID, MachineID: claim.lease.MachineID, SessionID: session, TTLSeconds: int64(s.leaseTTL / time.Second), ProtocolMajor: 2, LandingCapacity: claim.landingCapacity, WorkflowStates: []string{run.Issue.State}, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	if s.providerReports != nil {
		if resolver == nil {
			return nil, nil, errors.New("progress admission requires the runner's model resolver")
		}
		run.ProviderReports, err = s.providerReports()
		if err != nil {
			return nil, nil, err
		}
		requirement, err := resolver.DispatchCapacity(ctx, run)
		if err != nil {
			return nil, nil, err
		}
		issue, err := claim.source.client.Issue(ctx, claim.lease.WorkItemID)
		if err != nil {
			return nil, nil, err
		}
		request.ProviderCandidates = []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: requirement}}
		request.Capabilities = append(request.Capabilities, tracker.NativeProviderCapacityCapability)
	}
	if err := claim.execution.Finish(ctx, "cancelled"); err != nil {
		return nil, nil, err
	}
	claim.execution.RetainCompletion()
	if err := claim.execution.finishPrepared(ctx); err != nil {
		return nil, nil, err
	}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, found := s.nativeClaims[run.Issue.ID]; found && current.lease.FencingToken == claim.lease.FencingToken {
			delete(s.claims, run.Issue.ID)
			delete(s.nativeClaims, run.Issue.ID)
			delete(s.claimPolicies, run.Issue.ID)
		}
	}()
	started := s.now()
	lease, err, admitted := s.claimNative(ctx, claim.source, request)
	if err != nil {
		return nil, nil, err
	}
	if !admitted {
		return nil, nil, orchestrator.ErrSchedulingUnavailable
	}
	release := func(cause error) (runner.Execution, *isolation.Policy, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return nil, nil, errors.Join(cause, claim.source.client.Release(cleanupCtx, lease, "work_item_hydration_failed"))
	}
	recovery, err := claim.source.client.Recovery(ctx, lease.WorkItemID)
	if err == nil && s.providerReports != nil && lease.ProviderReservation == nil {
		err = errors.New("hub omitted the required provider reservation")
	}
	if err != nil {
		return release(err)
	}
	if err := ctx.Err(); err != nil {
		return release(err)
	}
	s.mu.Lock()
	if current, exists := s.nativeClaims[run.Issue.ID]; !exists || current.lost || current.lease.FencingToken != claim.lease.FencingToken {
		s.mu.Unlock()
		return release(runner.ErrExecutionAuthorityUnavailable)
	}
	s.claims[run.Issue.ID] = nativeTrackerLease(lease)
	s.nativeClaims[run.Issue.ID] = nativeClaim{lifecycle: claim.lifecycle, source: claim.source, lease: lease, recovery: recovery, deadline: nativeLeaseDeadline(started, lease), availabilityDeadline: claim.availabilityDeadline, landingCapacity: claim.landingCapacity}
	s.claimPolicies[run.Issue.ID] = policy
	s.mu.Unlock()
	return s.RunExecution(run.Issue.ID), lease.IsolationPolicy, nil
}

func (s *Scheduler) RetryProgress(ctx context.Context, issue connector.Issue) (progresscheck.Request, progresscheck.Owner, error) {
	s.mu.Lock()
	claim, ok := s.nativeClaims[issue.ID]
	s.mu.Unlock()
	if !ok || claim.source == nil || claim.source.client == nil {
		return progresscheck.Request{}, nil, errors.New("progress check requires current native claim")
	}
	request := progresscheck.NativeRequest(claim.recovery, claim.lease.PolicyID, string(claim.lease.MachineID))
	latestRefusal := len(request.Attempts) > 0 && request.Attempts[len(request.Attempts)-1].RefusalKind != ""
	if !latestRefusal && !request.NeedsCheck() {
		return request, &progressOwner{client: claim.source.client, request: request, lease: claim.lease}, nil
	}
	path, err := nativeItemPath(claim.lease.WorkItemID)
	if err != nil {
		return progresscheck.Request{}, nil, err
	}
	var view struct {
		Request progresscheck.Request `json:"request"`
		Record  *progresscheck.Record `json:"record"`
	}
	params := url.Values{"policy": {claim.lease.PolicyID}, "instance": {string(claim.lease.MachineID)}}
	err = claim.source.client.client.request(ctx, http.MethodGet, claim.source.client.base()+path+"/progress?"+params.Encode(), nil, &view)
	if err != nil {
		return progresscheck.Request{}, nil, err
	}
	if view.Request.ProjectID != string(claim.source.client.project) || view.Request.IssueID != string(claim.lease.WorkItemID) || view.Request.PolicyID != claim.lease.PolicyID || view.Request.Instance != string(claim.lease.MachineID) {
		return progresscheck.Request{}, nil, errors.New("progress evidence execution binding differs")
	}
	return view.Request, &progressOwner{client: claim.source.client, request: view.Request, retained: view.Record, lease: claim.lease}, nil
}

func (o *progressOwner) Load(_ context.Context, project, issue, id string) (progresscheck.Record, bool, error) {
	if project != o.request.ProjectID || issue != o.request.IssueID {
		return progresscheck.Record{}, false, errors.New("progress decision scope differs")
	}
	if o.retained == nil || o.retained.RequestID != id {
		return progresscheck.Record{}, false, nil
	}
	return *o.retained, true, nil
}

func (o *progressOwner) Publish(ctx context.Context, record progresscheck.Record) (progresscheck.Record, error) {
	path, err := nativeItemPath(o.lease.WorkItemID)
	if err != nil {
		return progresscheck.Record{}, err
	}
	modern, err := o.client.HubFeature(ctx, progresscheck.InvestigationCapability)
	if err != nil {
		return progresscheck.Record{}, err
	}
	decision := record.Decision
	if len(decision.RetrievedEvidence) > 0 {
		supported, err := o.client.HubFeature(ctx, progresscheck.RetrievedEvidenceCapability)
		if err != nil {
			return progresscheck.Record{}, err
		}
		if !supported {
			return progresscheck.Record{}, errors.New("hub cannot publish retrieved progress evidence")
		}
	}
	if !modern {
		decision.Diagnosis, decision.Instruction, decision.Convergence = "", "", ""
	}
	input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: o.lease.ID, FencingToken: o.lease.FencingToken}, RequestID: record.RequestID, PolicyID: o.request.PolicyID, Instance: o.request.Instance, Decision: decision}
	current, err := o.client.Issue(ctx, o.lease.WorkItemID)
	if err != nil {
		return progresscheck.Record{}, err
	}
	input.ExpectedRevision = current.Revision
	var result progresscheck.Record
	err = o.client.client.request(ctx, http.MethodPost, o.client.base()+path+"/progress", input, &result)
	if hubErrorCode(err) == "revision_conflict" {
		current, refreshErr := o.client.Issue(ctx, o.lease.WorkItemID)
		if refreshErr != nil {
			return progresscheck.Record{}, refreshErr
		}
		input.ExpectedRevision = current.Revision
		err = o.client.client.request(ctx, http.MethodPost, o.client.base()+path+"/progress", input, &result)
	}
	if err == nil {
		o.retained = &result
	}
	return result, nativeMutationError(err)
}
