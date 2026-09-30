package hubclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type intakeRepositoryBackend struct{}

func (intakeRepositoryBackend) Reconcile(context.Context, hubserver.ReconcileRequest) (hubserver.ReconcileSnapshot, error) {
	return hubserver.ReconcileSnapshot{Repository: hubserver.RepositorySource{NodeID: "R_source", Owner: "acme", Name: "orders", UpdatedAt: time.Now().UTC()}}, nil
}

func newLinkedChangeHub(t *testing.T) (*nativeChangeHub, tracker.NativeIssue) {
	t.Helper()
	h := newNativeChangeHubWithStates(t, "Human Review", hubserver.HostedProjectStates(), intakeRepositoryBackend{})
	if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/onboarding/repository", map[string]any{"idempotency_key": "attach", "expected_revision": "1", "repository": "acme/orders"}, nil); err != nil {
		t.Fatal(err)
	}
	issue, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), GitHubIssueURL: "https://github.com/acme/orders/issues/12", State: "In Progress"})
	if err != nil {
		t.Fatal(err)
	}
	return h, issue
}

func intakeSnapshot() tracker.GitHubIssueSnapshot {
	now := time.Now().UTC()
	prov := tracker.Provenance{Provider: "github", ExternalID: "I_source", AuthorID: "source-author", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute), ObservedAt: now}
	comment := prov
	comment.ExternalID = "C_source"
	return tracker.GitHubIssueSnapshot{URL: "https://github.com/acme/orders/issues/12", Title: "Imported execution task", Body: "Complete implementation context", Provenance: prov, Comments: []tracker.GitHubIssueComment{{Body: "Useful source discussion", Provenance: comment}}}
}

func TestNativeSourceIntakeRetryBeforeDispatch(t *testing.T) {
	t.Parallel()
	for _, failure := range []struct {
		name    string
		err     error
		missing bool
		persist bool
	}{
		{name: "missing credential", err: githubconnector.ErrMissingToken, missing: true},
		{name: "private authorization", err: githubconnector.ErrAuthenticationFailed},
		{name: "inaccessible issue", err: githubconnector.ErrNotFound},
		{name: "rate limit", err: githubconnector.ErrRateLimited},
		{name: "partial fetch", err: githubconnector.ErrInvalidResponse},
		{name: "persistence failure", persist: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			h, issue := newLinkedChangeHub(t)
			var calls int
			if !failure.missing {
				h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
					calls++
					if failure.persist {
						return intakeSnapshot(), nil
					}
					return tracker.GitHubIssueSnapshot{}, failure.err
				}
			}
			h.failChanges.failIntake.Store(failure.persist)
			request := orchestrator.SchedulingRequest{ProjectID: "local", Repository: "acme/orders", Policy: h.descriptor, WorkflowStates: []string{"In Progress"}}
			items, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
			if !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(items) != 0 || !strings.Contains(err.Error(), "source intake") {
				t.Fatalf("dispatch = %#v, error = %v", items, err)
			}
			if _, stop, guardErr := h.scheduler.RunExecution(string(issue.WorkItemID)).Guard(t.Context()); guardErr == nil {
				stop()
				t.Fatal("failed intake granted execution authority")
			}
			stored, err := h.admin.Issue(t.Context(), issue.WorkItemID)
			if err != nil || stored.LinkedSource.Status != "pending" || stored.Body != "" || stored.State != "In Progress" {
				t.Fatalf("failed intake changed issue: %#v, error = %v", stored, err)
			}
			// The released failed claim can be claimed immediately using existing
			// scheduling. No issue lane move or recovery mechanism is needed.
			h.failChanges.failIntake.Store(false)
			h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
				calls++
				return intakeSnapshot(), nil
			}
			items, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(items) != 1 || items[0].Description != "Complete implementation context" {
				t.Fatalf("retry = %#v, error = %v", items, err)
			}
			claim := h.scheduler.nativeClaims[string(issue.WorkItemID)]
			if len(claim.recovery.Discussion) != 1 || claim.recovery.Issue.LinkedSource.Status != "complete" || claim.recovery.Discussion[0].Provenance.ExternalID != "C_source" {
				t.Fatalf("recovery = %#v", claim.recovery)
			}
			if err := h.native.Release(t.Context(), claim.lease, "released"); err != nil {
				t.Fatal(err)
			}
			completedCalls := calls
			// Credential removal after successful intake must have no effect on
			// another run, comments, Workpad edits, or native source reads.
			h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
				calls++
				return tracker.GitHubIssueSnapshot{}, errors.New("must never be called after intake")
			}
			items, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(items) != 1 {
				t.Fatalf("second run = %#v, error = %v", items, err)
			}
			if err := h.connector.CreateComment(t.Context(), items[0].ID, "## Codex Workpad\nNative progress"); err != nil {
				t.Fatal(err)
			}
			if err := h.connector.UpdateIssueBody(t.Context(), items[0].ID, "Native task edits"); err != nil {
				t.Fatal(err)
			}
			if _, err := h.native.Recovery(t.Context(), issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if calls != completedCalls {
				t.Fatalf("GitHub source calls after intake = %d", calls-completedCalls)
			}
		})
	}
}

func TestNativeEditsDuringSourceFetch(t *testing.T) {
	t.Parallel()
	h, issue := newLinkedChangeHub(t)
	started, proceed := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h.scheduler.githubIntake = func(ctx context.Context, _ string) (tracker.GitHubIssueSnapshot, error) {
		close(started)
		select {
		case <-proceed:
		case <-ctx.Done():
			return tracker.GitHubIssueSnapshot{}, ctx.Err()
		}
		return intakeSnapshot(), nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := h.scheduler.FetchCandidateIssues(ctx, orchestrator.SchedulingRequest{ProjectID: "local", Repository: "acme/orders", Policy: h.descriptor, WorkflowStates: []string{"In Progress"}})
		finished <- err
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("claim ended before source fetch: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	title, body := "Human task title", "Human task body"
	_, editErr := h.admin.UpdateIssue(t.Context(), issue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision, Title: &title, Body: &body})
	close(proceed)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if editErr != nil {
		t.Fatal(editErr)
	}
	recovery := h.scheduler.nativeClaims[string(issue.WorkItemID)].recovery
	if recovery.Issue.Title != title || recovery.Issue.Body != body || recovery.Issue.LinkedSource.Snapshot.Body != "Complete implementation context" || len(recovery.Discussion) != 1 {
		t.Fatalf("native edits or complete source evidence lost during fetch: %#v", recovery)
	}
}
