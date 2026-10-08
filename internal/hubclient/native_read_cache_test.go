package hubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRefreshRereadsItemEvidenceOnlyWhenItChanges(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)
	type reads struct{ attempts, history, changes, comments int }
	steps := []struct {
		name     string
		activity time.Time
		state    string
		terminal bool
		write    bool
		want     reads
	}{
		{name: "first blocked refresh reads evidence", activity: start, state: "Blocked", want: reads{attempts: 1, history: 1, comments: 1}},
		{name: "unchanged blocked item reuses evidence", activity: start, state: "Blocked", want: reads{attempts: 1, history: 1, comments: 1}},
		{name: "new activity rereads evidence", activity: start.Add(time.Minute), state: "Blocked", want: reads{attempts: 2, history: 2, comments: 2}},
		{name: "a write through the client forgets the item", activity: start.Add(time.Minute), state: "Blocked", write: true, want: reads{attempts: 3, history: 3, comments: 3}},
		{name: "first terminal refresh reads changes", activity: start.Add(2 * time.Minute), state: "Done", terminal: true, want: reads{attempts: 3, history: 3, changes: 1, comments: 4}},
		{name: "unchanged terminal item reuses changes", activity: start.Add(2 * time.Minute), state: "Done", terminal: true, want: reads{attempts: 3, history: 3, changes: 1, comments: 4}},
	}
	var mu sync.Mutex
	var got reads
	native := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_cache", Profile: "native", Number: 9, Revision: 4}, CreatedAt: start, UpdatedAt: start}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodPost:
			result = native
		case strings.HasSuffix(r.URL.Path, "/attempts"):
			got.attempts++
			result = tracker.Page[tracker.NativeAttempt]{}
		case strings.HasSuffix(r.URL.Path, "/history"):
			got.history++
			result = tracker.Page[tracker.CollaborationEvent]{}
		case strings.HasSuffix(r.URL.Path, "/changes"):
			got.changes++
			result = []tracker.ChangeRequest{}
		case strings.HasSuffix(r.URL.Path, "/comments"):
			got.comments++
			result = tracker.Page[tracker.NativeComment]{Items: []tracker.NativeComment{{ID: "comment_1", Body: "held", Actor: tracker.Actor{Kind: "human", PrincipalID: "cory"}}}}
		case strings.HasSuffix(r.URL.Path, "/work-items"):
			result = tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{native}}
		default:
			t.Errorf("unexpected read: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := client.Native(native.OrganizationID, native.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewNativeConnector(scoped)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		mu.Lock()
		native.LastActivityAt, native.State, native.Terminal = step.activity, step.state, step.terminal
		mu.Unlock()
		if step.write {
			if _, err := scoped.Dependency(t.Context(), native.WorkItemID, tracker.DependencyMutation{}); err != nil {
				t.Fatalf("%s: write: %v", step.name, err)
			}
		}
		issues, err := c.FetchIssuesByStates(t.Context(), []string{step.state})
		if err != nil || len(issues) != 1 {
			t.Fatalf("%s: refresh count=%d error=%v", step.name, len(issues), err)
		}
		comments, err := c.FetchIssueComments(t.Context(), connector.Issue{ID: string(native.WorkItemID)})
		if err != nil || len(comments) != 1 || comments[0].Body != "held" {
			t.Fatalf("%s: comments=%+v error=%v", step.name, comments, err)
		}
		mu.Lock()
		current := got
		mu.Unlock()
		if current != step.want {
			t.Fatalf("%s: reads=%+v want %+v", step.name, current, step.want)
		}
	}
}
