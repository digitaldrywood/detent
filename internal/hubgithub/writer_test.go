package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestWriterReplaysWorkflowLabelAsDesiredState(t *testing.T) {
	t.Parallel()

	item := testOutboxItem(t, hubserver.MutationWorkflowLabel, hubserver.WorkflowLabelDesired{
		Label:         "detent:in-progress",
		ManagedPrefix: "detent:",
	})
	wantLabels := map[string]any{"labels": []string{"detent:in-progress", "feature"}}
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/17", response: restIssue{Labels: []restLabel{{Name: "feature"}, {Name: "detent:todo"}}}},
		{method: http.MethodPut, path: "/repos/digitaldrywood/detent/issues/17/labels", body: wantLabels},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/17", response: restIssue{Labels: []restLabel{{Name: "detent:in-progress"}, {Name: "feature"}}}},
		{method: http.MethodPut, path: "/repos/digitaldrywood/detent/issues/17/labels", body: wantLabels},
	}}
	writer := &Writer{client: client}

	for attempt := range 2 {
		if err := writer.Execute(t.Context(), item); err != nil {
			t.Fatalf("Execute(%d) error = %v", attempt, err)
		}
	}
	client.assertDone()
}

func TestWriterReplaysWorkpadIntoOneComment(t *testing.T) {
	t.Parallel()

	body := "## Codex Workpad\n\nTesting complete."
	marker := "<!-- detent-workpad -->"
	wantBody := body + "\n\n" + marker
	item := testOutboxItem(t, hubserver.MutationWorkpad, hubserver.WorkpadDesired{
		Phase:  "testing",
		Body:   body,
		Marker: marker,
	})
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/17/comments?per_page=100", response: []restComment{}},
		{method: http.MethodPost, path: "/repos/digitaldrywood/detent/issues/17/comments", body: map[string]any{"body": wantBody}, response: restComment{ID: 42}},
		{method: http.MethodGet, path: "/repos/digitaldrywood/detent/issues/17/comments?per_page=100", response: []restComment{{ID: 42, Body: wantBody}}},
		{method: http.MethodPatch, path: "/repos/digitaldrywood/detent/issues/comments/42", body: map[string]any{"body": wantBody}},
	}}
	writer := &Writer{client: client}

	for attempt := range 2 {
		if err := writer.Execute(t.Context(), item); err != nil {
			t.Fatalf("Execute(%d) error = %v", attempt, err)
		}
	}
	client.assertDone()
}

func TestWriterFindsWorkpadOnLaterCommentPage(t *testing.T) {
	t.Parallel()

	body := "## Codex Workpad\n\nTesting complete."
	marker := "<!-- detent-workpad -->"
	wantBody := body + "\n\n" + marker
	item := testOutboxItem(t, hubserver.MutationWorkpad, hubserver.WorkpadDesired{
		Phase:  "testing",
		Body:   body,
		Marker: marker,
	})
	firstPage := "/repos/digitaldrywood/detent/issues/17/comments?per_page=100"
	secondPage := "/repos/digitaldrywood/detent/issues/17/comments?page=2&per_page=100"
	client := &scriptedRESTClient{t: t, steps: []restStep{
		{method: http.MethodGet, path: firstPage, response: []restComment{{ID: 1, Body: "unrelated"}}, next: secondPage},
		{method: http.MethodGet, path: secondPage, response: []restComment{{ID: 42, Body: marker}}},
		{method: http.MethodPatch, path: "/repos/digitaldrywood/detent/issues/comments/42", body: map[string]any{"body": wantBody}},
	}}

	if err := (&Writer{client: client}).Execute(t.Context(), item); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	client.assertDone()
}

func TestWriterVerifiesFreshStateBeforeMerge(t *testing.T) {
	t.Parallel()

	ready := true
	item := testOutboxItem(t, hubserver.MutationMergePullRequest, hubserver.MergePullRequestDesired{
		PullRequestNumber: 29,
		HeadSHA:           "abc123",
		MergeMethod:       "squash",
	})
	pullPath := "/repos/digitaldrywood/detent/pulls/29"
	reviewsPath := pullPath + "/reviews?per_page=100"
	checksPath := "/repos/digitaldrywood/detent/commits/abc123/check-runs?per_page=100"
	statusesPath := "/repos/digitaldrywood/detent/commits/abc123/statuses?per_page=100"

	tests := []struct {
		name          string
		steps         []restStep
		wantError     bool
		wantPermanent bool
	}{
		{
			name: "fresh state allows exact head merge",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{testReview("APPROVED", "reviewer")}},
				{method: http.MethodGet, path: checksPath, response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 2, Name: "test", Status: "completed", Conclusion: "success"}}}},
				{method: http.MethodGet, path: statusesPath, response: []restStatus{{Context: "legacy", State: "success"}}},
				{method: http.MethodPut, path: pullPath + "/merge", body: map[string]string{"merge_method": "squash", "sha": "abc123"}, response: restMergeResponse{Merged: true}},
			},
		},
		{
			name: "already merged exact head is idempotent",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("closed", "abc123", true, &ready)},
			},
		},
		{
			name: "pull request refresh failure",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, err: errors.New("github unavailable")},
			},
			wantError: true,
		},
		{
			name: "closed without merge",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("closed", "abc123", false, &ready)},
			},
			wantError:     true,
			wantPermanent: true,
		},
		{
			name: "head changed",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "different", false, &ready)},
			},
			wantError:     true,
			wantPermanent: true,
		},
		{
			name: "mergeability unavailable",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, nil)},
			},
			wantError: true,
		},
		{
			name: "review requests changes",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{testReview("CHANGES_REQUESTED", "reviewer")}},
			},
			wantError: true,
		},
		{
			name: "later review page requests changes",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{testReview("APPROVED", "reviewer")}, next: reviewsPath + "&page=2"},
				{method: http.MethodGet, path: reviewsPath + "&page=2", response: []restReview{testReview("CHANGES_REQUESTED", "reviewer")}},
			},
			wantError: true,
		},
		{
			name: "merge state is not clean",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: func() restPullRequest {
					response := testPullRequest("open", "abc123", false, &ready)
					response.MergeableState = "blocked"
					return response
				}()},
			},
			wantError: true,
		},
		{
			name: "check is pending",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{}},
				{method: http.MethodGet, path: checksPath, response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 2, Name: "test", Status: "in_progress"}}}},
			},
			wantError: true,
		},
		{
			name: "later check page failed",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{}},
				{method: http.MethodGet, path: checksPath, response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "success"}}}, next: checksPath + "&page=2"},
				{method: http.MethodGet, path: checksPath + "&page=2", response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 2, Name: "test", Status: "completed", Conclusion: "failure"}}}},
			},
			wantError: true,
		},
		{
			name: "legacy status is pending",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{}},
				{method: http.MethodGet, path: checksPath, response: restCheckRuns{}},
				{method: http.MethodGet, path: statusesPath, response: []restStatus{{Context: "legacy", State: "pending"}}},
			},
			wantError: true,
		},
		{
			name: "later status page is pending",
			steps: []restStep{
				{method: http.MethodGet, path: pullPath, response: testPullRequest("open", "abc123", false, &ready)},
				{method: http.MethodGet, path: reviewsPath, response: []restReview{}},
				{method: http.MethodGet, path: checksPath, response: restCheckRuns{}},
				{method: http.MethodGet, path: statusesPath, response: []restStatus{{Context: "first-page", State: "success"}}, next: statusesPath + "&page=2"},
				{method: http.MethodGet, path: statusesPath + "&page=2", response: []restStatus{{Context: "later-page", State: "pending"}}},
			},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedRESTClient{t: t, steps: test.steps}
			err := (&Writer{client: client}).VerifyAndExecute(t.Context(), item)
			if (err != nil) != test.wantError {
				t.Fatalf("VerifyAndExecute() error = %v, wantError %t", err, test.wantError)
			}
			if got := hubserver.IsPermanent(err); got != test.wantPermanent {
				t.Fatalf("VerifyAndExecute() permanent = %t, want %t; error = %v", got, test.wantPermanent, err)
			}
			client.assertDone()
		})
	}
}

func TestWriterClassifiesDeliveryErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{name: "authentication", err: connectorgithub.ErrAuthenticationFailed, wantPermanent: true},
		{name: "not found", err: connectorgithub.ErrNotFound, wantPermanent: true},
		{name: "unprocessable", err: &connectorgithub.StatusError{StatusCode: http.StatusUnprocessableEntity, Err: connectorgithub.ErrUnexpectedStatus}, wantPermanent: true},
		{name: "rate limited", err: connectorgithub.ErrRateLimited},
		{name: "reserved budget", err: connectorgithub.ErrRESTBudgetReserved},
		{name: "transient", err: connectorgithub.ErrTransient},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyError(test.err)
			if permanent := hubserver.IsPermanent(got); permanent != test.wantPermanent {
				t.Fatalf("classifyError() permanent = %t, want %t; error = %v", permanent, test.wantPermanent, got)
			}
		})
	}
}

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

func testOutboxItem(t *testing.T, kind hubserver.MutationKind, desired any) hubserver.OutboxItem {
	t.Helper()
	raw, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired state: %v", err)
	}
	return hubserver.OutboxItem{
		IdempotencyKey:  "stable-key",
		RepositoryOwner: "digitaldrywood",
		RepositoryName:  "detent",
		IssueNumber:     17,
		Kind:            kind,
		Desired:         raw,
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
