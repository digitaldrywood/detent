package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// readWorkItem reads one work item, with or without the change review
// surface, through the operator API.
func readWorkItem(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID, query string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id)+query, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

// TestWorkItemChangeSurfaceIsOptional pins the two halves of the resource
// contract: the default work item resource is what it always was, and an
// unknown include member is refused rather than ignored.
func TestWorkItemChangeSurfaceIsOptional(t *testing.T) {
	t.Parallel()

	f := newAttemptDiffFixture(t)

	if issue := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, ""); issue.Change != nil {
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

	change := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change").Change
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
// and opens no pull request of its own -- opening one is the explicit action
// of decisions section 18.6 -- so the surface states a connector, a change at
// the item's current revision, and no pull request at all. That is the answer
// the promotion rule promotes on now; it used to demand a pull request that
// nothing was going to open.
func TestWorkItemChangeSurfaceReportsAConnectorWithNoPullRequest(t *testing.T) {
	t.Parallel()

	f := newAttemptDiffFixture(t)
	repositoryID, _ := seedProjection(t, f.service.database.db)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		// Inserting a repository auto-creates its github_compatible alias
		// project (migration 00008); the native project is the one that owns
		// this repository.
		{"UPDATE projects SET repository_id = NULL WHERE repository_id = ?", []any{repositoryID}},
		{"UPDATE projects SET repository_id = ?, github_repository_enabled = 1 WHERE id = ?", []any{repositoryID, f.project.ID}},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	requireNativeStatus(t, f.post(t, f.request(2, tracker.AttemptDiffFile{
		Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 1,
		Patch: "@@ -1 +1 @@\n-return \"Hello, \" + name\n+return \"Hello, \" + name + \"!\"\n",
	})), http.StatusAccepted)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, tracker.NativeRunEvent{
			Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
			Data: tracker.NativeRunData{
				Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
				LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt,
				PolicyID: f.policy, Outcome: "succeeded",
			},
		}), http.StatusOK)

	issue := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change")
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
// (decisions section 18.5) covers the item at the revision it was dispatched
// for, which is the same revision section 9.2.1's claim brake compares.
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
	if change := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change").Change; change.Revision != 0 {
		t.Fatalf("revision = %d, want 0 while the attempt is still running", change.Revision)
	}

	// The attempt succeeds. Its recorded work item revision is now the
	// revision the change covers.
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{
			Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt,
			PolicyID: f.policy, Outcome: "succeeded",
		},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.base+"/work-items/"+item+"/events", f.worker, event), http.StatusOK)

	issue := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change")
	if issue.Change.Revision != issue.Revision {
		t.Fatalf("change revision = %d, want the item's own %d", issue.Change.Revision, issue.Revision)
	}

	// An edit moves the item on, so the recorded change no longer covers it
	// and the promotion rule stops treating the item as answered -- the same
	// rule that re-offers the item for another attempt.
	title := "Make the unit test pass again"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+item, f.token,
		tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("edit")}, ExpectedRevision: issue.Revision, Title: &title}),
		http.StatusOK)
	edited := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change")
	if edited.Change.Revision >= edited.Revision {
		t.Fatalf("change revision = %d, want below the edited item's %d", edited.Change.Revision, edited.Revision)
	}
}

// TestWorkItemChangeSurfaceReportsTheChangeRequest covers the other source of
// a recorded change: the hub's own change request, reported alone because the
// project has no connector to mirror it (decisions section 18.6).
func TestWorkItemChangeSurfaceReportsTheChangeRequest(t *testing.T) {
	t.Parallel()

	f := newChangeFixture(t, nil)
	version := f.publish(t, "version-1", "")

	change := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change").Change
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
			got, err := nativeIssueChangeIncluded(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("nativeIssueChangeIncluded(%q) error = %v, wantErr %t", test.value, err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("nativeIssueChangeIncluded(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}
