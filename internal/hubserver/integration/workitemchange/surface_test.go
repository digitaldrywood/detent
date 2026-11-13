package workitemchange_test

import (
	"errors"
	"net/http"
	"testing"

	"fmt"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"net/url"
	"time"
)

// TestWorkItemChangeSurfaceIsOptional pins the two halves of the resource
// contract: the default work item resource is what it always was, and an
// unknown include member is refused rather than ignored.
func TestWorkItemChangeSurfaceIsOptional(t *testing.T) {
	t.Parallel()

	f := newAttemptDiffFixture(t)

	if issue := readWorkItem(t, f.fixture, f.issue.WorkItemID, ""); issue.Change != nil {
		t.Fatalf("change = %#v, want none unless the caller asked for it", *issue.Change)
	}
	requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodGet,
		f.base+"/work-items/"+string(f.issue.WorkItemID)+"?include=changes", f.token, nil),
		http.StatusUnprocessableEntity, "invalid_request")
}

// TestWorkItemChangeSurfaceReportsNoConnector is the seventh dogfood run's
// project: a native project with no GitHub repository bound, where no pull
// request can ever mirror a change. The fact is stated on the resource, so the
// promotion rule does not have to infer it from a missing pull request.
func TestWorkItemChangeSurfaceReportsNoConnector(t *testing.T) {
	t.Parallel()

	f := newAttemptDiffFixture(t)

	change := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change").Change
	if change == nil {
		t.Fatal("change = nil, want the change review surface")
	}
	if change.Connector != tracker.NativeChangeConnectorNone {
		t.Fatalf("connector = %q, want %q", change.Connector, tracker.NativeChangeConnectorNone)
	}
	if change.Revision != 0 {
		t.Fatalf("revision = %d, want 0 before any attempt records a change", change.Revision)
	}
	if change.ChangeID != "" {
		t.Fatalf("change_id = %q, want empty with no change request", change.ChangeID)
	}
}

// TestWorkItemChangeSurfaceReportsAConnectorWithNoPullRequest is the eighth
// dogfood run's project, and the browser preview fixture's: a native project
// that does have a GitHub repository bound and enabled, with a pull request
// projected for a different item. A conversation-driven attempt posts its diff
// and opens no pull request of its own -- opening one is an explicit action
// -- so the surface states a connector, a change at
// the item's current revision, and no pull request at all. That is the answer
// the promotion rule promotes on now; it used to demand a pull request that
// nothing was going to open.
func TestWorkItemChangeSurfaceReportsAConnectorWithNoPullRequest(t *testing.T) {
	t.Parallel()

	f := newAttemptDiffFixture(t)
	other := f.create(t, "Other item with a pull request")
	if err := f.repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = "example-org/example-project", true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	input := hubtest.ChangeInput(hubtest.PolicyDescriptor().ID)
	input.Repository = "https://github.com/example-org/example-project"
	input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/example-org/example-project/pull/1"}
	version := tracker.ChangeVersion{ID: "other_version", ChangeID: "other_change", Number: 1, ChangeVersionInput: input, Policy: hubtest.PolicyDescriptor(), Actor: other.Actor, CreatedAt: now}
	changeRequest := tracker.ChangeRequest{ID: version.ChangeID, OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: other.WorkItemID,
		Revision: 1, Title: "Other item change", CurrentVersion: version.ID, CreatedAt: now, UpdatedAt: now}
	content := eventstore.NewContent(f.details)
	changeRow, err := content.PrepareChange(t.Context(), f.project.OrganizationID, testJSON(t, changeRequest))
	if err != nil {
		t.Fatal(err)
	}
	versionRow, err := content.PrepareVersion(t.Context(), f.project.OrganizationID, testJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: other.WorkItemID, ChangeID: changeRequest.ID, CreatedAt: now}); err != nil {
			return err
		}
		_, err := w.CreateVersion(t.Context(), versionRow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if retained := readWorkItem(t, f.fixture, other.WorkItemID, "?include=change").Change; retained == nil || retained.Number != 1 || retained.ChangeID != changeRequest.ID {
		t.Fatalf("other item did not retain its own pull request: %+v", retained)
	}

	requireNativeStatus(t, f.post(t, f.request(2, tracker.AttemptDiffFile{
		Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 1,
		Patch: "@@ -1 +1 @@\n-return \"Hello, \" + name\n+return \"Hello, \" + name + \"!\"\n",
	})), http.StatusAccepted)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, tracker.NativeRunEvent{
			Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1,
			Data: tracker.NativeRunData{
				Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
				LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt,
				PolicyID: f.policy, Outcome: "succeeded",
			},
		}), http.StatusOK)

	issue := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change")
	if issue.Change == nil {
		t.Fatal("change = nil, want the change review surface")
	}
	if issue.Change.Connector != tracker.NativeChangeConnectorGitHub {
		t.Fatalf("connector = %q, want %q", issue.Change.Connector, tracker.NativeChangeConnectorGitHub)
	}
	if issue.Change.Revision != issue.Revision {
		t.Fatalf("change revision = %d, want the item's own %d", issue.Change.Revision, issue.Revision)
	}
	if issue.Change.ChangeID != "" || issue.Change.Number != 0 {
		t.Fatalf("change = %#v, want no pull request: nothing opened one for this item", *issue.Change)
	}
}

// TestWorkItemChangeSurfaceFollowsTheAttemptDiff is the fact a native
// promotion is judged on: an attempt that succeeded and posted its diff
// covers the item at the revision it was dispatched
// for, which is the same revision the claim-offer rule's claim brake compares.
func TestWorkItemChangeSurfaceFollowsTheAttemptDiff(t *testing.T) {
	t.Parallel()

	f := newAttemptDiffFixture(t)
	item := string(f.issue.WorkItemID)

	// A diff alone is not a recorded change: the attempt is still running, so
	// nothing has answered the item yet.
	requireNativeStatus(t, f.post(t, f.request(2, tracker.AttemptDiffFile{
		Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 1,
		Patch: "@@ -1 +1 @@\n-return \"Hello, \" + name\n+return \"Hello, \" + name + \"!\"\n",
	})), http.StatusAccepted)
	if change := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change").Change; change.Revision != 0 {
		t.Fatalf("revision = %d, want 0 while the attempt is still running", change.Revision)
	}

	// The attempt succeeds. Its recorded work item revision is now the
	// revision the change covers.
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{
			Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt,
			PolicyID: f.policy, Outcome: "succeeded",
		},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/work-items/"+item+"/events", f.worker, event), http.StatusOK)

	issue := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change")
	if issue.Change.Revision != issue.Revision {
		t.Fatalf("change revision = %d, want the item's own %d", issue.Change.Revision, issue.Revision)
	}

	// An edit moves the item on, so the recorded change no longer covers it
	// and the promotion rule stops treating the item as answered -- the same
	// rule that re-offers the item for another attempt.
	title := "Make the unit test pass again"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+item, f.token,
		tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("edit")}, ExpectedRevision: issue.Revision, Title: &title}),
		http.StatusOK)
	edited := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change")
	if edited.Change.Revision >= edited.Revision {
		t.Fatalf("change revision = %d, want below the edited item's %d", edited.Change.Revision, edited.Revision)
	}
}

// TestWorkItemChangeSurfaceReportsTheChangeRequest covers the other source of
// a recorded change: the hub's own change request, reported alone because the
// project has no connector to mirror it.
func TestWorkItemChangeSurfaceReportsTheChangeRequest(t *testing.T) {
	t.Parallel()

	f := newChangeFixture(t)
	version := f.publish(t, "version-1", "")

	change := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change").Change
	if change == nil {
		t.Fatal("change = nil, want the change review surface")
	}
	if change.ChangeID != f.change.ID {
		t.Fatalf("change_id = %q, want %q", change.ChangeID, f.change.ID)
	}
	if change.State != "open" {
		t.Fatalf("state = %q, want open: the hub's change request is open until it merges", change.State)
	}
	if change.Number != 0 {
		t.Fatalf("number = %d, want 0: nothing mirrors this change", change.Number)
	}
	if change.HeadSHA != version.HeadSHA {
		t.Fatalf("head_sha = %q, want the current version's %q", change.HeadSHA, version.HeadSHA)
	}
}

// TestNativeIssueChangeIncluded is the include parser: one member, refused on
// anything else.
func TestNativeIssueChangeIncluded(t *testing.T) {
	t.Parallel()

	f := newChangeFixture(t)
	f.hub.refreshBoard(t, f.project.OrganizationID)
	tests := []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{value: "", want: false},
		{value: "change", want: true},
		{value: " change ", want: true},
		{value: ",", want: false},
		{value: "changes", wantErr: true},
		{value: "change,attempts", wantErr: true},
		{value: "attempts,change", wantErr: true},
	}

	for _, test := range tests {
		t.Run("include="+test.value, func(t *testing.T) {
			t.Parallel()
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"?include="+url.QueryEscape(test.value), f.token, nil)
			var err error
			if response.Code != http.StatusOK {
				err = fmt.Errorf("include response: %s", response.Body.String())
			}
			var issue tracker.NativeIssue
			if err == nil {
				decodeHubResponse(t, response, &issue)
			}
			got := issue.Change != nil
			if (err != nil) != test.wantErr {
				t.Fatalf("nativeIssueChangeIncluded(%q) error = %v, wantErr %t", test.value, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("nativeIssueChangeIncluded(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}

func TestAttemptRecordsTheClaimedRevision(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name            string
		editBeforeStart bool
	}{
		{name: "unchanged item"},
		{name: "edit between claim and start", editBeforeStart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newClaimedAttemptDiffFixture(t)
			claimed := f.issue.Revision
			if test.editBeforeStart {
				title := "Edited after the claim"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token,
					tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("edit")}, ExpectedRevision: claimed, Title: &title}),
					http.StatusOK)
				if edited := readWorkItem(t, f.fixture, f.issue.WorkItemID, ""); edited.Revision <= claimed {
					t.Fatalf("edited revision = %d, want above the claimed %d", edited.Revision, claimed)
				}
			}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("event")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event)
			if test.editBeforeStart {
				requireNativeCode(t, response, http.StatusConflict, "stale_fencing_token")
				if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.Attempt(t.Context(), f.attempt)
					return err
				}); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("rejected start recorded an attempt: %v", err)
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			var recorded tracker.Revision
			if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				attempt, err := r.Attempt(t.Context(), f.attempt)
				recorded = tracker.Revision(attempt.WorkItemRevision)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if recorded != claimed {
				t.Fatalf("attempt revision = %d, want the claimed %d", recorded, claimed)
			}
		})
	}
}

// TestWorkItemChangeSurfaceCreditsOnlyTheFinalDiff credits an attempt's
// stored diff only when it is the diff the attempt finished with and it
// changed something. A clean worktree's empty diff and an earlier checkpoint's
// diff, left behind when the final post failed, cover nothing.
func TestWorkItemChangeSurfaceCreditsOnlyTheFinalDiff(t *testing.T) {
	t.Parallel()

	changed := tracker.AttemptDiffFile{
		Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 1,
		Patch: "@@ -1 +1 @@\n-return \"Hello, \" + name\n+return \"Hello, \" + name + \"!\"\n",
	}
	for _, test := range []struct {
		name    string
		posts   []tracker.AttemptDiffRequest
		covered bool
	}{
		{name: "non-empty diff at the final sequence", posts: []tracker.AttemptDiffRequest{{Generation: tracker.DiffGeneration{Seq: 2}, Files: []tracker.AttemptDiffFile{changed}}}, covered: true},
		{name: "empty diff at the final sequence", posts: []tracker.AttemptDiffRequest{{Generation: tracker.DiffGeneration{Seq: 2}}}},
		{name: "only an earlier checkpoint diff", posts: []tracker.AttemptDiffRequest{{Generation: tracker.DiffGeneration{Seq: 1}, Files: []tracker.AttemptDiffFile{changed}}}},
		{name: "no diff"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newAttemptDiffFixture(t)
			for _, post := range test.posts {
				requireNativeStatus(t, f.post(t, f.request(post.Generation.Seq, post.Files...)), http.StatusAccepted)
			}
			f.event(t, "run.finished", 2, "succeeded")
			issue := readWorkItem(t, f.fixture, f.issue.WorkItemID, "?include=change")
			want := tracker.Revision(0)
			if test.covered {
				want = issue.Revision
			}
			if issue.Change.Revision != want {
				t.Fatalf("change revision = %d, want %d", issue.Change.Revision, want)
			}
		})
	}
}
