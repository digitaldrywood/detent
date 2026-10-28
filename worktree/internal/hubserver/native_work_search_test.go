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
	target := f.create(t, "fix(setup): Associate runner checkout deadlocks on a new project")
	body := `## Context

Observed 2026-10-12 onboarding a new native Cloud project (prj_example, repo example-org/example-project, private) with a freshly enrolled, online runner on detent 0.119.0. Setup step 3, "Associate the runner checkout", returns 422 checkout_unavailable: "Start an enrolled runner with this project's Git checkout and matching GitHub origin, then retry."

This can never succeed for a new project:
- Hub side: bindRunnerCheckoutCommand (internal/hubserver/integration_checkout.go ~line 80) binds only if an active runner reported checkout_repository for this project in the last 5 minutes.
- Runner side: the runner only reports a checkout for projects in its config (runnerProjects.repository, internal/cli/runner_projects.go:188). For a project with no repository, prepareRunnerCheckout (internal/cli/runner_checkout.go:45) fails with "Project has no bound repository" because the clone URL is empty, and runnerProjectsForIdentity skips the project (continue). So it is never added and never reported.
- Manually cloning into <workspace_root>/<project_id> does not help: runnerProjectCheckout (runner_projects.go:347) returns "runner checkout ... does not match Cloud repository" when the Cloud repository is empty and the target has a .git.

So the Hub waits for the runner to report a checkout, and the runner won't set the project up until the Hub has a repository. Setup steps 2 and 3 both stay failed.

## Acceptance criteria

- A new native project with no repository can be bound to a private GitHub repository from the setup page in one action, with no manual file operations on the runner host.
- For example: the runner clones the owner/name entered in setup using its own git credentials and reports the checkout, or it adopts and reports a local checkout whose origin it finds, when the Cloud repository is empty.
- The page shows progress using the selected runner display name and the repository entered by the user, instead of a 422 error. Use a generic fixture such as "Waiting for Example runner to clone example-org/example-repository" in tests and screenshots. Never hard-code a real operator, customer, organization, host or repository name into product copy, design samples or fixtures. Real project names in production must come from the selected project data.

## Must not break

- Cloud never receives repository credentials. The runner clones with its own access.
- Existing repository bindings stay immutable. A checkout whose origin mismatches a bound repository is still refused.
- The GitHub App binding path (bindNativeRepositoryCommand without source) is unchanged.

## How we know it worked

- Table-driven tests covering a new project with an empty repository through to a bound repository and a reported checkout, on both the runner (runnerProjectsForIdentity) and the Hub (bindRunnerCheckoutCommand) sides.
- make check-land passes.
- Post-deploy: create a new native project, enroll a runner, enter owner/name, and confirm binding plus checkout complete with no manual steps.

## Invariants touched

INV-17: the runner prepares its workspace from Hub-provided project data and its install configuration; no machine-local state is hand-placed. No mechanism added (INV-3 not applicable).

## Safety regression

No safety-critical path is touched; onboarding repository binding and runner checkout preparation only.`
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ?, labels_json = ?, native_created_at = ?, native_updated_at = ? WHERE native_id = ?", body, `["search-fixture","unique-xyz"]`, "2026-10-12T00:00:00Z", "2026-10-13T00:00:00Z", target.WorkItemID); err != nil {
		t.Fatal(err)
	}
	syncNativeIssueFixtureFacts(t, f.service.database.db, target.WorkItemID)
	for index := range 8 {
		f.create(t, fmt.Sprintf("Organization names background %d", index))
		f.create(t, fmt.Sprintf("Names catalog %d", index))
		partial := f.create(t, fmt.Sprintf("Partial term match %d", index))
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ? WHERE native_id = ?", strings.Repeat("Real organizations in fixtures. ", 12), partial.WorkItemID); err != nil {
			t.Fatal(err)
		}
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
	numberFixture := newNativeFixture(t, nil, "", "number-search")
	numberForeign := newNativeFixture(t, numberFixture.service, numberFixture.project.OrganizationID, "number-other")
	numberHidden := numberForeign.create(t, "Separate project number lookup")
	numbered := numberFixture.create(t, "Number lookup without matching content")
	mentioned := numberFixture.create(t, "Discussion number mentions")
	requireNativeStatus(t, performHubAPIRequest(t, numberFixture.service, http.MethodPost, numberFixture.base+"/work-items/"+string(mentioned.WorkItemID)+"/comments", numberFixture.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "number-mentions"}, Body: "References #1 and #10003"}), http.StatusOK)
	archived := numberFixture.create(t, "Archived number lookup")
	requireNativeStatus(t, performHubAPIRequest(t, numberFixture.service, http.MethodPost, numberFixture.base+"/work-items/"+string(archived.WorkItemID)+"/archive", numberFixture.token, archiveRequest(archived, "archive-number-search")), http.StatusOK)
	if _, err := numberFixture.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", strings.Repeat("Unrelated preview. ", 2000)+"archivebodytoken", archived.WorkItemID); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, numberFixture.service, http.MethodPost, numberFixture.base+"/work-items/"+string(archived.WorkItemID)+"/comments", numberFixture.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "archived-search-comment"}, Body: strings.Repeat("Unrelated comment. ", 2000) + "archivecommenttoken"}), http.StatusOK)
	drainTestNativeBoard(t, numberFixture.service)
	numberContext := changeOperatorContext(t, numberFixture.service, numberFixture.token, string(numberFixture.project.OrganizationID))
	numberProtocols := map[string]func(string, string, any) hostedContextReply{}
	for _, transport := range []string{"stdio", "http"} {
		numberProtocols[transport] = hostedContextProtocol(t, numberFixture.service, numberContext, transport)
	}
	for _, test := range []struct {
		name, query string
		fixture     nativeFixture
		filters     url.Values
		want        []tracker.NativeWorkItemID
	}{
		{"bare number", "1", numberFixture, nil, []tracker.NativeWorkItemID{numbered.WorkItemID, mentioned.WorkItemID}},
		{"hash number", "#1", numberFixture, nil, []tracker.NativeWorkItemID{numbered.WorkItemID, mentioned.WorkItemID}},
		{"project number", numberFixture.project.Name + "#1", numberFixture, nil, []tracker.NativeWorkItemID{numbered.WorkItemID, mentioned.WorkItemID}},
		{"project ID number", string(numberFixture.project.ID) + "#1", numberFixture, nil, []tracker.NativeWorkItemID{numbered.WorkItemID, mentioned.WorkItemID}},
		{"text number only", "10003", numberFixture, nil, []tracker.NativeWorkItemID{mentioned.WorkItemID}},
		{"nonexistent number", "999999", numberFixture, nil, nil},
		{"other project number", "1", numberForeign, nil, []tracker.NativeWorkItemID{numberHidden.WorkItemID}},
		{"archived excluded", "#3", numberFixture, nil, nil},
		{"archived included", "#3", numberFixture, url.Values{"archived": {"true"}}, []tracker.NativeWorkItemID{archived.WorkItemID}},
		{"archived title", "Archived", numberFixture, url.Values{"archived": {"all"}}, []tracker.NativeWorkItemID{archived.WorkItemID}},
		{"archived body", "archivebodytoken", numberFixture, url.Values{"archived": {"all"}}, []tracker.NativeWorkItemID{archived.WorkItemID}},
		{"archived comment", "archivecommenttoken", numberFixture, url.Values{"archived": {"all"}}, []tracker.NativeWorkItemID{archived.WorkItemID}},
		{"lane excluded", "1", numberFixture, url.Values{"state": {"Backlog"}}, nil},
		{"lane included", "1", numberFixture, url.Values{"state": {"Todo"}}, []tracker.NativeWorkItemID{numbered.WorkItemID, mentioned.WorkItemID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, include := range []string{"summary", "work", "board"} {
				t.Run(include, func(t *testing.T) {
					params := url.Values{"q": {test.query}, "include": {include}, "limit": {"1"}}
					for name, values := range test.filters {
						params[name] = values
					}
					var got []tracker.NativeWorkItemID
					for {
						response := performHubAPIRequest(t, numberFixture.service, http.MethodGet, test.fixture.base+"/work-items?"+params.Encode(), test.fixture.token, nil)
						requireNativeStatus(t, response, http.StatusOK)
						var page tracker.NativeIssuePage
						decodeHubResponse(t, response, &page)
						for _, item := range page.Items {
							got = append(got, item.WorkItemID)
							if item.Snippet == "" {
								t.Fatal("number search lost snippet")
							}
						}
						if include == "work" && test.filters.Get("archived") != "true" {
							var workIDs []tracker.NativeWorkItemID
							for _, item := range page.Work.Items {
								workIDs = append(workIDs, item.WorkItemID)
							}
							if !slices.Equal(workIDs, test.want[:min(1, len(test.want))]) {
								t.Fatalf("work number search = %v, want first of %v", workIDs, test.want)
							}
						}
						if page.NextCursor == "" {
							break
						}
						params.Set("cursor", page.NextCursor)
					}
					if !slices.Equal(got, test.want) {
						t.Fatalf("number search = %v, want %v", got, test.want)
					}
				})
			}
			if test.fixture.project.ID != numberFixture.project.ID {
				return
			}
			for _, transport := range []string{"stdio", "http"} {
				t.Run(transport, func(t *testing.T) {
					args := map[string]any{"project_id": numberFixture.project.ID, "query": test.query}
					for name := range test.filters {
						args[name] = test.filters.Get(name)
					}
					raw := hostedContextData(t, numberProtocols[transport]("tools/call", operatortool.WorkList, args), false)
					var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					var got []tracker.NativeWorkItemID
					for _, item := range result.Data.Items {
						got = append(got, item.WorkItemID)
					}
					if !slices.Equal(got, test.want) {
						t.Fatalf("MCP number search = %v, want %v", got, test.want)
					}
				})
			}
		})
	}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	protocols := map[string]func(string, string, any) hostedContextReply{}
	for _, transport := range []string{"stdio", "http"} {
		protocols[transport] = hostedContextProtocol(t, f.service, ctx, transport)
	}
	for _, test := range []struct {
		name, query, matched string
		want                 tracker.NativeWorkItemID
		label                string
	}{
		{"issue 958 ranked example", "real organization names in fixtures", "fixture", target.WorkItemID, ""},
		{"stem", "fixture", "fixture", target.WorkItemID, "search-fixture"},
		{"prefix", "prod", "prod", target.WorkItemID, ""},
		{"OR", "unicorn nonexistentterm", "Unicorns", commentItem.WorkItemID, ""},
		{"substring recall", "ganiz", "organization", target.WorkItemID, "search-fixture"},
		{"label recall", "nique-xy", "unique-xyz", target.WorkItemID, ""},
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
			for _, transport := range []string{"stdio", "http"} {
				t.Run(transport, func(t *testing.T) {
					args := map[string]any{"project_id": f.project.ID, "query": test.query, "limit": 5}
					if test.label != "" {
						args["label"] = test.label
					}
					raw := hostedContextData(t, protocols[transport]("tools/call", operatortool.WorkList, args), false)
					var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					index := slices.IndexFunc(result.Data.Items, func(item operatortool.NativeItem) bool { return item.WorkItemID == test.want })
					if index < 0 {
						t.Fatal("MCP target absent from top five")
					}
					if !strings.Contains(strings.ToLower(result.Data.Items[index].Snippet), strings.ToLower(test.matched)) {
						t.Fatal("MCP snippet omits matching text")
					}
				})
			}
		})
	}
	for _, transport := range []string{"stdio", "http"} {
		protocol := protocols[transport]
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
	migration, err := migrationFiles.ReadFile("migrations/20261021171000_native_work_search_content.sql")
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
