package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkSearch(t *testing.T) {
	f := newNativeFixture(t, nil, "", "ranked-search")
	target := f.create(t, "Associate runner checkout on a new project")
	body := "Never hard-code a real operator, customer, organization, host or repository name into product copy, design samples or fixtures. Real project names in production must come from the selected project data."
	targetComment := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(target.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "generic-example-guidance"}, Body: "Use generic names such as example-org/example-project or owner/repository in fixtures, designs, instructional copy and issue examples. Product values come from the selected runtime context. Keep real names only as factual historical evidence or actual project screenshots; never reuse them as synthetic mock data."})
	requireNativeStatus(t, targetComment, http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ?, labels_json = ?, native_created_at = ?, native_updated_at = ? WHERE native_id = ?", body, `["search-fixture","unique-tag"]`, "2026-10-12T00:00:00Z", "2026-10-13T00:00:00Z", target.WorkItemID); err != nil {
		t.Fatal(err)
	}
	for index := range 8 {
		f.create(t, fmt.Sprintf("Organization background %d", index))
	}
	commentItem := f.create(t, "Discussion-only match")
	path := f.base + "/work-items/" + string(commentItem.WorkItemID) + "/comments"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "search-comment"}, Body: "Unicorns decorate mock fixture data."})
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-search")
	hidden := foreign.create(t, "Unicorns decorate mock fixture data")
	list := func(params url.Values) tracker.NativeIssuePage {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		return page
	}
	for _, test := range []struct {
		name, query, matched string
		want                 tracker.NativeWorkItemID
		label                string
	}{
		{"ranked example", "real organization names in fixtures", "fixture", target.WorkItemID, ""},
		{"stem", "fixture", "fixture", target.WorkItemID, "search-fixture"},
		{"prefix", "prod", "prod", target.WorkItemID, ""},
		{"OR", "unicorn nonexistentterm", "Unicorns", commentItem.WorkItemID, ""},
		{"substring recall", "ganiz", "organization", target.WorkItemID, "search-fixture"},
		{"label recall", "nique-ta", "unique-tag", target.WorkItemID, ""},
		{"identifier recall", string(f.project.ID) + "#" + strconv.Itoa(target.Number), strconv.Itoa(target.Number), target.WorkItemID, "search-fixture"},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := url.Values{"q": {test.query}, "limit": {"5"}, "include": {"summary"}}
			if test.label != "" {
				params.Set("label", test.label)
			}
			page := list(params)
			index := slices.IndexFunc(page.Items, func(item tracker.NativeIssue) bool { return item.WorkItemID == test.want })
			if index < 0 {
				t.Fatalf("target absent from top five: %#v", page.Items)
			}
			if !strings.Contains(strings.ToLower(page.Items[index].Snippet), strings.ToLower(test.matched)) {
				t.Fatalf("snippet %q omits match %q", page.Items[index].Snippet, test.matched)
			}
			for _, item := range page.Items {
				if item.WorkItemID == hidden.WorkItemID {
					t.Fatal("foreign project matched")
				}
				if item.Snippet == "" {
					t.Fatal("missing snippet")
				}
			}
		})
	}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	for _, transport := range []string{"stdio", "http"} {
		protocol := hostedContextProtocol(t, f.service, ctx, transport)
		raw := hostedContextData(t, protocol("tools/call", operatortool.WorkList, map[string]any{"project_id": f.project.ID, "query": "real organization names in fixtures", "created_after": "2026-10-12T00:00:00Z", "created_before": "2026-10-13T00:00:00Z", "updated_after": "2026-10-13T00:00:00Z", "label": "search-fixture", "state": "Todo", "limit": 5}), false)
		var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Data.Items) != 1 || result.Data.Items[0].WorkItemID != target.WorkItemID || result.Data.Items[0].Snippet == "" {
			t.Fatalf("MCP search lost filters/snippet: %s", raw)
		}
	}
	workPage := list(url.Values{"q": {"real organization names in fixtures"}, "include": {"work"}, "limit": {"5"}})
	if workPage.Work == nil || slices.IndexFunc(workPage.Work.Items, func(item tracker.NativeIssue) bool { return item.WorkItemID == target.WorkItemID }) < 0 {
		t.Fatal("work projection lost ranked target")
	}
	for _, item := range workPage.Work.Items {
		if item.Snippet == "" {
			t.Fatal("work projection lost matched snippet")
		}
	}
	params := url.Values{"q": {"organization"}, "limit": {"1"}}
	original := list(url.Values{"q": {"organization"}, "limit": {"100"}})
	var seen []tracker.NativeWorkItemID
	page := list(params)
	for {
		for _, item := range page.Items {
			seen = append(seen, item.WorkItemID)
		}
		if page.NextCursor == "" {
			break
		}
		if len(seen) == 1 {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title = 'Changed unrelated title', body = '' WHERE native_id = ?", target.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}
		params.Set("cursor", page.NextCursor)
		page = list(params)
	}
	want := make([]tracker.NativeWorkItemID, len(original.Items))
	for index, item := range original.Items {
		want[index] = item.WorkItemID
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("cursor reordered search: %v want %v", seen, want)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-search-comment"}, ExpectedRevision: comment.Revision, Body: "Narwhals decorate mock data."}), http.StatusOK)
	if page := list(url.Values{"q": {"unicorn"}}); len(page.Items) != 0 {
		t.Fatal("edited comment retained old match")
	}
	if page := list(url.Values{"q": {"narwhal"}}); len(page.Items) != 1 || page.Items[0].WorkItemID != commentItem.WorkItemID {
		t.Fatal("edited comment missing new match")
	}
	for _, query := range []string{"\" OR NOT * : ()", "!!!"} {
		list(url.Values{"q": {query}})
	}
}

func TestNativeWorkDateFilters(t *testing.T) {
	f := newNativeFixture(t, nil, "", "search-dates")
	stamps := []string{"2026-10-11T23:59:59.999999999Z", "2026-10-12T00:00:00Z", "2026-10-12T06:00:00+05:00", "2026-10-13T00:00:00Z", "2026-10-12T00:00:00.000000001Z", "2026-10-12T00:00:00.001Z", "2026-10-12T00:00:00.0015Z"}
	ids := make([]tracker.NativeWorkItemID, len(stamps))
	for index, stamp := range stamps {
		date, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		stamp = formatHubTime(date)
		item := f.create(t, fmt.Sprintf("Date fixture %d", index))
		ids[index] = item.WorkItemID
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_created_at = ?, native_updated_at = ? WHERE native_id = ?", stamp, stamp, item.WorkItemID); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name   string
		params url.Values
		want   []int
		status int
	}{
		{"inclusive created", url.Values{"created_after": {"2026-10-12T00:00:00Z"}}, []int{1, 2, 3, 4, 5, 6}, 200},
		{"exclusive before", url.Values{"created_before": {"2026-10-12T00:00:00Z"}}, []int{0}, 200},
		{"day window", url.Values{"created_after": {"2026-10-12T00:00:00Z"}, "created_before": {"2026-10-13T00:00:00Z"}}, []int{1, 2, 4, 5, 6}, 200},
		{"updated offset", url.Values{"updated_after": {"2026-10-12T06:00:00+05:00"}}, []int{2, 3}, 200},
		{"before nanosecond", url.Values{"created_before": {"2026-10-12T00:00:00.000000001Z"}}, []int{0, 1}, 200},
		{"fractional precision", url.Values{"updated_after": {"2026-10-12T00:00:00.0015Z"}}, []int{2, 3, 6}, 200},
		{"board rejects date selectors", url.Values{"include": {"board"}, "created_after": {"2026-10-12T00:00:00Z"}}, nil, 422},
		{"invalid date", url.Values{"created_after": {"private invalid date"}}, nil, 422},
		{"invalid before", url.Values{"created_before": {"today"}}, nil, 422},
		{"invalid updated", url.Values{"updated_after": {"2026-10-12"}}, nil, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+test.params.Encode(), f.token, nil)
			requireNativeStatus(t, response, test.status)
			if test.status != 200 {
				if strings.Contains(response.Body.String(), test.params.Get("created_after")) && test.params.Get("created_after") != "" {
					t.Fatal("invalid date echoed content")
				}
				return
			}
			var page tracker.NativeIssuePage
			decodeHubResponse(t, response, &page)
			got := make([]tracker.NativeWorkItemID, len(page.Items))
			for index, item := range page.Items {
				got[index] = item.WorkItemID
			}
			want := make([]tracker.NativeWorkItemID, len(test.want))
			for index, id := range test.want {
				want[index] = ids[id]
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("items %v want %v", got, want)
			}
		})
	}
}

func TestNativeWorkSearchIndexMaintenance(t *testing.T) {
	f := newNativeFixture(t, nil, "", "index-maintenance")
	issue := f.create(t, "Existing migration document")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "existing-comment"}, Body: "Existing zebras discussion"})
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	migration, err := migrationFiles.ReadFile("migrations/20261012201500_native_work_search.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(migration), "-- +goose Down")
	if !ok {
		t.Fatal("missing rollback migration")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	matches := func(term string) int {
		t.Helper()
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_fts WHERE native_work_fts MATCH ?", term).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	for _, term := range []string{"migration", "zebra"} {
		if matches(term) != 1 {
			t.Fatalf("backfill missed %s", term)
		}
	}
	for _, test := range []struct {
		name, statement, old, replacement string
		id                                any
	}{
		{"issue rollback", "UPDATE issues SET title = 'Rollback narwhals', body = '' WHERE native_id = ?", "migration", "narwhal", issue.WorkItemID},
		{"comment rollback", "UPDATE native_comments SET body = 'Rollback narwhals' WHERE id = ?", "zebra", "narwhal", comment.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), test.statement, test.id); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_fts WHERE native_work_fts MATCH ?", test.replacement).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatal("index did not update in write transaction")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if matches(test.old) != 1 || matches(test.replacement) != 0 {
				t.Fatal("rollback did not restore search index")
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_comments WHERE id = ?", comment.ID); err != nil {
		t.Fatal(err)
	}
	if matches("zebra") != 0 {
		t.Fatal("deleted comment retained a match")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issues
(native_id, organization_id, project_id, number, title, body, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at)
SELECT 'wi_search_deletable', organization_id, project_id, 1000, 'Deletable wombats', '', '', 'open', '1', source_updated_at, synchronized_at, created_at, updated_at
FROM issues WHERE native_id = ?`, issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if matches("wombat") != 1 {
		t.Fatal("inserted issue missing match")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM issues WHERE native_id = 'wi_search_deletable'"); err != nil {
		t.Fatal(err)
	}
	if matches("wombat") != 0 {
		t.Fatal("deleted issue retained a match")
	}

	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_work_fts(native_work_fts, rank) VALUES ('integrity-check', 1)"); err != nil {
		t.Fatal(err)
	}
}
