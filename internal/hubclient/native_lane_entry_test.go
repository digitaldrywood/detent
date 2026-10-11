package hubclient

import (
	"context"
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

func TestNativeConnectorCurrentLaneEntry(t *testing.T) {
	t.Parallel()
	blocked := time.Date(2026, 10, 8, 9, 41, 0, 0, time.UTC)
	todo := time.Date(2026, 10, 8, 18, 7, 12, 0, time.UTC)
	transition := func(id, to string, at time.Time) tracker.CollaborationEvent {
		return tracker.CollaborationEvent{ID: id, Type: "workflow.transitioned", RecordedAt: at, Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, Data: tracker.CollaborationData{ToState: to}}
	}
	comment := tracker.CollaborationEvent{ID: "comment", Type: "comment.created", RecordedAt: todo.Add(time.Minute)}
	tests := []struct {
		name        string
		state       string
		revision    string
		pages       [][]tracker.CollaborationEvent
		calls       int
		wantFound   bool
		wantEntered time.Time
		wantReads   int
	}{
		{
			name:        "latest transition into current lane",
			state:       "Todo",
			revision:    "7",
			pages:       [][]tracker.CollaborationEvent{{transition("blocked", "Blocked", blocked), transition("todo", "Todo", todo), comment}},
			calls:       1,
			wantFound:   true,
			wantEntered: todo,
			wantReads:   1,
		},
		{
			name:        "transition on a later page wins",
			state:       "todo",
			revision:    "7",
			pages:       [][]tracker.CollaborationEvent{{transition("todo-old", "Todo", blocked.Add(-time.Hour))}, {transition("blocked", "Blocked", blocked)}, {transition("todo", "Todo", todo)}},
			calls:       1,
			wantFound:   true,
			wantEntered: todo,
			wantReads:   3,
		},
		{
			name:      "latest transition names another lane",
			state:     "Todo",
			revision:  "7",
			pages:     [][]tracker.CollaborationEvent{{transition("todo", "Todo", blocked), transition("blocked", "Blocked", todo)}},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/work-items/wi_lane/history") {
					t.Errorf("unexpected read: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				mu.Lock()
				reads++
				mu.Unlock()
				index := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					index = int(cursor[0] - '0')
				}
				page := tracker.Page[tracker.CollaborationEvent]{Items: tt.pages[index]}
				if index+1 < len(tt.pages) {
					page.NextCursor = string(rune('0' + index + 1))
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
			for range tt.calls {
				entry, found, err := c.CurrentLaneEntry(context.Background(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if found != tt.wantFound {
					t.Fatalf("found = %v, want %v", found, tt.wantFound)
				}
				if !entry.EnteredAt.Equal(tt.wantEntered) {
					t.Fatalf("entered = %s, want %s", entry.EnteredAt, tt.wantEntered)
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
