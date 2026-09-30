package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	return f.publishReference(t, key, tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: pullRequestTestURL})
}

// publishReference publishes a change version that names reference.
func (f *pullRequestFixture) publishReference(t *testing.T, key string, reference tracker.ChangeExternalReference) tracker.ChangeVersion {
	t.Helper()
	input := changeTestInput()
	input.External = &reference
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

// A change can name a pull request the connector projection has not seen yet.
// Its number comes from the change's own reference, so ?refresh=1 queues
// hydration for exactly that pull request instead of skipping it.
func TestPullRequestRefreshHydratesAnUnprojectedPullRequest(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		reference  tracker.ChangeExternalReference
		wantNumber int
	}{
		{name: "numbered reference", reference: tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://github.com/example/repo/pull/42"}, wantNumber: 42},
		{name: "second unprojected number", reference: tracker.ChangeExternalReference{Provider: "github", ID: "43", URL: "https://github.com/example/repo/pull/43"}, wantNumber: 43},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newPullRequestFixture(t, true)
			f.publishReference(t, "external", test.reference)
			views := f.list(t, "?refresh=1")
			if len(views) != 1 || views[0].Number != test.wantNumber || views[0].Connector == nil {
				t.Fatalf("views = %#v, want one connected row numbered %d", views, test.wantNumber)
			}
			var queued int
			if err := f.service.database.db.QueryRowContext(t.Context(),
				"SELECT count(*) FROM github_hydration_requests WHERE object_kind = 'pull_request' AND object_key = ? AND reason = 'client_refresh'",
				strconv.Itoa(test.wantNumber)).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued != 1 {
				t.Fatalf("queued refreshes for #%d = %d, want 1", test.wantNumber, queued)
			}
		})
	}
}

func TestExternalPullRequestNumber(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		reference tracker.ChangeExternalReference
		want      int
	}{
		{name: "github number", reference: tracker.ChangeExternalReference{Provider: "github", ID: "42"}, want: 42},
		{name: "padded provider and id", reference: tracker.ChangeExternalReference{Provider: " GitHub ", ID: " 7 "}, want: 7},
		{name: "node id", reference: tracker.ChangeExternalReference{Provider: "github", ID: "PR_kwDO"}},
		{name: "zero", reference: tracker.ChangeExternalReference{Provider: "github", ID: "0"}},
		{name: "negative", reference: tracker.ChangeExternalReference{Provider: "github", ID: "-3"}},
		{name: "other provider", reference: tracker.ChangeExternalReference{Provider: "gitlab", ID: "42"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := externalPullRequestNumber(test.reference); got != test.want {
				t.Fatalf("externalPullRequestNumber(%#v) = %d, want %d", test.reference, got, test.want)
			}
		})
	}
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
	comparePullRequestShape(t, "", want, got)
}

// comparePullRequestShape asserts that the Go payload carries exactly the
// golden's key set at every level and agrees on whether a value is null.
// Values themselves are not compared: the golden is an example, the shape is
// the contract.
func comparePullRequestShape(t *testing.T, path string, want, got any) {
	t.Helper()
	label := path
	if label == "" {
		label = "(root)"
	}
	if (want == nil) != (got == nil) {
		t.Errorf("%s: golden null = %t, Go null = %t", label, want == nil, got == nil)
		return
	}
	switch wanted := want.(type) {
	case nil:
	case map[string]any:
		gotten, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: golden is an object, Go value is %T", label, got)
			return
		}
		for key, value := range wanted {
			other, present := gotten[key]
			if !present {
				t.Errorf("%s: Go value is missing key %q", label, key)
				continue
			}
			comparePullRequestShape(t, strings.TrimPrefix(path+"."+key, "."), value, other)
		}
		for key := range gotten {
			if _, present := wanted[key]; !present {
				t.Errorf("%s: Go value has extra key %q", label, key)
			}
		}
	case []any:
		gotten, ok := got.([]any)
		if !ok {
			t.Errorf("%s: golden is an array, Go value is %T", label, got)
			return
		}
		if len(wanted) != len(gotten) {
			t.Errorf("%s: golden has %d elements, Go value has %d", label, len(wanted), len(gotten))
			return
		}
		for i := range wanted {
			comparePullRequestShape(t, path+"[]", wanted[i], gotten[i])
		}
	default:
		if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			t.Errorf("%s: golden is %T, Go value is %T", label, want, got)
		}
	}
}
