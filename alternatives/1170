package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRefreshRereadsItemEvidenceOnlyWhenItChanges(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)
	type reads struct{ attempts, history, changes, comments int }
	steps := []struct {
		name         string
		commentCount int
		activity     time.Time
		state        string
		terminal     bool
		write        bool
		want         reads
	}{
		{name: "first blocked refresh reads evidence", activity: start, state: "Blocked", want: reads{attempts: 1, history: 1, comments: 1}},
		{name: "unchanged blocked item reuses evidence", activity: start, state: "Blocked", want: reads{attempts: 1, history: 1, comments: 1}},
		{name: "new activity rereads evidence", activity: start.Add(time.Minute), state: "Blocked", want: reads{attempts: 2, history: 2, comments: 2}},
		{name: "a write through the client forgets the item", activity: start.Add(time.Minute), state: "Blocked", write: true, want: reads{attempts: 3, history: 3, comments: 3}},
		{name: "first terminal refresh reads changes", activity: start.Add(2 * time.Minute), state: "Done", terminal: true, want: reads{attempts: 3, history: 3, changes: 1, comments: 4}},
		{name: "unchanged terminal item reuses changes", activity: start.Add(2 * time.Minute), state: "Done", terminal: true, want: reads{attempts: 3, history: 3, changes: 1, comments: 4}},
		{name: "600 comments require at most three pages", activity: start.Add(3 * time.Minute), state: "Done", terminal: true, commentCount: 600, want: reads{attempts: 3, history: 3, changes: 2, comments: 7}},
	}
	var mu sync.Mutex
	var got reads
	commentCount, commentLimit := 1, 0
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
			limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil || limit < 1 || limit > 200 {
				t.Errorf("invalid comment limit: %q", r.URL.Query().Get("limit"))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			commentLimit = limit
			offset := 0
			if cursor := r.URL.Query().Get("cursor"); cursor != "" {
				offset, err = strconv.Atoi(cursor)
				if err != nil || offset < 0 || offset >= commentCount {
					t.Errorf("invalid comment cursor: %q", cursor)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			page := tracker.Page[tracker.NativeComment]{}
			end := min(offset+limit, commentCount)
			for i := offset; i < end; i++ {
				page.Items = append(page.Items, tracker.NativeComment{ID: "comment_" + strconv.Itoa(i+1), Body: "held", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}})
			}
			if end < commentCount {
				page.NextCursor = strconv.Itoa(end)
			}
			result = page
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
		commentCount = max(1, step.commentCount)
		previousCommentReads := got.comments
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
		if err != nil || len(comments) != max(1, step.commentCount) {
			t.Fatalf("%s: comments=%+v error=%v", step.name, comments, err)
		}
		for i, comment := range comments {
			if comment.ID != "comment_"+strconv.Itoa(i+1) || comment.Body != "held" || comment.AuthorLogin != "example-owner" || comment.AuthorKind != "human" {
				t.Fatalf("%s: comment %d=%+v", step.name, i, comment)
			}
		}
		mu.Lock()
		current, currentLimit := got, commentLimit
		mu.Unlock()
		if step.commentCount == 600 && (current.comments-previousCommentReads > 3 || currentLimit != 200) {
			t.Fatalf("%s: page requests=%d limit=%d; want <=3 requests with limit=200", step.name, current.comments-previousCommentReads, currentLimit)
		}
		if current != step.want {
			t.Fatalf("%s: reads=%+v want %+v", step.name, current, step.want)
		}
	}
}

func TestNativeRefreshDropsConfirmedMissingItem(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		attemptStatus int
		itemStatus    int
		newHeartbeat  bool
		newSnapshot   bool
		wantGone      bool
	}{
		{name: "missing item", attemptStatus: 404, itemStatus: 404, wantGone: true},
		{name: "new heartbeat survives confirmation", attemptStatus: 404, itemStatus: 404, newHeartbeat: true, wantGone: true},
		{name: "new snapshot survives confirmation", attemptStatus: 404, itemStatus: 404, newHeartbeat: true, newSnapshot: true, wantGone: true},
		{name: "live item missing attempt data", attemptStatus: 404, itemStatus: 200},
		{name: "confirmation unavailable", attemptStatus: 404, itemStatus: 503},
		{name: "confirmation transport failure", attemptStatus: 404, itemStatus: -1},
		{name: "attempts unavailable", attemptStatus: 503, itemStatus: 404},
		{name: "attempts denied", attemptStatus: 403, itemStatus: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_missing", Revision: 1}, State: "Blocked", LastActivityAt: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)}
			attemptReads, itemReads := 0, 0
			var native *NativeClient
			client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/api/v2/organizations/org_test/projects/prj_test/work-items":
					_ = json.NewEncoder(w).Encode(tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{item}})
				case "/api/v2/organizations/org_test/projects/prj_test/work-items/wi_missing/history":
					_ = json.NewEncoder(w).Encode(tracker.Page[tracker.CollaborationEvent]{})
				case "/api/v2/organizations/org_test/projects/prj_test/work-items/wi_missing/attempts":
					attemptReads++
					w.WriteHeader(test.attemptStatus)
					if test.attemptStatus == 200 {
						_ = json.NewEncoder(w).Encode(tracker.Page[tracker.NativeAttempt]{})
					}
				case "/api/v2/organizations/org_test/projects/prj_test/work-items/wi_missing":
					itemReads++
					if test.itemStatus < 0 {
						return nil, errors.New("connection unavailable")
					}
					w.WriteHeader(test.itemStatus)
					if test.newHeartbeat && itemReads == 1 {
						item.Revision++
						item.State = "Todo"
						native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "newer", Items: []runnerauth.HeartbeatItem{{ID: item.WorkItemID, Revision: item.Revision}}})
						test.itemStatus = http.StatusOK
						if test.newSnapshot {
							native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "reloaded", Reset: true})
							if _, _, err := native.heartbeatIssues(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
						return w.Result(), nil
					}
					if test.itemStatus == http.StatusOK {
						_ = json.NewEncoder(w).Encode(item)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
					w.WriteHeader(http.StatusInternalServerError)
				}
				return w.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err = client.Native(item.OrganizationID, item.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "initial", Reset: true})
			c, err := NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				issues, err := c.FetchIssuesByStates(t.Context(), []string{"Blocked"})
				if test.wantGone {
					if err != nil || len(issues) != 0 {
						t.Fatalf("missing refresh: issues=%v error=%v", issues, err)
					}
				} else if err == nil {
					t.Fatal("failed evidence read was accepted")
				}
			}
			wantAttempts, wantItems := 3, 0
			if test.attemptStatus == 404 {
				wantItems = 3
			}
			if test.wantGone {
				wantAttempts, wantItems = 1, 1
			}
			if test.newHeartbeat {
				wantItems = 2
				if test.newSnapshot {
					wantItems = 1
				}
				issues, err := c.FetchIssuesByStates(t.Context(), []string{"Todo"})
				if err != nil || len(issues) != 1 {
					t.Fatalf("new heartbeat item: issues=%v error=%v", issues, err)
				}
			}
			if attemptReads != wantAttempts || itemReads != wantItems {
				t.Fatalf("attempt/item reads = %d/%d, want %d/%d", attemptReads, itemReads, wantAttempts, wantItems)
			}
			if test.wantGone {
				test.attemptStatus = 200
				item.State = "Blocked"
				native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "rediscovered", Reset: true})
				issues, err := c.FetchIssuesByStates(t.Context(), []string{"Blocked"})
				if err != nil || len(issues) != 1 {
					t.Fatalf("rediscovered item: issues=%v error=%v", issues, err)
				}
			}
		})
	}
}
