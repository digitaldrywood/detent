package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeCleanupLookup(t *testing.T) {
	for _, mode := range []string{"mixed", "invalid first", "restart", "other 422", "churn", "cancel"} {
		for _, lookup := range []string{"ids", "states", "identifiers"} {
			if (mode == "mixed" || mode == "invalid first") != (lookup == "ids") {
				continue
			}
			t.Run(mode+"/"+lookup, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				defer slog.SetDefault(previous)
				calls := 0
				var cursors []string
				item := func(id string, n int) tracker.NativeIssue {
					return tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(id), ProjectID: "prj_example", Number: n}, State: "Done", Terminal: true}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(r.URL.Path, "/changes") {
						json.NewEncoder(w).Encode([]tracker.ChangeRequest{})
						return
					}
					if lookup == "ids" {
						id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
						v := item(id, 7)
						if id == "wi_active" {
							v.State, v.Terminal = "Todo", false
						}
						json.NewEncoder(w).Encode(v)
						return
					}
					calls++
					q := r.URL.Query()
					cursors = append(cursors, q.Get("cursor"))
					if lookup == "states" && q.Get("state") != "Done" || lookup == "identifiers" && q.Get("q") != "prj_example#7" {
						t.Error("restart changed filters", q)
					}
					if calls == 2 || mode == "churn" && calls == 4 {
						message := "Work list order changed; restart the query"
						if mode == "other 422" {
							message = "Invalid search filter"
						}
						if mode == "cancel" {
							cancel()
						}
						w.WriteHeader(422)
						json.NewEncoder(w).Encode(map[string]string{"code": "invalid_request", "message": message})
						return
					}
					page := tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{item("wi_terminal", 7)}, NextCursor: "fresh"}
					if calls == 1 {
						page.Items = []tracker.NativeIssue{item("wi_stale", 7)}
						page.NextCursor = "stale"
					}
					if calls == 4 {
						page.Items = append(page.Items, item("wi_other", 8))
						page.NextCursor = ""
					}
					json.NewEncoder(w).Encode(page)
				}))
				defer server.Close()
				client, err := New(Config{URL: server.URL, HTTPClient: server.Client(), TokenSource: func() string { return "example" }})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				c, err := NewNativeConnector(native)
				if err != nil {
					t.Fatal(err)
				}
				var got []connector.Issue
				switch lookup {
				case "ids":
					ids := []string{"wi_terminal", "I_kwDO_example", "wi_active"}
					if mode == "invalid first" {
						ids = append(slices.Repeat([]string{"I_kwDO_example"}, 101), ids...)
					}
					got, err = c.FetchIssueStatesByIDs(ctx, ids)
					if err != nil || len(got) != 2 || got[0].ID != "wi_terminal" || got[1].State != "Todo" || !strings.Contains(logs.String(), "I_kwDO_example") {
						t.Fatalf("mixed lookup=%+v error=%v logs=%s", got, err, logs.String())
					}
					return
				case "states":
					got, err = c.FetchIssuesByStates(ctx, []string{"Done"})
				case "identifiers":
					got, err = c.FetchIssueStatesByIdentifiers(ctx, []string{"prj_example#7"})
				}
				if mode != "restart" {
					if err == nil || len(got) != 0 || calls > 4 {
						t.Fatalf("incomplete lookup authorized records: %+v %v calls=%d", got, err, calls)
					}
					return
				}
				want := 1
				if lookup == "states" {
					want = 2
				}
				if err != nil || len(got) != want || got[0].ID != "wi_terminal" || !slices.Equal(cursors, []string{"", "stale", "", "fresh"}) {
					t.Fatalf("restart=%+v err=%v cursors=%v", got, err, cursors)
				}
			})
		}
	}
}
