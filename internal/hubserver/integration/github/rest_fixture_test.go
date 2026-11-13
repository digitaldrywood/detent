package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type restStep struct {
	method   string
	path     string
	body     any
	response any
	err      error
	next     string
}

type scriptedRESTClient struct {
	t     *testing.T
	steps []restStep
}

func (c *scriptedRESTClient) REST(_ context.Context, method, path string, body, output any) error {
	_, err := c.call(method, path, body, output)
	return err
}

func (c *scriptedRESTClient) RESTPage(_ context.Context, path string, output any) (string, error) {
	return c.call(http.MethodGet, path, nil, output)
}

func (c *scriptedRESTClient) call(method, path string, body, output any) (string, error) {
	c.t.Helper()
	if len(c.steps) == 0 {
		c.t.Fatalf("unexpected REST call %s %s", method, path)
	}
	step := c.steps[0]
	c.steps = c.steps[1:]
	if method != step.method || path != step.path {
		c.t.Fatalf("REST call = %s %s, want %s %s", method, path, step.method, step.path)
	}
	if !reflect.DeepEqual(body, step.body) {
		c.t.Fatalf("REST %s %s body = %#v, want %#v", method, path, body, step.body)
	}
	if step.err != nil {
		return "", step.err
	}
	if output == nil || step.response == nil {
		return step.next, nil
	}
	raw, err := json.Marshal(step.response)
	if err != nil {
		c.t.Fatalf("marshal REST response: %v", err)
	}
	if err := json.Unmarshal(raw, output); err != nil {
		c.t.Fatalf("unmarshal REST response: %v", err)
	}
	return step.next, nil
}

func (c *scriptedRESTClient) assertDone() {
	c.t.Helper()
	if len(c.steps) != 0 {
		c.t.Fatalf("REST steps remaining = %d", len(c.steps))
	}
}

func testPullRequest(state, head string, merged bool, mergeable *bool) restPullRequest {
	result := restPullRequest{State: state, Merged: merged, Mergeable: mergeable, MergeableState: "clean"}
	result.Head.SHA = head
	return result
}

func testReview(state, login string) restReview {
	result := restReview{State: state}
	result.User.Login = login
	return result
}

type restClient interface {
	REST(context.Context, string, string, any, any) error
	RESTPage(context.Context, string, any) (string, error)
}

type restComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

type restPullRequest struct {
	UpdatedAt      time.Time `json:"updated_at"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	Merged         bool      `json:"merged"`
	Mergeable      *bool     `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type restReview struct {
	State       string     `json:"state"`
	Body        string     `json:"body"`
	SubmittedAt *time.Time `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

type restCheckRuns struct {
	CheckRuns []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type restStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

type restMergeResponse struct {
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}
