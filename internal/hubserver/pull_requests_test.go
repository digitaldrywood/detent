package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const pullRequestTestURL = "https://github.com/example/repo/pull/1"

// pullRequestFixture is a work item with one change request, optionally joined
// to the GitHub connector's projection of a pull request.
type pullRequestFixture struct {
	changeFixture
	path         string
	now          time.Time
	repositoryID int64
}

func newPullRequestFixture(t *testing.T, connected bool) *pullRequestFixture {
	t.Helper()
	fixture := &pullRequestFixture{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return fixture.now }}
	service := openTestService(t, config)
	fixture.changeFixture = newChangeFixture(t, service)
	fixture.path = fixture.base + "/work-items/" + string(fixture.issue.WorkItemID) + "/pull-requests"
	if !connected {
		return fixture
	}
	repositoryID, _ := seedProjection(t, service.database.db)
	fixture.repositoryID = repositoryID
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE projects SET repository_id = NULL WHERE repository_id = ?", []any{repositoryID}},
		{"UPDATE projects SET repository_id = ?, github_repository_enabled = 1 WHERE id = ?", []any{repositoryID, fixture.project.ID}},
		{"UPDATE issues SET repository_id = ? WHERE native_id = ?", []any{repositoryID, fixture.issue.WorkItemID}},
		{`UPDATE pull_requests SET issue_id = (SELECT id FROM issues WHERE native_id = ?), url = ?, title = 'Connector title',
head_sha = ?, head_ref = 'feature', base_ref = 'main', draft = 0, mergeable_state = 'clean',
checks_summary_json = '{"status":"completed","conclusion":"success","total":1,"passed":1}',
reviews_summary_json = '{"decision":"approved","approvals":1}'`,
			[]any{fixture.issue.WorkItemID, pullRequestTestURL, strings.Repeat("b", 40)}},
	} {
		if _, err := service.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

// publishExternal publishes a change version that names the pull request.
func (f *pullRequestFixture) publishExternal(t *testing.T, key string) tracker.ChangeVersion {
	t.Helper()
	input := changeTestInput()
	input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: pullRequestTestURL}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.changeFixture.path+"/versions", f.token,
		tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ChangeVersionInput: input})
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	return version
}

func (f *pullRequestFixture) list(t *testing.T, query string) []pullRequestView {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+query, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var views []pullRequestView
	decodeHubResponse(t, response, &views)
	return views
}

func (f *pullRequestFixture) action(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, body)
}

func TestPullRequestsWithoutConnector(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, false)
	f.publish(t, "v1", "")
	views := f.list(t, "")
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	view := views[0]
	if view.Connector != nil {
		t.Fatalf("connector = %#v, want null", view.Connector)
	}
	if view.ChangeID != f.change.ID || view.Number != 0 || view.State != "open" {
		t.Fatalf("view = %#v", view)
	}
	if view.Head.SHA != strings.Repeat("b", 40) {
		t.Fatalf("head sha = %q", view.Head.SHA)
	}
	if view.Mergeable.Known {
		t.Fatalf("mergeable = %#v, want unknown", view.Mergeable)
	}
	if view.Checks == nil || view.Reviews == nil || view.Labels == nil {
		t.Fatalf("arrays must be present and empty: %#v", view)
	}
}

// The connector half fills in state, draft, head, base, mergeability and the
// review decision, and fetched_at is when the projection was synchronized.
func TestPullRequestsJoinConnector(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	version := f.publishExternal(t, "external")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.changeFixture.path+"/versions/"+version.ID+"/reviews", f.token,
		tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.changeFixture.path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)

	views := f.list(t, "")
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	view := views[0]
	if view.Connector == nil || view.Connector.Provider != "github" || view.Connector.Repository != "digitaldrywood/detent" {
		t.Fatalf("connector = %#v", view.Connector)
	}
	if view.Number != 1 || view.URL != pullRequestTestURL || view.State != "open" || view.Draft {
		t.Fatalf("view = %#v", view)
	}
	if view.Head.Ref != "feature" || view.Head.SHA != strings.Repeat("b", 40) || view.Base.Ref != "main" {
		t.Fatalf("refs = %#v %#v", view.Head, view.Base)
	}
	if view.Head.Repository != "digitaldrywood/detent" || view.FromFork {
		t.Fatalf("head repository = %q from_fork = %v", view.Head.Repository, view.FromFork)
	}
	if !view.Mergeable.Known || !view.Mergeable.Value {
		t.Fatalf("mergeable = %#v, want true", view.Mergeable)
	}
	if view.ReviewDecision != "approved" {
		t.Fatalf("review decision = %q", view.ReviewDecision)
	}
	if len(view.Checks) != 1 || view.Checks[0].Conclusion != "success" || view.Checks[0].Status != "completed" {
		t.Fatalf("checks = %#v", view.Checks)
	}
	if len(view.Reviews) != 1 || view.Reviews[0].State != "approved" || view.Reviews[0].Author == "" {
		t.Fatalf("reviews = %#v", view.Reviews)
	}
	if view.FetchedAt.IsZero() {
		t.Fatal("fetched_at must name when the connector view was taken")
	}
}

// mergeable is 18.6's three values on the wire: true, false or "unknown".
func TestPullRequestMergeableJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{name: "clean", state: "clean", want: "true"},
		{name: "blocked is still mergeable as far as the projection knows", state: "blocked", want: "true"},
		{name: "dirty", state: "dirty", want: "false"},
		{name: "empty", state: "", want: `"unknown"`},
		{name: "unknown", state: "UNKNOWN", want: `"unknown"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(mergeableFromState(test.state))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("mergeable = %s, want %s", encoded, test.want)
			}
			var back pullRequestMergeable
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("decode %s: %v", encoded, err)
			}
			if back != mergeableFromState(test.state) {
				t.Fatalf("round trip = %#v", back)
			}
			if err := json.Unmarshal([]byte(`"maybe"`), &back); err == nil {
				t.Fatal("an unknown string must be refused")
			}
		})
	}
}

// "?refresh=1 forces a fetch, limited to one per 10 seconds per organization."
func TestPullRequestRefreshLimit(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	f.publishExternal(t, "external")
	if views := f.list(t, "?refresh=1"); len(views) != 1 {
		t.Fatalf("views = %d", len(views))
	}
	var queued int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM github_hydration_requests WHERE object_kind = 'pull_request' AND reason = 'client_refresh'").Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued refreshes = %d, want 1", queued)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?refresh=1", f.token, nil)
	requireNativeCode(t, response, http.StatusTooManyRequests, "refresh_limited")
	f.now = f.now.Add(pullRequestRefreshInterval)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?refresh=1", f.token, nil), http.StatusOK)
	requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?refresh=2", f.token, nil),
		http.StatusUnprocessableEntity, "invalid_request")
}

// The assembled view is served from memory for sixty seconds, so a client that
// polls the panel does not re-run the join on every tick.
func TestPullRequestViewCache(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	f.publishExternal(t, "external")
	first := f.list(t, "")
	if len(first) != 1 || first[0].Number != 1 {
		t.Fatalf("first = %#v", first)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET github_state = 'closed'"); err != nil {
		t.Fatal(err)
	}
	cached := f.list(t, "")
	if len(cached) != 1 || cached[0].State != "open" {
		t.Fatalf("cached views = %#v, want one still-open row", cached)
	}
	f.now = f.now.Add(pullRequestViewTTL)
	fresh := f.list(t, "")
	if len(fresh) != 1 || fresh[0].State != "closed" {
		t.Fatalf("views after the TTL = %#v, want one closed row", fresh)
	}
}

// Actions become one work item the merge queue claims, and are idempotent by
// key like every other mutation.
func TestPullRequestActionsQueueWork(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	version := f.publishExternal(t, "external")
	body := map[string]any{"idempotency_key": "open-one", "action": "open", "expected_head_sha": version.HeadSHA}
	response := f.action(t, f.path+"/actions", body)
	requireNativeStatus(t, response, http.StatusAccepted)
	var accepted pullRequestActionResponse
	decodeHubResponse(t, response, &accepted)
	if accepted.ActionID == "" || accepted.WorkItem.WorkItemID == "" {
		t.Fatalf("accepted = %#v", accepted)
	}
	if !contains(accepted.WorkItem.Labels, pullRequestActionLabel) {
		t.Fatalf("labels = %v", accepted.WorkItem.Labels)
	}
	replay := f.action(t, f.path+"/actions", body)
	requireNativeStatus(t, replay, http.StatusAccepted)
	var replayed pullRequestActionResponse
	decodeHubResponse(t, replay, &replayed)
	if replayed.ActionID != accepted.ActionID || replayed.WorkItem.WorkItemID != accepted.WorkItem.WorkItemID {
		t.Fatalf("a replayed key opened a second action: %#v", replayed)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM pull_request_actions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("actions = %d, want 1", count)
	}
}

// "A head that moved since expected_head_sha fails the action with head_moved."
func TestPullRequestActionHeadMoved(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	f.publishExternal(t, "external")
	response := f.action(t, f.path+"/1/actions",
		map[string]any{"idempotency_key": "update-stale", "action": "update_branch", "expected_head_sha": strings.Repeat("f", 40)})
	requireNativeCode(t, response, http.StatusConflict, "head_moved")
	requireNativeStatus(t, f.action(t, f.path+"/1/actions",
		map[string]any{"idempotency_key": "update-current", "action": "update_branch", "expected_head_sha": strings.Repeat("b", 40)}), http.StatusAccepted)
}

// The same head_moved rule on the `open` route, which learns the head from a
// different place: there is no pull request yet, so the head the hub compares
// against is the change request's current version rather than the connector's
// projection. A test on the numbered route proves the connector branch and
// nothing at all about this one.
func TestPullRequestOpenActionHeadMoved(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	version := f.publishExternal(t, "external")
	requireNativeCode(t, f.action(t, f.path+"/actions",
		map[string]any{"idempotency_key": "open-stale", "action": "open", "expected_head_sha": strings.Repeat("f", 40)}),
		http.StatusConflict, "head_moved")
	requireNativeStatus(t, f.action(t, f.path+"/actions",
		map[string]any{"idempotency_key": "open-current", "action": "open", "expected_head_sha": version.HeadSHA}),
		http.StatusAccepted)
}

// addMergeLane gives the fixture's project the dispatchable "Merging" state
// section 18.6's actions are created in.
//
// It writes the project's states and its workflow_states row directly because
// a project's states are fixed at creation: there is no endpoint that adds a
// lane, and creating a second project would leave the change request, the
// connector projection and the token grant behind on the first one.
func (f *pullRequestFixture) addMergeLane(t *testing.T) {
	t.Helper()
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Merging", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	encoded, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	now := formatHubTime(f.now)
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE projects SET states_json = ? WHERE id = ?", string(encoded), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO workflow_states
 (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
VALUES (?, 'Merging', 'Merging', 0, 1, ?, ?)`, f.project.ID, now, now); err != nil {
		t.Fatal(err)
	}
}

// The action lands in the project's merge lane, and it lands there as an item
// the queue can actually claim.
//
// mergeLaneState is unit-tested on its own, and what a unit test of it cannot
// show is the end of the path: a lane chosen from this project's own states, a
// work item created in it, and the queue_entries row behind it. Section 18.6
// says there is no second queue, so an action issue with no queue row would be
// an issue nobody drains -- which is indistinguishable from a queued action
// until somebody waits for it.
func TestPullRequestActionLandsInTheMergeLane(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	f.addMergeLane(t)
	version := f.publishExternal(t, "external")
	response := f.action(t, f.path+"/actions",
		map[string]any{"idempotency_key": "open-merging", "action": "open", "expected_head_sha": version.HeadSHA})
	requireNativeStatus(t, response, http.StatusAccepted)
	var accepted pullRequestActionResponse
	decodeHubResponse(t, response, &accepted)
	if accepted.WorkItem.State != "Merging" {
		t.Fatalf("action state = %q, want the project's merge lane", accepted.WorkItem.State)
	}
	var queued int
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*) FROM queue_entries q
JOIN issues i ON i.id = q.issue_id
JOIN workflow_states w ON w.id = q.workflow_state_id
WHERE i.native_id = ? AND q.state = 'Merging' AND w.source_name = 'Merging' AND w.dispatchable = 1`,
		accepted.WorkItem.WorkItemID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("claimable queue entries in the merge lane = %d, want 1", queued)
	}
}

// "merge additionally follows the change-request merge policy."
func TestPullRequestMergeFollowsPolicy(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	version := f.publishExternal(t, "external")
	head := strings.Repeat("b", 40)
	requireNativeCode(t, f.action(t, f.path+"/1/actions",
		map[string]any{"idempotency_key": "merge-early", "action": "merge", "expected_head_sha": head}),
		http.StatusConflict, "merge_not_permitted")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.changeFixture.path+"/versions/"+version.ID+"/reviews", f.token,
		tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost,
		f.changeFixture.path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
	requireNativeStatus(t, f.action(t, f.path+"/1/actions",
		map[string]any{"idempotency_key": "merge-reviewed", "action": "merge", "expected_head_sha": head}), http.StatusAccepted)
}

func TestPullRequestActionValidation(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, true)
	version := f.publishExternal(t, "external")
	tests := []struct {
		name   string
		path   string
		body   map[string]any
		status int
		code   string
	}{
		{
			name: "unknown action", path: f.path + "/actions",
			body:   map[string]any{"idempotency_key": "a", "action": "rebase", "expected_head_sha": version.HeadSHA},
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name: "open addressed by number", path: f.path + "/1/actions",
			body:   map[string]any{"idempotency_key": "b", "action": "open", "expected_head_sha": version.HeadSHA},
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name: "update_branch addressed by issue", path: f.path + "/actions",
			body:   map[string]any{"idempotency_key": "c", "action": "update_branch", "expected_head_sha": version.HeadSHA},
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name: "missing expected head", path: f.path + "/actions",
			body:   map[string]any{"idempotency_key": "d", "action": "open"},
			status: http.StatusUnprocessableEntity, code: "invalid_request",
		},
		{
			name: "unknown pull request number", path: f.path + "/99/actions",
			body:   map[string]any{"idempotency_key": "e", "action": "update_branch", "expected_head_sha": version.HeadSHA},
			status: http.StatusNotFound, code: "not_found",
		},
		{
			name: "non numeric number", path: f.path + "/abc/actions",
			body:   map[string]any{"idempotency_key": "f", "action": "update_branch", "expected_head_sha": version.HeadSHA},
			status: http.StatusNotFound, code: "not_found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireNativeCode(t, f.action(t, test.path, test.body), test.status, test.code)
		})
	}
}

// The action label is the hub's to set: a tracker writer that could set it
// would turn any issue into a merge-lane run that produces no deliverable.
func TestPullRequestActionLabelIsReserved(t *testing.T) {
	t.Parallel()
	f := newPullRequestFixture(t, false)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token,
		tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved"}, Title: "mine", Body: "body", State: "Todo", Labels: []string{pullRequestActionLabel}})
	requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
}

// mergeLaneState puts the action in the project's merge lane when it has one,
// so the existing merge queue drains it rather than a second queue.
func TestMergeLaneState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		states []tracker.NativeState
		want   string
		found  bool
	}{
		{
			name:   "merge lane wins",
			states: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Merging", Dispatchable: true}},
			want:   "Merging", found: true,
		},
		{
			name:   "case insensitive",
			states: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "merging", Dispatchable: true}},
			want:   "merging", found: true,
		},
		{
			name:   "a non-dispatchable merge lane is not a lane a runner can claim",
			states: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Merging"}},
			want:   "Todo", found: true,
		},
		{
			name:   "falls back to the first dispatchable state",
			states: []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}},
			want:   "Todo", found: true,
		},
		{name: "no dispatchable state", states: []tracker.NativeState{{Name: "Done", Terminal: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, found := mergeLaneState(tracker.NativeProject{States: test.states})
			if got != test.want || found != test.found {
				t.Fatalf("mergeLaneState = %q, %v, want %q, %v", got, found, test.want, test.found)
			}
		})
	}
}

// TestPullRequestViewGoldenShape pins the 18.6 shape on the Go side, so the
// client has a contract to decode against before it draws the panel.
func TestPullRequestViewGoldenShape(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "pull-requests.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var views []pullRequestView
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&views); err != nil {
		t.Fatalf("the golden must decode into the served shape: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("golden views = %d, want 2", len(views))
	}
	if views[0].Connector == nil || views[0].Number != 4211 || !views[0].Mergeable.Known || !views[0].Mergeable.Value {
		t.Fatalf("connected row = %#v", views[0])
	}
	if views[1].Connector != nil || views[1].Mergeable.Known {
		t.Fatalf("unavailable row = %#v", views[1])
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	compareConversationShape(t, "", want, got, map[string]bool{}, map[string]bool{})
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
