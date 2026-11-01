package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func snapshotPage(cursor string, total int, next bool) map[string]any {
	node := func(id, body string) map[string]any {
		return map[string]any{"id": id, "body": body, "createdAt": "2026-09-01T10:00:00Z", "updatedAt": "2026-09-01T10:00:00Z", "author": map[string]string{"login": "author"}}
	}
	issue := node("I_source", "Complete task body")
	issue["url"], issue["title"] = "https://github.com/acme/orders/issues/12", "Source task"
	issue["comments"] = map[string]any{"totalCount": total, "nodes": []any{node("C_"+cursor, "Discussion "+cursor)}, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "next"}}
	return map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "acme/orders", "issue": issue}}}
}

func TestFetchIssueSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name, token                 string
		status                      int
		partial, malformed, changed bool
		want                        error
	}{
		{name: "paginated discussion", token: "private-read-token"},
		{name: "missing credential", want: ErrMissingToken},
		{name: "private issue authorization", token: "private-read-token", status: 401, want: ErrAuthenticationFailed},
		{name: "private issue forbidden", token: "private-read-token", status: 403, want: ErrAuthenticationFailed},
		{name: "inaccessible issue", token: "private-read-token", status: 404, want: ErrNotFound},
		{name: "rate limit", token: "private-read-token", status: 429, want: ErrRateLimited},
		{name: "second page failure", token: "private-read-token", partial: true, want: ErrTransient},
		{name: "incomplete page", token: "private-read-token", malformed: true, want: ErrInvalidResponse},
		{name: "issue edited during pagination", token: "private-read-token", changed: true, want: ErrInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer private-read-token" {
					t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				}
				var request struct {
					Variables struct {
						Cursor string `json:"cursor"`
					} `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if test.status != 0 || test.partial && request.Variables.Cursor != "" {
					status := test.status
					if status == 0 {
						status = 503
					}
					w.WriteHeader(status)
					fmt.Fprint(w, `{"message":"unavailable"}`)
					return
				}
				page := snapshotPage(request.Variables.Cursor, 2, request.Variables.Cursor == "" && !test.malformed)
				if test.changed && request.Variables.Cursor != "" {
					page["data"].(map[string]any)["repository"].(map[string]any)["issue"].(map[string]any)["body"] = "edited"
				}
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(ClientConfig{Endpoint: server.URL + "/graphql", TokenSource: StaticTokenSource(test.token), HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := client.FetchIssueSnapshot(t.Context(), "https://github.com/acme/orders/issues/12")
			if test.want != nil {
				if !errors.Is(err, test.want) || snapshot.Title != "" || len(snapshot.Comments) != 0 {
					t.Fatalf("snapshot = %#v, error = %v, want %v and no excerpt", snapshot, err, test.want)
				}
				if test.token == "" && calls.Load() != 0 {
					t.Fatal("missing credentials made a GitHub call")
				}
				return
			}
			if err != nil || calls.Load() != 2 || len(snapshot.Comments) != 2 || snapshot.Body != "Complete task body" || snapshot.Comments[1].Body != "Discussion next" || snapshot.Provenance.ObservedAt.IsZero() {
				t.Fatalf("snapshot = %#v, calls = %d, error = %v", snapshot, calls.Load(), err)
			}
		})
	}
}

func TestFetchIssueSnapshotBoundsCompleteDiscussion(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		page := snapshotPage(strconv.FormatInt(call, 10), 21, true)
		issue := page["data"].(map[string]any)["repository"].(map[string]any)["issue"].(map[string]any)
		issue["comments"].(map[string]any)["pageInfo"].(map[string]any)["endCursor"] = fmt.Sprint("page-", call)
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{Endpoint: server.URL + "/graphql", TokenSource: StaticTokenSource("read-token"), HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.FetchIssueSnapshot(t.Context(), "https://github.com/acme/orders/issues/12")
	if !errors.Is(err, ErrInvalidResponse) || snapshot.Title != "" || len(snapshot.Comments) != 0 || calls.Load() != 20 {
		t.Fatalf("partial snapshot=%#v error=%v calls=%d", snapshot, err, calls.Load())
	}
}
