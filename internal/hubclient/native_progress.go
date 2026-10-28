package hubclient

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
	if !ok || claim.source == nil || claim.source.client == nil {
		return progresscheck.Request{}, nil, errors.New("progress check requires current native claim")
	}
	request := progresscheck.NativeRequest(claim.recovery, claim.lease.PolicyID, string(claim.lease.MachineID))
	if len(request.Attempts) == 0 || !progresscheck.Failed(request.Attempts[len(request.Attempts)-1].Outcome) {
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
	input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: o.lease.ID, FencingToken: o.lease.FencingToken}, RequestID: record.RequestID, PolicyID: o.request.PolicyID, Instance: o.request.Instance, Decision: record.Decision}
	var result progresscheck.Record
	err = o.client.client.request(ctx, http.MethodPost, o.client.base()+path+"/progress", input, &result)
	if err == nil {
		o.retained = &result
	}
	return result, nativeMutationError(err)
}
