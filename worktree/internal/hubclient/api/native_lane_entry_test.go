package api

import (
	"context"
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
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeConnectorCurrentLaneEntry(t *testing.T) {
	t.Parallel()
	blocked := time.Date(2026, 10, 8, 9, 41, 0, 0, time.UTC)
	todo := time.Date(2026, 10, 8, 18, 7, 12, 0, time.UTC)
	transition := func(id, to string, at time.Time) tracker.CollaborationEvent {
		return tracker.CollaborationEvent{ID: id, Type: "workflow.transitioned", RecordedAt: at, Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, Data: tracker.CollaborationData{ToState: to}}
	}
	comment := tracker.CollaborationEvent{ID: "comment", Type: "comment.created", RecordedAt: todo.Add(time.Minute)}
	boundary := make([][]tracker.CollaborationEvent, nativeLaneEntryHistoryPages)
	for index := range boundary {
		boundary[index] = []tracker.CollaborationEvent{comment}
	}
	incomplete := append(append([][]tracker.CollaborationEvent(nil), boundary...), []tracker.CollaborationEvent{transition("older", "Todo", blocked)})
	boundary[len(boundary)-1] = []tracker.CollaborationEvent{transition("boundary", "Todo", todo)}
	tests := []struct {
		name              string
		state             string
		revision          string
		pages             [][]tracker.CollaborationEvent
		calls             int
		wantFound         bool
		wantEntered       time.Time
		wantReads         int
		wantError         bool
		advanceRevision   bool
		secondPages       [][]tracker.CollaborationEvent
		wantSecondEntered time.Time
		wantSecondError   bool
	}{
		{
			name:        "latest transition into current lane",
			state:       "Todo",
			revision:    "7",
			pages:       [][]tracker.CollaborationEvent{{comment, transition("todo", "Todo", todo), transition("blocked", "Blocked", blocked)}},
			calls:       1,
			wantFound:   true,
			wantEntered: todo,
			wantReads:   1,
		},
		{
			name:        "newest transition follows newer comments",
			state:       "todo",
			revision:    "7",
			pages:       [][]tracker.CollaborationEvent{{comment}, {transition("todo", "Todo", todo), transition("blocked", "Blocked", blocked)}, {transition("todo-old", "Todo", blocked.Add(-time.Hour))}},
			calls:       1,
			wantFound:   true,
			wantEntered: todo,
			wantReads:   2,
		},
		{
			name:      "latest transition names another lane",
			state:     "Todo",
			revision:  "7",
			pages:     [][]tracker.CollaborationEvent{{transition("blocked", "Blocked", todo), transition("todo", "Todo", blocked)}},
			calls:     1,
			wantFound: false,
			wantReads: 1,
		},
		{
			name:        "unchanged revision reuses the history read",
			state:       "Todo",
			revision:    "7",
			pages:       [][]tracker.CollaborationEvent{{transition("todo", "Todo", todo)}},
			calls:       3,
			wantFound:   true,
			wantEntered: todo,
			wantReads:   1,
		},
		{
			name:        "missing revision rereads history",
			state:       "Todo",
			pages:       [][]tracker.CollaborationEvent{{transition("todo", "Todo", todo)}},
			calls:       2,
			wantFound:   true,
			wantEntered: todo,
			wantReads:   2,
		},
		{name: "no workflow transition", state: "Todo", revision: "7", pages: [][]tracker.CollaborationEvent{{comment}}, calls: 1, wantReads: 1},
		{name: "transition at last allowed page", state: "Todo", revision: "7", pages: boundary, calls: 1, wantFound: true, wantEntered: todo, wantReads: nativeLaneEntryHistoryPages},
		{name: "incomplete history refuses older matching lane", state: "Todo", revision: "7", pages: incomplete, calls: 2, wantError: true, wantReads: 2 * nativeLaneEntryHistoryPages},
		{name: "new revision replaces cached lane entry", state: "Todo", revision: "7", pages: [][]tracker.CollaborationEvent{{transition("first", "Todo", blocked)}}, secondPages: [][]tracker.CollaborationEvent{{transition("second", "Todo", todo)}}, calls: 3, advanceRevision: true, wantFound: true, wantEntered: blocked, wantSecondEntered: todo, wantReads: 2},
		{name: "incomplete new revision leaves prior cache intact", state: "Todo", revision: "7", pages: [][]tracker.CollaborationEvent{{transition("first", "Todo", blocked)}}, secondPages: incomplete, calls: 2, advanceRevision: true, wantFound: true, wantEntered: blocked, wantSecondError: true, wantReads: 1 + nativeLaneEntryHistoryPages},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			reads, pass := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/work-items/wi_lane/history") {
					t.Errorf("unexpected read: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.URL.Query().Get("direction") != "newest" || r.URL.Query().Get("view") != "timeline" || r.URL.Query().Get("limit") != "200" {
					t.Errorf("history read did not use the bounded newest timeline: %s", r.URL.RawQuery)
				}
				mu.Lock()
				reads++
				pages := tt.pages
				if pass > 0 && tt.secondPages != nil {
					pages = tt.secondPages
				}
				mu.Unlock()
				index := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					parsed, err := strconv.Atoi(cursor)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					index = parsed
				}
				page := tracker.Page[tracker.CollaborationEvent]{Items: pages[index]}
				if index+1 < len(pages) {
					page.NextCursor = strconv.Itoa(index + 1)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			scoped, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			c, err := NewNativeConnector(scoped)
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "wi_lane", State: tt.state, Metadata: map[string]string{}}
			if tt.revision != "" {
				issue.Metadata["hub_revision"] = tt.revision
			}
			for call := range tt.calls {
				mu.Lock()
				pass = call
				mu.Unlock()
				if call > 0 && tt.advanceRevision {
					issue.Metadata["hub_revision"] = "8"
				}
				wantError, wantEntered := tt.wantError, tt.wantEntered
				if call > 0 && tt.secondPages != nil {
					wantError = tt.wantSecondError
					wantEntered = tt.wantSecondEntered
				}
				entry, found, err := c.CurrentLaneEntry(context.Background(), issue)
				if (err != nil) != wantError {
					t.Fatalf("history error=%v, wantError=%t", err, wantError)
				}
				if wantError {
					if found || !entry.EnteredAt.IsZero() {
						t.Fatalf("incomplete history established an entry: %+v found=%t", entry, found)
					}
					if tt.advanceRevision {
						issue.Metadata["hub_revision"] = "7"
						prior, priorFound, priorErr := c.CurrentLaneEntry(context.Background(), issue)
						if priorErr != nil || !priorFound || !prior.EnteredAt.Equal(tt.wantEntered) {
							t.Fatalf("incomplete read replaced the earlier cache: %+v, %t, %v", prior, priorFound, priorErr)
						}
					}
					continue
				}
				if found != tt.wantFound {
					t.Fatalf("found = %v, want %v", found, tt.wantFound)
				}
				if !entry.EnteredAt.Equal(wantEntered) {
					t.Fatalf("entered = %s, want %s", entry.EnteredAt, wantEntered)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if reads != tt.wantReads {
				t.Fatalf("history reads = %d, want %d", reads, tt.wantReads)
			}
		})
	}
}

func TestNativeLaneWriteWithoutLeaseIsUnauthorized(t *testing.T) {
	t.Parallel()
	const item = tracker.NativeWorkItemID("wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for _, test := range []struct {
		name     string
		lease    tracker.LeaseID
		wantSent bool
	}{
		{name: "lease held", lease: "lease_example", wantSent: true},
		{name: "lease gone"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var sent []tracker.Transition
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: item, Profile: "native", Revision: 3}, State: "Rework"})
					return
				}
				var request tracker.Transition
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode transition: %v", err)
				}
				mu.Lock()
				sent = append(sent, request)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: item, Profile: "native", Revision: 4}, State: "Todo"})
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			if test.lease != "" {
				client.nativeLeases.Store(native.base()+"/"+string(item), tracker.NativeLease{ID: test.lease, WorkItemID: item, FencingToken: 7})
			}
			connector, err := NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			err = connector.UpdateIssueState(t.Context(), string(item), "Todo")
			mu.Lock()
			defer mu.Unlock()
			if test.wantSent {
				if err != nil || len(sent) != 1 || sent[0].LeaseID != test.lease || sent[0].FencingToken != 7 {
					t.Fatalf("UpdateIssueState() = %v with %+v, want one fenced transition", err, sent)
				}
				return
			}
			if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || len(sent) != 0 {
				t.Fatalf("UpdateIssueState() = %v with %d transitions, want execution authority unavailable before any write", err, len(sent))
			}
		})
	}
}
