package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) LandingBarrier(ctx context.Context, repository string) (tracker.LandingBarrier, error) {
	var result tracker.LandingBarrier
	err := c.client.request(ctx, http.MethodGet, c.base()+"/landing-barrier?repository="+url.QueryEscape(repository), nil, &result)
	return result, err
}

func (c *NativeClient) MutateLandingBarrier(ctx context.Context, request tracker.LandingBarrierRequest) (tracker.LandingBarrier, error) {
	var result tracker.LandingBarrier
	err := c.client.request(ctx, http.MethodPost, c.base()+"/landing-barrier", request, &result)
	return result, err
}

func (s *Scheduler) NextLandingBarrier(ctx context.Context, project, repository, policyID string, recoverClaim bool, observeHead func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error) {
	source := s.nativeProject(project)
	if source == nil {
		return tracker.LandingBarrier{}, false, nil
	}
	observed, err := source.client.LandingBarrier(ctx, repository)
	if err != nil || observed.ProjectID == "" || observed.Running && !recoverClaim {
		return observed, false, err
	}
	head, err := observeHead(ctx, observed.BaseRef)
	if err != nil || !observed.Running && observed.Result != nil && observed.Result.HeadSHA == head {
		return observed, false, err
	}
	key, err := randomSessionID()
	if err != nil {
		return observed, false, err
	}
	result, err := source.client.MutateLandingBarrier(ctx, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, Action: "start", Repository: repository, PolicyID: policyID, Head: head, Recover: recoverClaim})
	return result, err == nil && result.Running && result.ID == key, err
}

func (s *Scheduler) FinishLandingBarrier(ctx context.Context, project string, barrier tracker.LandingBarrier, result *gate.CommandResult) error {
	source := s.nativeProject(project)
	if source == nil {
		return errors.New("landing barrier project is unavailable")
	}
	action := "finish"
	if result == nil {
		action = "cancel"
	}
	_, err := source.client.MutateLandingBarrier(ctx, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: barrier.ID + ":" + action}, Action: action, Repository: barrier.Repository, ID: barrier.ID, Result: result})
	return settledBarrierFinish(err)
}

func (s *Scheduler) LandingBarrierCurrent(ctx context.Context, project, repository, id string) bool {
	source := s.nativeProject(project)
	if source == nil {
		return true
	}
	observed, err := source.client.LandingBarrier(ctx, repository)
	return err != nil || observed.ID == id
}

func settledBarrierFinish(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return nil
	}
	return err
}
