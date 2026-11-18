package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeStateIDsAvoidProjectRefresh(t *testing.T) {
	for _, archived := range []int{0, 10000} {
		t.Run(strconv.Itoa(archived), func(t *testing.T) {
			items := make(map[string]tracker.NativeIssue, archived+2)
			for index := range archived {
				id := fmt.Sprintf("wi_archived%d", index)
				items[id] = tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(id)}, State: "Done"}
			}
			for _, id := range []string{"wi_one", "wi_two"} {
				items[id] = tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(id)}, State: "In Progress"}
			}
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				id := strings.TrimPrefix(r.URL.Path, "/api/v2/organizations/org_test/projects/prj_test/work-items/")
				if id != "wi_one" && id != "wi_two" {
					t.Errorf("unbounded request with %d archived items: %s", archived, r.URL)
					http.Error(w, "project list unavailable", http.StatusGatewayTimeout)
					return
				}
				_ = json.NewEncoder(w).Encode(items[id])
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "reset", Reset: true})
			c, err := NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.FetchIssueStatesByIDs(t.Context(), []string{"wi_one", "wi_two"})
			if err != nil || len(got) != 2 || reads != 2 {
				t.Fatalf("items=%d reads=%d error=%v", len(got), reads, err)
			}
			if got[0].ID != "wi_one" || got[1].ID != "wi_two" {
				t.Fatalf("unexpected IDs: %+v", got)
			}
		})
	}
}
