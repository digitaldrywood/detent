package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatOversizedIssuePages(t *testing.T) {
	for _, test := range []struct {
		name            string
		reset           bool
		failLater       bool
		singleOversized bool
		repeatCursor    bool
		denied          bool
	}{
		{name: "initial board"},
		{name: "reset board", reset: true},
		{name: "later page failure preserves cache", reset: true, failLater: true},
		{name: "single oversized issue fails", singleOversized: true},
		{name: "repeated cursor preserves cache", reset: true, repeatCursor: true},
		{name: "oversized denial is not retried", denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			items := make([]tracker.NativeIssue, 182)
			for i := range items {
				items[i] = tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", i)), Number: i + 1, Revision: 3}, State: "Todo", Body: strings.Repeat("x", 12<<10)}
			}
			if test.singleOversized {
				items = items[:1]
				items[0].Body = strings.Repeat("x", maxResponseBytes)
			}
			calls := 0
			laterRead := false
			client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				w := httptest.NewRecorder()
				limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
				if err != nil || limit < 1 {
					t.Fatalf("invalid page limit: %s", r.URL.RawQuery)
				}
				start := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					laterRead = true
					start, err = strconv.Atoi(cursor)
					if err != nil {
						t.Fatal(err)
					}
				}
				if test.failLater && start > 0 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.WriteString(`{"code":"unavailable"}`)
					return w.Result(), nil
				}
				end := min(start+limit, len(items))
				page := tracker.Page[tracker.NativeIssue]{Items: items[start:end]}
				if end < len(items) {
					page.NextCursor = strconv.Itoa(end)
				}
				if test.repeatCursor && start > 0 {
					page.NextCursor = strconv.Itoa(start)
				}
				if test.denied {
					w.WriteHeader(http.StatusForbidden)
				}
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Fatal(err)
				}
				return w.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			state := native.heartbeatProject()
			prior := tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_prior"}, Body: "preserved"}
			if test.reset {
				state.items = map[tracker.NativeWorkItemID]tracker.NativeIssue{prior.WorkItemID: prior}
				state.loaded = true
				state.cursor = "prior"
			}
			native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "current", Reset: test.reset})
			got, supported, err := native.heartbeatIssues(t.Context())
			if test.failLater || test.singleOversized || test.repeatCursor || test.denied {
				if err == nil || !supported || got != nil {
					t.Fatalf("oversized/failing page succeeded: count=%d supported=%v error=%v", len(got), supported, err)
				}
				if test.reset && (state.cursor != "prior" || len(state.items) != 1 || state.items[prior.WorkItemID].Body != "preserved") {
					t.Fatal("failed reset installed a partial cache")
				}
				if calls > 10 {
					t.Fatalf("unbounded page retries: %d", calls)
				}
				if (test.failLater || test.repeatCursor) && !laterRead {
					t.Fatal("failure did not reach a later page")
				}
				if test.denied && calls != 1 {
					t.Fatalf("retried a denied response: %d calls", calls)
				}
				return
			}
			if err != nil || !supported || len(got) != len(items) {
				t.Fatalf("hydrate board: count=%d supported=%v error=%v", len(got), supported, err)
			}
			for i, item := range got {
				if item.WorkItemID != items[i].WorkItemID || item.Revision != 3 || item.Body != items[i].Body {
					t.Fatalf("item %d lost identity, revision or body", i)
				}
			}
			before := calls
			if _, _, err := native.heartbeatIssues(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != before {
				t.Fatal("unchanged board was fetched again")
			}
		})
	}
}

func TestHeartbeatDeltaApplication(t *testing.T) {
	start := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	item := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: "wi_delta", Number: 1, Revision: 1}, Title: "initial", State: "Todo", LastActivityAt: start}
	lists, details := 0, 0
	fail := false
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodPost:
			result = item
		case strings.HasSuffix(r.URL.Path, "/work-items"):
			lists++
			result = tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{item}}
		default:
			details++
			if fail {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.WriteString(`{"code":"unavailable"}`)
				return w.Result(), nil
			}
			result = item
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	c := &NativeConnector{client: native}
	for _, test := range []struct {
		name      string
		reset     bool
		changed   bool
		write     bool
		fail      bool
		fallback  bool
		wantLists int
		wantReads int
		wantTitle string
	}{
		{name: "initial reset", reset: true, wantLists: 1, wantTitle: "initial"},
		{name: "unchanged", wantLists: 1, wantTitle: "initial"},
		{name: "changed item", changed: true, wantLists: 1, wantReads: 1, wantTitle: "changed item"},
		{name: "failed hydration", changed: true, fail: true, wantLists: 1, wantReads: 2},
		{name: "retry retained delta", wantLists: 1, wantReads: 3, wantTitle: "failed hydration"},
		{name: "cursor reset", reset: true, wantLists: 2, wantReads: 3, wantTitle: "failed hydration"},
		{name: "reset acknowledged once", wantLists: 2, wantReads: 3, wantTitle: "failed hydration"},
		{name: "read own mutation", write: true, wantLists: 2, wantReads: 4, wantTitle: "read own mutation"},
		{name: "legacy fallback", fallback: true, wantLists: 3, wantReads: 4, wantTitle: "read own mutation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := native.changeCursor()
			changes := &runnerauth.HeartbeatChanges{Cursor: test.name, Reset: test.reset, CapabilitiesDigest: "digest"}
			if test.changed || test.write {
				item.Title, item.Revision, item.LastActivityAt = test.name, item.Revision+1, item.LastActivityAt.Add(time.Second)
			}
			if test.changed {
				changes.Items = []runnerauth.HeartbeatItem{{ID: item.WorkItemID, Revision: item.Revision, LastActivityAt: item.LastActivityAt}}
			}
			if test.fallback {
				changes = nil
			}
			native.ApplyHeartbeatChanges(changes)
			if test.write {
				if _, err := native.Dependency(t.Context(), item.WorkItemID, tracker.DependencyMutation{}); err != nil {
					t.Fatal(err)
				}
			}
			fail = test.fail
			issues, err := c.FetchIssuesByStates(t.Context(), []string{"Todo"})
			if test.fail {
				if err == nil || native.changeCursor() != previous {
					t.Fatalf("failed delta acknowledged: cursor=%q error=%v", native.changeCursor(), err)
				}
			} else if err != nil || len(issues) != 1 || issues[0].Title != test.wantTitle {
				t.Fatalf("issues=%+v error=%v", issues, err)
			}
			if lists != test.wantLists || details != test.wantReads {
				t.Fatalf("lists/details=%d/%d want=%d/%d", lists, details, test.wantLists, test.wantReads)
			}
		})
	}
}

func TestHeartbeatClaimHints(t *testing.T) {
	client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "test" }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		changes   *runnerauth.HeartbeatChanges
		freed     bool
		want      bool
		wantAgain bool
	}{
		{name: "legacy", want: true, wantAgain: true},
		{name: "idle", changes: &runnerauth.HeartbeatChanges{Cursor: "idle"}},
		{name: "claimable", changes: &runnerauth.HeartbeatChanges{Cursor: "ready", Claimable: true}, want: true, wantAgain: true},
		{name: "slot freed", changes: &runnerauth.HeartbeatChanges{Cursor: "idle"}, freed: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			native.ApplyHeartbeatChanges(test.changes)
			if test.freed {
				native.SlotFreed()
			}
			if first, second := native.ClaimHint(), native.ClaimHint(); first != test.want || second != test.wantAgain {
				t.Fatalf("claim permission=%v/%v want=%v/%v", first, second, test.want, test.wantAgain)
			}
		})
	}
}
