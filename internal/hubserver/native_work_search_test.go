package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"

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
	longTerms := make([]string, 502)
	for index := range longTerms {
		longTerms[index] = "w" + strconv.Itoa(index)
	}
	longTerms[len(longTerms)-1] = "unicorn"
	t.Run("long HTTP query", func(t *testing.T) {
		page := list(url.Values{"q": {strings.Join(longTerms, " ")}})
		if len(page.Items) != 1 || page.Items[0].WorkItemID != commentItem.WorkItemID {
			t.Fatalf("long query lost comment match: %+v", page.Items)
		}
	})
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

func TestNativeWorkSearchRanking(t *testing.T) {
	f := newNativeFixture(t, nil, "", "search-ranking")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	body := f.create(t, "Separate symptom")
	title := f.create(t, "Repository association deadlocks")
	partial := f.create(t, "Repository association")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='' WHERE project_id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='Repository association deadlocks' WHERE native_id=?", body.WorkItemID); err != nil {
		t.Fatal(err)
	}
	want := []tracker.NativeWorkItemID{title.WorkItemID, body.WorkItemID, partial.WorkItemID}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, mode := range []string{"issues", "duplicates", "rebuilding issues"} {
		t.Run(mode, func(t *testing.T) {
			var got []tracker.NativeWorkItemID
			if mode == "rebuilding issues" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM native_work_scoped_fts; DELETE FROM native_work_search_lengths;
UPDATE native_work_search_rebuild SET through_id=0,last_id=(SELECT max(id) FROM native_work_documents)`); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "duplicates" {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+url.Values{"q": {"repos associ deadlock"}}.Encode(), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				decodeHubResponse(t, response, &page)
				for _, item := range page.Items {
					got = append(got, item.WorkItemID)
				}
			} else {
				matches, err := resolveNativeDuplicates(t.Context(), f.service.database.reader, scope, tracker.CreateIssue{Title: "Repository association deadlocks"}, f.service.config.now(), false)
				if err != nil {
					t.Fatal(err)
				}
				for _, match := range matches {
					got = append(got, match.WorkItemID)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("coverage then title-weighted BM25 order=%v want=%v", got, want)
			}
		})
	}
	t.Run("fallback BM25 matches SQLite", func(t *testing.T) {
		for _, name := range []string{"Café relational runners", "Unrelated sentence", "Another subject", "Separate topic", "Different subject", "Empty document"} {
			item := f.create(t, name)
			content := ""
			if name == "Café relational runners" {
				content = "Relational runners prefix\x00café " + strings.Repeat("unrelated ", 20)
			}
			if name == "Empty document" {
				name = ""
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title=?,body=? WHERE native_id=?", name, content, item.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM native_work_scoped_fts; DELETE FROM native_work_search_lengths;
UPDATE native_work_search_rebuild SET through_id=0,last_id=(SELECT max(id) FROM native_work_documents)`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE VIRTUAL TABLE fallback_rank_probe USING fts5(title,body,tokenize='porter unicode61');
INSERT INTO fallback_rank_probe(rowid,title,body)
SELECT c.id,c.title,c.body FROM native_work_search_content c JOIN native_work_documents d ON d.id=c.id
JOIN issues i ON i.id=d.issue_id WHERE i.organization_id=? AND i.project_id=?`, scope.organization, scope.project); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"repository association", "repos associ", "repo repository", "cafe relation run", "prefix", "no matching terms", ""} {
			t.Run(query, func(t *testing.T) {
				expressions := nativeSearchExpressions(query)
				for _, plain := range []bool{false, true} {
					if plain {
						for i := range expressions {
							expressions[i] = strings.TrimSuffix(expressions[i], "*")
						}
					}
					readScores := func(statement string, args ...any) map[int64]float64 {
						t.Helper()
						rows, err := f.service.database.reader.QueryContext(t.Context(), statement, args...)
						if err != nil {
							t.Fatal(err)
						}
						defer rows.Close()
						scores := map[int64]float64{}
						for rows.Next() {
							var id int64
							var score float64
							if err := rows.Scan(&id, &score); err != nil {
								t.Fatal(err)
							}
							scores[id] = score
						}
						if err := rows.Err(); err != nil {
							t.Fatal(err)
						}
						return scores
					}
					want := readScores("SELECT rowid,bm25(fallback_rank_probe,5.0,1.0) FROM fallback_rank_probe WHERE fallback_rank_probe MATCH ?", strings.Join(expressions, " OR "))
					statement, args := nativeSearchHits(scope, "", expressions)
					got := readScores(statement+"SELECT id/2,score FROM ranked WHERE hit=1", args...)
					if len(got) != len(want) {
						t.Fatalf("plain=%t fallback scores=%v SQLite scores=%v", plain, got, want)
					}
					for id, score := range want {
						actual, ok := got[id]
						if !ok || math.Abs(actual-score) > 1e-12 {
							t.Errorf("plain=%t document=%d fallback BM25=%g SQLite BM25=%g", plain, id, actual, score)
						}
					}
				}
			})
		}
	})
}

func nativeSearchProbe(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return pool
}

func TestNativeWorkSearchUnicode61Tokens(t *testing.T) {
	pool := nativeSearchProbe(t)
	if _, err := pool.ExecContext(t.Context(), `CREATE VIRTUAL TABLE search_token_probe USING fts5(original,tokenize='porter unicode61');
CREATE VIRTUAL TABLE search_token_vocabulary USING fts5vocab(search_token_probe,instance);
CREATE VIRTUAL TABLE search_scoped_token_probe USING fts5(indexed,tokenize='native_scoped');
CREATE VIRTUAL TABLE search_scoped_token_vocabulary USING fts5vocab(search_scoped_token_probe,instance)`); err != nil {
		t.Fatal(err)
	}
	var boundaryText strings.Builder
	for _, boundary := range nativeSearchTokenBoundaries {
		for delta := -1; delta <= 1; delta++ {
			r := boundary + rune(delta)
			if r >= 0 && r <= 0x10ffff && (r < 0xd800 || r > 0xdfff) {
				boundaryText.WriteString("a" + string(r) + "b ")
			}
		}
	}
	for index, test := range []struct{ name, content string }{
		{"stemming", "running runners relational generously skies dying ties"},
		{"punctuation", "repo/path-name:word@example.org #123"},
		{"underscores and empty input", "a_b __ _scope_term"},
		{"empty", ""},
		{"unicode", "café cafe\u0301 naïve Ελληνικά 中文 ß א ב ع"},
		{"noncharacters", "a\ufffeb a\uffffb a\ufffdb"},
		{"combining separators", "a\u0301b a\u0312b \u0301word \u1885x a\u200db"},
		{"Unicode61 boundaries", boundaryText.String()},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := nativeSearchScopes(nativeScope{organization: "example-organization", project: "example-project"}, "")[0]
			if _, err := pool.ExecContext(t.Context(), "INSERT INTO search_token_probe(rowid,original) VALUES(?,?)", index+1, test.content); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(t.Context(), "INSERT INTO search_scoped_token_probe(rowid,indexed) VALUES(?,?)", index+1, nativeSearchText(test.content, scope)); err != nil {
				t.Fatal(err)
			}
			rows, err := pool.QueryContext(t.Context(), `SELECT term,col,offset FROM search_token_vocabulary WHERE doc=?
UNION ALL SELECT term,col,offset FROM search_scoped_token_vocabulary WHERE doc=? ORDER BY col,offset`, index+1, index+1)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var original, indexed []string
			for rows.Next() {
				var term, column string
				var offset int
				if err := rows.Scan(&term, &column, &offset); err != nil {
					t.Fatal(err)
				}
				if column == "original" {
					original = append(original, term)
				} else {
					unqualified, ok := strings.CutPrefix(term, strings.ToLower(scope)+"_")
					if !ok {
						t.Fatalf("unqualified token at %d = %q", offset, term)
					}
					indexed = append(indexed, unqualified)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(original, indexed) {
				t.Fatalf("Unicode61 tokens changed: original=%v indexed=%v", original, indexed)
			}
		})
	}
}

func TestNativeWorkSearchSnippet(t *testing.T) {
	pool := nativeSearchProbe(t)
	for _, test := range []struct{ name, title, body, query, want string }{
		{"stemming", "Relational runners", "", `"relation"*`, "[Relational] runners"},
		{"diacritics", "Café résumé", "", `"cafe"*`, "[Café] résumé"},
		{"body coverage", "Runner", "Runner checkout deadlocks", `"runner"* OR "checkout"*`, "[Runner] [checkout] deadlocks"},
		{"Unicode offsets", strings.Repeat("é", 80) + " runners", "", `"runner"*`, strings.Repeat("é", 30) + " [runners]"},
		{"embedded NUL", "prefix\x00runners", "", `"runner"*`, "prefix\x00[runners]"},
		{"plain terms", "repository association", "", `"repository" OR "association"`, "[repository] [association]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var snippet string
			if err := pool.QueryRowContext(t.Context(), "SELECT native_search_snippet(?,?,?)", test.title, test.body, test.query).Scan(&snippet); err != nil {
				t.Fatal(err)
			}
			if snippet != test.want {
				t.Fatalf("snippet=%q want=%q", snippet, test.want)
			}
		})
	}
}

func TestNativeWorkSearchScopedMigration(t *testing.T) {
	f := newNativeFixture(t, nil, "", "scoped-migration")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	first := f.create(t, "Original migration document")
	second := f.create(t, "Second migration document")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "migration-comment"}, Body: "Older comment zebras"})
	requireNativeStatus(t, response, http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE seed(n) AS (
SELECT 1 UNION ALL SELECT n+1 FROM seed WHERE n<?)
INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at)
SELECT 'migration-history-'||seed.n,i.organization_id,i.project_id,i.native_id,1,seed.n,'Separate historical discussion','{}',i.native_created_at,i.native_created_at
FROM seed CROSS JOIN issues i WHERE i.native_id=?`, nativeSearchRebuildBatch+5, second.WorkItemID); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/20261028173817_native_work_search_scope.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(migration), "-- +goose Down")
	if !ok {
		t.Fatal("missing rollback section")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_work_scoped_fts(rowid,title,body,scope)
SELECT id,scope||' legacy',scope||' repository',scope FROM native_work_scoped_content`); err != nil {
		t.Fatal(err)
	}
	tokens, err := migrationFiles.ReadFile("migrations/20261028205333_native_work_search_scoped_tokens.sql")
	if err != nil {
		t.Fatal(err)
	}
	tokenUp, tokenDown, ok := strings.Cut(string(tokens), "-- +goose Down")
	if !ok {
		t.Fatal("missing token rollback section")
	}
	for _, statement := range []string{tokenUp, tokenDown, tokenUp} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); !errors.Is(err, operatortool.ErrServiceUnavailable) {
		t.Fatalf("pending migration err=%v", err)
	}
	var indexed int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_scoped_fts").Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("migration rebuilt %d rows err=%v", indexed, err)
	}
	for _, test := range []struct {
		statement string
		args      []any
	}{
		{"UPDATE issues SET title='Current narwhals' WHERE native_id=?", []any{first.WorkItemID}},
		{`INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,body,actor_json,created_at,updated_at,sequence)
SELECT 'concurrent-migration-comment',organization_id,project_id,native_id,1,'Concurrent wombats','{}',native_created_at,native_created_at,1000 FROM issues WHERE native_id=?`, []any{second.WorkItemID}},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), test.statement, test.args...); err != nil {
			t.Fatal(err)
		}
	}
	ctx, timing := withWriterTiming(t.Context())
	f.service.maintainNativeSearch(ctx)
	if got := timing.txs.Load(); got != nativeSearchRebuildBatch {
		t.Fatalf("maintenance wrote %d transactions want %d", got, nativeSearchRebuildBatch)
	}
	if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); !errors.Is(err, operatortool.ErrServiceUnavailable) {
		t.Fatalf("unbounded maintenance completed all history: %v", err)
	}
	for range 15 {
		more, err := f.service.database.backfillNativeSearch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		term string
		want tracker.NativeWorkItemID
	}{
		{"narwhal", first.WorkItemID}, {"zebra", first.WorkItemID}, {"wombat", second.WorkItemID},
	} {
		statement, args, err := nativeIssueSelection(scope, url.Values{"q": {test.term}}, false)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := nativePageIDs(t.Context(), f.service.database.reader, statement, args...)
		if err != nil || !slices.Equal(ids, []string{string(test.want)}) {
			t.Fatalf("term=%q ids=%v err=%v", test.term, ids, err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_work_scoped_fts(native_work_scoped_fts) VALUES('integrity-check')"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeWorkSearchScopeRefresh(t *testing.T) {
	for _, test := range []struct{ name, statement, archived, principal string }{
		{"archive", "UPDATE issues SET archived=1 WHERE native_id=?", "true", "owner"},
		{"reporter", "UPDATE issues SET actor_principal_id='new-owner' WHERE native_id=?", "", "new-owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "scope-refresh")
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			issue := f.create(t, "Issue context")
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_principal_id='owner' WHERE native_id=?", issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "scope-comment"}, Body: "Comment-only narwhals"})
			requireNativeStatus(t, response, http.StatusOK)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{HostedRole: "reporter"}, sourceActor: &tracker.Actor{Kind: "human", PrincipalID: test.principal}}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), test.statement, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if err := requireNativeSearchIndex(t.Context(), tx, scope); !errors.Is(err, operatortool.ErrServiceUnavailable) {
				t.Fatalf("uncommitted refresh err=%v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); err != nil {
				t.Fatalf("rollback left dirty scope: %v", err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), test.statement, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); !errors.Is(err, operatortool.ErrServiceUnavailable) {
				t.Fatalf("dirty scope err=%v", err)
			}
			page, err := f.service.readIssues(t.Context(), scope, url.Values{"q": {"narwhal"}, "archived": {test.archived}})
			if err != nil || len(page.Items) != 1 || page.Items[0].WorkItemID != issue.WorkItemID {
				t.Fatalf("search during scope refresh: items=%v err=%v", page.Items, err)
			}
			previous := scope
			params := url.Values{"q": {"narwhal"}}
			if test.name == "reporter" {
				previous.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "owner"}
			}
			hidden, err := f.service.readIssues(t.Context(), previous, params)
			if err != nil || len(hidden.Items) != 0 {
				t.Fatalf("stale scope leaked during refresh: items=%v err=%v", hidden.Items, err)
			}
			for range 5 {
				more, err := f.service.database.backfillNativeSearch(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
			if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); err != nil {
				t.Fatal(err)
			}
			query, args, err := nativeIssueSelection(scope, url.Values{"q": {"narwhal"}, "archived": {test.archived}}, false)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := nativePageIDs(t.Context(), f.service.database.reader, query, args...)
			if err != nil || !slices.Equal(ids, []string{string(issue.WorkItemID)}) {
				t.Fatalf("refreshed ids=%v err=%v", ids, err)
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
	matches := func(term string) int {
		t.Helper()
		var count int
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_fts WHERE native_work_fts MATCH ?", term).Scan(&count); err != nil {
			t.Fatal(err)
		}
		var scoped int
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(DISTINCT rowid/2) FROM native_work_scoped_fts WHERE native_work_scoped_fts MATCH ?", nativeScopedExpression(nativeSearchScopes(scope, ""), []string{`"` + term + `"`}, " OR ")).Scan(&scoped); err != nil {
			t.Fatal(err)
		}
		if scoped != count {
			t.Fatalf("term=%q original index=%d scoped index=%d", term, count, scoped)
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

func TestNativeWorkSearchProjectRebuild(t *testing.T) {
	f := newNativeFixture(t, nil, "", "rebuilding-search")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	target := f.create(t, "Distinct narwhal")
	indexed := newNativeFixture(t, f.service, f.project.OrganizationID, "indexed-search")
	other := indexed.create(t, "Indexed narwhal")
	empty := newNativeFixture(t, f.service, f.project.OrganizationID, "empty-search")
	drainTestNativeSearch(t, f.service)
	if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM native_work_scoped_fts WHERE rowid IN (SELECT d.id*2 FROM native_work_documents d JOIN issues i ON i.id=d.issue_id WHERE i.native_id=? UNION ALL SELECT d.id*2+1 FROM native_work_documents d JOIN issues i ON i.id=d.issue_id WHERE i.native_id=?);
 UPDATE native_work_search_rebuild SET through_id=0,last_id=(SELECT max(id) FROM native_work_documents)`, target.WorkItemID, target.WorkItemID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, base, token, query string
		want                     tracker.NativeWorkItemID
	}{
		{"rebuilding project", f.base, f.token, "narwhal", target.WorkItemID},
		{"number during rebuild", f.base, f.token, strconv.Itoa(target.Number), target.WorkItemID},
		{"already indexed project", indexed.base, indexed.token, "narwhal", other.WorkItemID},
		{"empty project", empty.base, empty.token, "narwhal", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, test.base+"/work-items?q="+url.QueryEscape(test.query), test.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.NativeIssuePage
			decodeHubResponse(t, response, &page)
			if test.want == "" {
				if len(page.Items) != 0 {
					t.Fatalf("empty project returned %v", page.Items)
				}
				return
			}
			if len(page.Items) != 1 || page.Items[0].WorkItemID != test.want {
				t.Fatalf("items=%v want %s", page.Items, test.want)
			}
		})
	}
	for _, fixture := range []nativeFixture{indexed, empty} {
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
		if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); err != nil {
			t.Errorf("unaffected project %s: %v", fixture.project.ID, err)
		}
		matches, err := resolveNativeDuplicates(t.Context(), f.service.database.reader, scope, tracker.CreateIssue{Title: "Indexed narwhal"}, f.service.config.now(), false)
		want := 0
		if fixture.project.ID == indexed.project.ID {
			want = 1
		}
		if err != nil || len(matches) != want {
			t.Errorf("unaffected duplicate search project=%s matches=%v err=%v", fixture.project.ID, matches, err)
		}
	}
}

func TestNativeSearchIndexedIssueDuringRebuild(t *testing.T) {
	for _, mode := range []string{"deployment backfill", "scope refresh"} {
		t.Run(mode, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			legacy := f.create(t, "Legacy narwhal discussion")
			if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE seed(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seed WHERE n<?)
 INSERT INTO issues(native_id,organization_id,project_id,number,workflow_state_id,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,actor_kind,actor_principal_id)
 SELECT 'wi_rebuild_legacy_'||seed.n,i.organization_id,i.project_id,seed.n+1,i.workflow_state_id,'Legacy discussion','','','open','','','',i.created_at,i.updated_at,i.actor_kind,i.actor_principal_id
 FROM seed CROSS JOIN issues i WHERE i.native_id=?`, nativeSearchPostingBudget+100, legacy.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM native_work_scoped_fts;
 DELETE FROM native_work_search_lengths; DELETE FROM native_work_search_refresh`); err != nil {
				t.Fatal(err)
			}
			if mode == "deployment backfill" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_work_search_rebuild SET through_id=0,last_id=(SELECT max(id) FROM native_work_documents)`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_work_search_dirty(issue_id,organization_id,project_id)
 SELECT id,organization_id,project_id FROM issues WHERE native_id=?`, legacy.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			fresh := f.create(t, "Narwhal quasaruniqueterm")
			if more, err := f.service.database.backfillNativeSearch(t.Context()); err != nil || !more {
				t.Fatalf("index fresh document: more=%t err=%v", more, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			if err := requireNativeSearchIndex(t.Context(), f.service.database.reader, scope); !errors.Is(err, operatortool.ErrServiceUnavailable) {
				t.Fatalf("search rebuild not pending: %v", err)
			}
			for _, test := range []struct {
				query string
				want  []tracker.NativeWorkItemID
			}{
				{"quasaruniqueterm", []tracker.NativeWorkItemID{fresh.WorkItemID}},
				{"narwhal quasaruniqueterm", []tracker.NativeWorkItemID{fresh.WorkItemID, legacy.WorkItemID}},
			} {
				t.Run(test.query, func(t *testing.T) {
					params := url.Values{"q": {test.query}, "limit": {"2"}}
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var page tracker.NativeIssuePage
					decodeHubResponse(t, response, &page)
					var got []tracker.NativeWorkItemID
					for _, item := range page.Items {
						got = append(got, item.WorkItemID)
					}
					if !slices.Equal(got, test.want) {
						t.Fatalf("search during rebuild=%v want=%v", got, test.want)
					}
				})
			}
		})
	}
}

func TestNativeSearchPostingStorage(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			tokens := make([]string, 1000)
			for i := range tokens {
				tokens[i] = fmt.Sprintf("token%07d", i)
			}
			item := f.create(t, "Token storage")
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title=?,body='' WHERE native_id=?", strings.Join(tokens, " "), item.WorkItemID); err != nil {
				t.Fatal(err)
			}
			drainTestNativeSearch(t, f.service)
			if legacy {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_work_search_postings(scope,term,document_id,frequency,impact) SELECT l.scope,'ptok',l.document_id,1,1 FROM native_work_search_lengths l JOIN native_work_documents d ON d.id=l.document_id JOIN issues i ON i.id=d.issue_id WHERE i.native_id=?`, item.WorkItemID); err != nil {
					t.Fatal(err)
				}
				migration, err := migrationFiles.ReadFile("migrations/20261029102902_native_work_search_full_tokens.sql")
				if err != nil {
					t.Fatal(err)
				}
				up, _, ok := strings.Cut(string(migration), "-- +goose Down")
				if !ok {
					t.Fatal("missing migration rollback")
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), up); err != nil {
					t.Fatal(err)
				}
				var through int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT through_id FROM native_work_search_rebuild").Scan(&through); err != nil || through != 0 {
					t.Fatalf("rebuild cursor=%d err=%v", through, err)
				}
				ctx, timing := withWriterTiming(t.Context())
				f.service.maintainNativeSearch(ctx)
				if got := timing.txs.Load(); got != 1 {
					t.Fatalf("backfill transactions=%d want=1", got)
				}
				var old int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_search_postings WHERE term='ptok'").Scan(&old); err != nil || old != 0 {
					t.Fatalf("legacy prefix rows=%d err=%v", old, err)
				}
			}

			rows, err := f.service.database.reader.QueryContext(t.Context(), `SELECT p.scope,count(*) FROM native_work_search_postings p JOIN native_work_documents d ON d.id=p.document_id JOIN issues i ON i.id=d.issue_id WHERE i.native_id=? GROUP BY p.scope`, item.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			scopes := 0
			for rows.Next() {
				var scope string
				var count int
				if err := rows.Scan(&scope, &count); err != nil {
					t.Fatal(err)
				}
				if count != len(tokens) {
					t.Errorf("scope=%s postings=%d want=%d distinct tokens", scope, count, len(tokens))
				}
				scopes++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if scopes != 2 {
				t.Fatalf("scopes=%d want=2", scopes)
			}
		})
	}
}

func TestNativeSearchCommonTermBudget(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "PRAGMA synchronous=NORMAL"); err != nil {
		t.Fatal(err)
	}
	target := f.create(t, "Failure breaker classification blame phrase heuristic")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='' WHERE native_id=?", target.WorkItemID); err != nil {
		t.Fatal(err)
	}
	var steps int64
	pool := sql.OpenDB(duplicateCostConnector{timedConnector: timedConnector{driver: f.service.database.reader.Driver(), dsn: sqliteReaderDSN(f.service.database.path, f.service.config.BusyTimeout)}, steps: &steps})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	count := 0
	saturated := map[string]int64{}
	for _, size := range []int{100, 2000, 5000} {
		_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE seed(n) AS (
 SELECT ? UNION ALL SELECT n+1 FROM seed WHERE n<?)
 INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at)
 SELECT 'common-term-budget-'||seed.n,i.organization_id,i.project_id,i.native_id,1,seed.n,'failure breaker classification fairground','{}',i.native_created_at,i.native_created_at
 FROM seed CROSS JOIN issues i WHERE i.native_id=?`, count+1, size, target.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		count = size
		drainTestNativeSearch(t, f.service)
		postings, postingArgs := nativeSearchHits(scope, "", nativeSearchExpressions("fai"))
		var examined int
		if err := pool.QueryRowContext(t.Context(), postings+"SELECT count(*) FROM candidates", postingArgs...).Scan(&examined); err != nil {
			t.Fatal(err)
		}
		if examined > nativeSearchPostingBudget {
			t.Fatalf("documents=%d prefix candidates=%d budget=%d", size, examined, nativeSearchPostingBudget)
		}
		for _, query := range []string{"fai", "litestream WAL truncate", "failure breaker classification blame phrase heuristic", "failure breaker cifailure classification instance blame phrase"} {
			statement, args, err := nativeIssueSelection(scope, url.Values{"q": {query}}, false)
			if err != nil {
				t.Fatal(err)
			}
			steps = 0
			projected, err := f.service.projectIssuePage(t.Context(), pool, statement, args, "", query, scope)
			if err != nil {
				t.Fatal(err)
			}
			slices.SortStableFunc(projected, compareIssuePageRows)
			want := 1
			if query == "litestream WAL truncate" {
				want = 0
			}
			if len(projected) != want || want > 0 && issuePageItem(projected[0]).ID != string(target.WorkItemID) {
				t.Fatalf("documents=%d query=%q first page=%v", size, query, projected)
			}
			t.Logf("documents=%d query=%q VM steps=%d", size, query, steps)
			if steps == 0 || steps > 1000000 {
				t.Errorf("documents=%d query=%q VM steps=%d, fixed budget=1000000", size, query, steps)
			}
			if size == 2000 {
				saturated[query] = steps
			}
			if size == 5000 && steps > saturated[query]+2000 {
				t.Errorf("query=%q VM steps grew past posting budget: 2000=%d 5000=%d", query, saturated[query], steps)
			}
		}
	}
}

func TestNativeSearchPostingPrefixes(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	first := f.create(t, strings.Repeat("a", 64)+"one café cafeteria tokenalpha tokenbeta")
	second := f.create(t, strings.Repeat("a", 64)+"two cag tokenalphabet")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='' WHERE project_id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	drainTestNativeSearch(t, f.service)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, test := range []struct {
		name, query string
		plain       bool
		want        tracker.NativeWorkItemID
	}{
		{"long prefix one", strings.Repeat("a", 64) + "o", false, first.WorkItemID},
		{"long prefix two", strings.Repeat("a", 64) + "t", false, second.WorkItemID},
		{"long exact", strings.Repeat("a", 64) + "one", true, first.WorkItemID},
		{"unicode prefix", "café", false, first.WorkItemID},
		{"exact excludes longer tokens", "tokenalpha", true, first.WorkItemID},
		{"prefix excludes adjacent terms", "tokenb", false, first.WorkItemID},
	} {
		t.Run(test.name, func(t *testing.T) {
			expressions := nativeSearchExpressions(test.query)
			if test.plain {
				expressions[0] = strings.TrimSuffix(expressions[0], "*")
			}
			statement, args := nativeSearchHits(scope, "", expressions)
			ids, err := nativePageIDs(t.Context(), f.service.database.reader, statement+"SELECT i.native_id FROM ranked r JOIN issues i ON i.id=r.issue_id WHERE hit=1", args...)
			if err != nil || !slices.Equal(ids, []string{string(test.want)}) {
				t.Fatalf("matches=%v want=%s err=%v", ids, test.want, err)
			}
		})
	}
	statement, args := nativeSearchHits(scope, "", nativeSearchExpressions("token"))
	var frequency, terms int
	if err := f.service.database.reader.QueryRowContext(t.Context(), statement+"SELECT m.frequency,s.terms FROM indexed_matches m JOIN indexed_scores s ON s.document_id=m.document_id AND s.scope=m.scope JOIN issues i ON i.id=s.issue_id WHERE i.native_id=?", append(args, first.WorkItemID)...).Scan(&frequency, &terms); err != nil {
		t.Fatal(err)
	}
	if frequency != 10 || terms != 1 {
		t.Fatalf("multi-token prefix frequency=%d terms=%d want 10,1", frequency, terms)
	}
}

func TestNativeSearchBoundedCandidatePages(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	target := f.create(t, "Failure classification unique")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='' WHERE native_id=?", target.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE seed(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seed WHERE n<?)
 INSERT INTO issues(native_id,organization_id,project_id,number,workflow_state_id,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,actor_kind,actor_principal_id)
 SELECT 'wi_candidate_budget_'||seed.n,i.organization_id,i.project_id,seed.n+1,i.workflow_state_id,'Failure classification','','','open','','','',i.created_at,i.updated_at,i.actor_kind,i.actor_principal_id
 FROM seed CROSS JOIN issues i WHERE i.native_id=?`, nativeSearchPostingBudget+100, target.WorkItemID); err != nil {
		t.Fatal(err)
	}
	drainTestNativeSearch(t, f.service)
	params := url.Values{"q": {"failure classification unique"}, "limit": {"2"}}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.NativeIssuePage
	decodeHubResponse(t, response, &page)
	if page.Total != nativeSearchPostingBudget+1 || len(page.Items) != 2 || page.Items[0].WorkItemID != target.WorkItemID || page.NextCursor == "" {
		t.Fatalf("bounded first page=%+v", page)
	}
	total := page.Total
	late := f.create(t, "Failure classification unique unique unique")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='' WHERE native_id=?", late.WorkItemID); err != nil {
		t.Fatal(err)
	}
	drainTestNativeSearch(t, f.service)
	params.Set("cursor", page.NextCursor)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	if page.Total != total || len(page.Items) != 2 || slices.ContainsFunc(page.Items, func(item tracker.NativeIssue) bool {
		return item.WorkItemID == late.WorkItemID || item.WorkItemID == target.WorkItemID
	}) {
		t.Fatalf("cursor changed candidate snapshot=%+v", page)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	statement, args := nativeSearchHits(scope, "", nativeSearchExpressions("unique"))
	var id string
	if err := f.service.database.reader.QueryRowContext(t.Context(), statement+"SELECT i.native_id FROM ranked r JOIN issues i ON i.id=r.issue_id WHERE hit=1 ORDER BY search_score LIMIT 1", args...).Scan(&id); err != nil || id != string(late.WorkItemID) {
		t.Fatalf("late high-impact issue=%s want=%s err=%v", id, late.WorkItemID, err)
	}
}

func drainTestNativeSearch(t *testing.T, service *Service) {
	t.Helper()
	for {
		more, err := service.database.backfillNativeSearch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
}

type nativeSearchWriterProbe struct {
	timedConnector
	functions, steps *int64
}

type nativeSearchWriterProbeConn struct {
	*timedConn
	authorize func(*libc.TLS, uintptr, int32, uintptr, uintptr, uintptr, uintptr) int32
	progress  func(*libc.TLS, uintptr) int32
}

func (p nativeSearchWriterProbe) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := p.timedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	measured := &nativeSearchWriterProbeConn{timedConn: conn.(*timedConn)}
	measured.authorize = func(_ *libc.TLS, _ uintptr, operation int32, _ uintptr, name uintptr, _ uintptr, _ uintptr) int32 {
		if operation == sqlite3.SQLITE_FUNCTION {
			function := libc.GoString(name)
			if function == "native_search_tokens" || function == "native_search_postings" {
				*p.functions++
			}
		}
		return sqlite3.SQLITE_OK
	}
	measured.progress = func(*libc.TLS, uintptr) int32 { *p.steps += 100; return 0 }
	database := uintptr(reflect.ValueOf(measured.Conn).Elem().FieldByName("db").Uint())
	tls := libc.NewTLS()
	defer tls.Close()
	if code := sqlite3.Xsqlite3_set_authorizer(tls, database, nativeSearchCallback(measured.authorize), 0); code != sqlite3.SQLITE_OK {
		conn.Close()
		return nil, fmt.Errorf("install search writer probe: sqlite code %d", code)
	}
	sqlite3.Xsqlite3_progress_handler(tls, database, 100, nativeSearchCallback(measured.progress), 0)
	return measured, nil
}

func TestNativeSearchPostingWriterWork(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	issue := f.create(t, "Posting writer fixture")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body='narwhal' WHERE native_id=?", issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	drainTestNativeSearch(t, f.service)
	var functions, steps int64
	original := f.service.database.db
	pool := sql.OpenDB(nativeSearchWriterProbe{timedConnector: timedConnector{driver: original.Driver(), dsn: sqliteDSN(f.service.database.path, f.service.config.BusyTimeout), writer: true}, functions: &functions, steps: &steps})
	pool.SetMaxOpenConns(1)
	f.service.database.db = pool
	t.Cleanup(func() {
		f.service.database.db = original
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	var baseline int64
	for _, size := range []int{1024, 200 * 1024} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			functions = 0
			if _, err := pool.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", strings.Repeat("narwhal ", size/8), issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if functions != 0 {
				t.Fatalf("issue mutation invoked %d posting/token functions on writer", functions)
			}
			var id int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT d.id FROM native_work_documents d JOIN issues i ON i.id=d.issue_id WHERE i.native_id=? AND d.comment_id IS NULL`, issue.WorkItemID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			functions, steps = 0, 0
			more, err := f.service.database.backfillNativeSearch(t.Context())
			if err != nil || !more {
				t.Fatalf("index document: more=%t err=%v", more, err)
			}
			if functions != 0 {
				t.Fatalf("background publication invoked %d posting/token functions on writer", functions)
			}
			t.Logf("body=%d writer VM steps=%d posting/token calls=%d", size, steps, functions)
			if size == 1024 {
				baseline = steps
			} else if steps > baseline+1000 {
				t.Fatalf("writer work grew with body: 1KB=%d 200KB=%d", baseline, steps)
			}
			var tokens, frequency, queued int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT l.tokens,p.frequency,(SELECT count(*) FROM native_work_search_refresh WHERE document_id=l.document_id)
 FROM native_work_search_lengths l JOIN native_work_search_postings p ON p.document_id=l.document_id AND p.scope=l.scope
 WHERE l.document_id=? AND p.term='enarwhal' LIMIT 1`, id).Scan(&tokens, &frequency, &queued); err != nil {
				t.Fatal(err)
			}
			if tokens != int64(size/8+3) || frequency != int64(size/8) || queued != 0 {
				t.Fatalf("tokens=%d frequency=%d queued=%d", tokens, frequency, queued)
			}
		})
	}
	functions = 0
	created := f.create(t, "Unrelated writer mutation example")
	if functions != 0 {
		t.Fatalf("API issue mutation invoked %d posting/token functions on writer", functions)
	}
	var pending int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_search_refresh q JOIN native_work_documents d ON d.id=q.document_id JOIN issues i ON i.id=d.issue_id WHERE i.native_id=?`, created.WorkItemID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("API mutation pending=%d err=%v", pending, err)
	}

	migration, err := migrationFiles.ReadFile("migrations/20261029071651_native_work_search_impact.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, legacy, ok := strings.Cut(string(migration), "CREATE TRIGGER native_work_search_refresh_insert")
	if !ok {
		t.Fatal("missing legacy refresh trigger")
	}
	legacy, _, _ = strings.Cut(legacy, "-- +goose StatementEnd")
	if _, err := pool.ExecContext(t.Context(), "CREATE TRIGGER native_work_search_refresh_insert"+legacy); err != nil {
		t.Fatal(err)
	}
	functions = 0
	if _, err := pool.ExecContext(t.Context(), "UPDATE issues SET body='Legacy narwhal' WHERE native_id=?", issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if functions == 0 {
		t.Fatal("probe did not detect legacy writer tokenization")
	}
	t.Logf("legacy mutation posting/token calls=%d", functions)
}

func TestNativeSearchPostingRefreshRevision(t *testing.T) {
	for _, test := range []struct{ name, statement string }{
		{"body", "UPDATE issues SET body='replacement quasar' WHERE native_id=?"},
		{"scope", "UPDATE issues SET archived=1 WHERE native_id=?"},
		{"reporter", "UPDATE issues SET actor_principal_id='replacement-owner' WHERE native_id=?"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			issue := f.create(t, "Original narwhal")
			drainTestNativeSearch(t, f.service)
			var id int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT d.id FROM native_work_documents d JOIN issues i ON i.id=d.issue_id WHERE i.native_id=? AND d.comment_id IS NULL`, issue.WorkItemID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			prepared, err := f.service.database.prepareNativeSearchDocument(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), test.statement, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			published, err := f.service.database.publishNativeSearchDocument(t.Context(), tx, prepared)
			if err != nil || published {
				t.Fatalf("published stale snapshot=%t err=%v", published, err)
			}
			if err := f.service.database.commit(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			var queued int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_search_refresh WHERE document_id=?", id).Scan(&queued); err != nil || queued != 1 {
				t.Fatalf("queued=%d err=%v", queued, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			if test.name == "body" {
				for _, query := range []string{"quasar", "full"} {
					page, err := f.service.readIssues(t.Context(), scope, url.Values{"q": {query}})
					want := 0
					if query == "quasar" {
						want = 1
					}
					if err != nil || len(page.Items) != want {
						t.Fatalf("queued query=%s items=%d want=%d err=%v", query, len(page.Items), want, err)
					}
				}
			}
			drainTestNativeSearch(t, f.service)
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_search_refresh WHERE document_id=?", id).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("after retry queued=%d err=%v", queued, err)
			}
		})
	}
}

func TestNativeSearchPostingRefreshRejectsReusedDocumentID(t *testing.T) {
	for _, name := range []string{"issue identity", "comment identity", "content"} {
		t.Run(name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			first := f.create(t, "Original narwhal")
			second := f.create(t, "Replacement narwhal")
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title='Original narwhal' WHERE native_id=?", second.WorkItemID); err != nil {
				t.Fatal(err)
			}
			var comment, replacementComment any
			if name == "comment identity" {
				for index, id := range []string{"source-comment", "replacement-comment"} {
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at)
 SELECT ?,organization_id,project_id,native_id,1,?,'Original narwhal','{}',native_created_at,native_created_at FROM issues WHERE native_id=?`, id, index+1, first.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				comment, replacementComment = "source-comment", "replacement-comment"
			}
			drainTestNativeSearch(t, f.service)
			if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM native_work_documents WHERE issue_id IN (SELECT id FROM issues WHERE native_id IN (?,?))`, first.WorkItemID, second.WorkItemID); err != nil {
				t.Fatal(err)
			}
			insert := `INSERT INTO native_work_documents(issue_id,comment_id) SELECT id,? FROM issues WHERE native_id=?`
			result, err := f.service.database.db.ExecContext(t.Context(), insert, comment, first.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			id, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := f.service.database.prepareNativeSearchDocument(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_work_documents WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
			replacementIssue := first.WorkItemID
			if name == "issue identity" {
				replacementIssue = second.WorkItemID
			}
			if name == "content" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title='Replacement quasar' WHERE native_id=?", first.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			result, err = f.service.database.db.ExecContext(t.Context(), insert, replacementComment, replacementIssue)
			if err != nil {
				t.Fatal(err)
			}
			replacementID, err := result.LastInsertId()
			if err != nil || replacementID != id {
				t.Fatalf("replacement id=%d want=%d err=%v", replacementID, id, err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			published, err := f.service.database.publishNativeSearchDocument(t.Context(), tx, prepared)
			if err != nil || published {
				t.Fatalf("published replaced document=%t err=%v", published, err)
			}
			if err := f.service.database.commit(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			var queued int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_search_refresh WHERE document_id=?", id).Scan(&queued); err != nil || queued != 1 {
				t.Fatalf("replacement queued=%d err=%v", queued, err)
			}
			drainTestNativeSearch(t, f.service)
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_work_search_refresh WHERE document_id=?", id).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("after retry queued=%d err=%v", queued, err)
			}
		})
	}
}

func TestNativeSearchPostingRefreshMigrationPreservesQueuedSearch(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, "", "queued-search")
	for _, fixture := range []nativeFixture{f, other} {
		issue := fixture.create(t, "Queued migration fixture")
		response := performHubAPIRequest(t, f.service, http.MethodPost, fixture.base+"/work-items/"+string(issue.WorkItemID)+"/comments", fixture.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "queued-comment"}, Body: "Obsolete narwhal"})
		requireNativeStatus(t, response, http.StatusOK)
	}
	drainTestNativeSearch(t, f.service)
	migration, err := migrationFiles.ReadFile("migrations/20261029094650_native_work_search_refresh_outside_writer.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(migration), "-- +goose Down")
	if !ok {
		t.Fatal("missing rollback section")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `DROP TRIGGER native_work_search_refresh_insert;
 CREATE TRIGGER native_work_search_refresh_insert AFTER INSERT ON native_work_search_refresh BEGIN SELECT 1; END;
 UPDATE native_comments SET body='Current quasar' WHERE body='Obsolete narwhal';`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []nativeFixture{f, other} {
		var queued int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_work_search_refresh WHERE organization_id=? AND project_id=?`, fixture.project.OrganizationID, fixture.project.ID).Scan(&queued); err != nil || queued != 1 {
			t.Errorf("project=%s queued=%d want=1 err=%v", fixture.project.ID, queued, err)
		}
		for _, query := range []string{"quasar", "narwhal"} {
			scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
			page, err := f.service.readIssues(t.Context(), scope, url.Values{"q": {query}})
			want := 0
			if query == "quasar" {
				want = 1
			}
			if err != nil || len(page.Items) != want {
				t.Errorf("project=%s queued query=%s items=%d want=%d err=%v", fixture.project.ID, query, len(page.Items), want, err)
			}
		}
	}
}
