package hubclient

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeMachineIntakeAuthorityAndFingerprint(t *testing.T) {
	t.Parallel()
	states := append(hubserver.HostedProjectStates(), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Done"}})
	h := newNativeChangeHubWithStates(t, "Human Review", states)
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, h.admin.client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "native-intake", Capacity: 1, Version: "test"}
	if _, err := EnrollRunner(t.Context(), identityPath, h.organization, enrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	store, ok := any(conn).(intake.IssueStore)
	if !ok {
		t.Fatal("native worker has no existing machine intake owner")
	}
	for range 22 {
		if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Earlier work", State: "Backlog"}); err != nil {
			t.Fatal(err)
		}
	}
	origin := issueorigin.Origin{Kind: "worker", Source: "attempt-1", Fingerprint: "pool-live-acceptance"}
	draft := intake.IssueDraft{Title: "Verify integrated pool", Body: issueorigin.Stamp("Pending real two-Sprite acceptance.\n<!-- acceptance-owner -->", origin), Labels: []string{"acceptance"}}
	created, err := store.CreateIntakeIssue(t.Context(), draft)
	if err != nil || created.ID == "" || created.Reused {
		t.Fatalf("create = %#v, error = %v", created, err)
	}
	current, err := native.Issue(t.Context(), tracker.NativeWorkItemID(created.ID))
	if err != nil || current.State != "Backlog" {
		t.Fatalf("follow-up entered a dispatchable first workflow state: %#v, %v", current, err)
	}
	found, matched, err := store.FindIntakeIssue(t.Context(), "<!-- acceptance-owner -->")
	if err != nil || !matched || found.ID != created.ID {
		t.Fatalf("paginated match = %#v, %t, %v", found, matched, err)
	}
	origin.Source = "attempt-2"
	draft.Body = issueorigin.Stamp("Second occurrence", origin)
	reused, err := store.CreateIntakeIssue(t.Context(), draft)
	if err != nil || !reused.Reused || reused.ID != created.ID {
		t.Fatalf("same fingerprint = %#v, %v", reused, err)
	}
	comments, err := native.Comments(t.Context(), tracker.NativeWorkItemID(created.ID), "")
	if err != nil || len(comments.Items) != 1 || !strings.Contains(comments.Items[0].Body, "## New machine occurrence") {
		t.Fatalf("occurrence = %#v, %v", comments, err)
	}
	updated, err := store.UpdateIntakeIssue(t.Context(), created.ID, intake.IssueDraft{Title: "Verify deployed pool", Body: "Updated criteria", Labels: []string{"deployment"}})
	if err != nil {
		t.Fatal(err)
	}
	updatedOrigin, stamped := issueorigin.Parse(updated.Body)
	if !stamped || updatedOrigin.Source != "attempt-1" || updatedOrigin.Fingerprint != origin.Fingerprint {
		t.Fatalf("origin replaced: %#v", updatedOrigin)
	}
	current, err = native.Issue(t.Context(), tracker.NativeWorkItemID(created.ID))
	if err != nil || !slices.Equal(current.Labels, []string{"acceptance", "deployment"}) {
		t.Fatalf("metadata = %#v, %v", current.Labels, err)
	}
	if err := store.SetIntakeIssueState(t.Context(), created.ID, "Backlog"); err != nil {
		t.Fatal(err)
	}
	var foreignProject tracker.NativeProject
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", map[string]any{"name": "foreign", "idempotency_key": "foreign-intake", "states": states}, &foreignProject); err != nil {
		t.Fatal(err)
	}
	foreign, err := client.Native(h.organization, foreignProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreignConnector, err := NewNativeConnector(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignStore, ok := any(foreignConnector).(intake.IssueStore)
	if !ok {
		t.Fatal("foreign connector lost the shared intake interface")
	}
	_, err = foreignStore.CreateIntakeIssue(t.Context(), draft)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("foreign authority = %v", err)
	}
	readOnlyPath := filepath.Join(t.TempDir(), "private", "identity.json")
	readOnlyFile, err := runnerauth.Initialize(readOnlyPath, h.admin.client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	readOnlyEnrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: readOnlyFile.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine.ID = readOnlyFile.Identity.MachineID
	if _, err := EnrollRunner(t.Context(), readOnlyPath, h.organization, readOnlyEnrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	readOnlyClient, err := New(Config{URL: readOnlyFile.HubURL, IdentityFile: readOnlyPath})
	if err != nil {
		t.Fatal(err)
	}
	readOnlyNative, err := readOnlyClient.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyConnector, err := NewNativeConnector(readOnlyNative)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyStore, ok := any(readOnlyConnector).(intake.IssueStore)
	if !ok {
		t.Fatal("read-only connector lost the shared intake interface")
	}
	origin.Fingerprint = "missing-collaboration-permission"
	draft.Body = issueorigin.Stamp("Cannot delegate without filing authority", origin)
	_, err = readOnlyStore.CreateIntakeIssue(t.Context(), draft)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("read-only runner published follow-up: %v", err)
	}
}

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
