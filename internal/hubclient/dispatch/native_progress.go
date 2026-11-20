package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type progressOwner struct {
	client   *NativeClient
	request  progresscheck.Request
	retained *progresscheck.Record
	lease    tracker.NativeLease
}

func (s *Scheduler) RetryProgress(ctx context.Context, issue connector.Issue) (progresscheck.Request, progresscheck.Owner, error) {
	s.mu.Lock()
	claim, ok := s.nativeClaims[issue.ID]
	s.mu.Unlock()
	if !ok || claim.source == nil || claim.source.NativeClient() == nil {
		return progresscheck.Request{}, nil, errors.New("progress check requires current native claim")
	}
	request := progresscheck.NativeRequest(claim.recovery, claim.lease.PolicyID, string(claim.lease.MachineID))
	latestRefusal := len(request.Attempts) > 0 && request.Attempts[len(request.Attempts)-1].RefusalKind != ""
	if !latestRefusal && !request.NeedsCheck() {
		return request, &progressOwner{client: claim.source.NativeClient(), request: request, lease: claim.lease}, nil
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
	err = claim.source.NativeClient().Transport().Request(ctx, http.MethodGet, claim.source.NativeClient().Scope()+path+"/progress?"+params.Encode(), nil, &view)
	if err != nil {
		return progresscheck.Request{}, nil, err
	}
	if view.Request.ProjectID != string(claim.source.NativeClient().ProjectID()) || view.Request.IssueID != string(claim.lease.WorkItemID) || view.Request.PolicyID != claim.lease.PolicyID || view.Request.Instance != string(claim.lease.MachineID) {
		return progresscheck.Request{}, nil, errors.New("progress evidence execution binding differs")
	}
	return view.Request, &progressOwner{client: claim.source.NativeClient(), request: view.Request, retained: view.Record, lease: claim.lease}, nil
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
	err = o.client.Transport().Request(ctx, http.MethodPost, o.client.Scope()+path+"/progress", input, &result)
	if hubErrorCode(err) == "revision_conflict" {
		current, refreshErr := o.client.Issue(ctx, o.lease.WorkItemID)
		if refreshErr != nil {
			return progresscheck.Record{}, refreshErr
		}
		input.ExpectedRevision = current.Revision
		err = o.client.Transport().Request(ctx, http.MethodPost, o.client.Scope()+path+"/progress", input, &result)
	}
	if err == nil {
		o.retained = &result
	}
	return result, nativeMutationError(err)
}
