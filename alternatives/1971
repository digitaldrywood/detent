package hubserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeIssuePickerProjection(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	issues := seedArchiveIssues(t, f.service, scope, 120, "Todo")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title = 'Picker needle' WHERE native_id = ?", issues[7].WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 1 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", issues[7].WorkItemID); err != nil {
		t.Fatal(err)
	}

	read := func(t *testing.T, query string) (int, nativeIssuePickerPage, []byte) {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+query, f.token, nil)
		var page nativeIssuePickerPage
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return response.Code, page, response.Body.Bytes()
	}

	_, first, _ := read(t, "fields=picker&limit=100")
	tests := []struct {
		name      string
		query     string
		status    int
		items     int
		more      bool
		maxBytes  int
		wantTitle string
		priority  bool
	}{
		{name: "first page is compact", query: "fields=picker&limit=100", status: http.StatusOK, items: 100, more: true, maxBytes: 30_000},
		{name: "keyset continuation", query: "fields=picker&limit=100&cursor=" + url.QueryEscape(first.NextCursor), status: http.StatusOK, items: 20, maxBytes: 30_000},
		{name: "server search", query: "fields=picker&limit=100&q=needle", status: http.StatusOK, items: 1, maxBytes: 30_000, wantTitle: "Picker needle", priority: true},
		{name: "unknown projection", query: "fields=full", status: http.StatusUnprocessableEntity},
		{name: "combined with include", query: "fields=picker&include=summary", status: http.StatusUnprocessableEntity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, page, body := read(t, test.query)
			if status != test.status {
				t.Fatalf("status = %d, want %d: %s", status, test.status, body)
			}
			if test.status != http.StatusOK {
				return
			}
			if len(page.Items) != test.items || (page.NextCursor != "") != test.more || page.Total == 0 {
				t.Fatalf("items = %d next = %q total = %d", len(page.Items), page.NextCursor, page.Total)
			}
			if len(body) > test.maxBytes {
				t.Fatalf("picker page is %d bytes, want at most %d", len(body), test.maxBytes)
			}
			for _, forbidden := range []string{`"body"`, `"labels"`, `"actor"`, `"revision"`} {
				if strings.Contains(string(body), forbidden) {
					t.Fatalf("picker page carries %s: %s", forbidden, body)
				}
			}
			for _, item := range page.Items {
				if item.WorkItemID == "" || item.Number == 0 || item.Title == "" || item.State != "Todo" {
					t.Fatalf("incomplete picker row: %+v", item)
				}
			}
			if test.wantTitle != "" && page.Items[0].Title != test.wantTitle {
				t.Fatalf("title = %q, want %q", page.Items[0].Title, test.wantTitle)
			}
			if test.priority && (page.Items[0].Priority == nil || *page.Items[0].Priority != 1) {
				t.Fatalf("priority = %v, want 1", page.Items[0].Priority)
			}
		})
	}

	_, second, _ := read(t, "fields=picker&limit=100&cursor="+url.QueryEscape(first.NextCursor))
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[string(item.WorkItemID)] {
			t.Fatalf("continuation repeated %s", item.WorkItemID)
		}
		seen[string(item.WorkItemID)] = true
	}
	if len(seen) != len(issues) {
		t.Fatalf("pages covered %d issues, want %d", len(seen), len(issues))
	}
}
