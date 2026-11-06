package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSearchExpressionsBound(t *testing.T) {
	terms := make([]string, 33)
	want := make([]string, 32)
	for index := range terms {
		terms[index] = "term" + strconv.Itoa(index)
		if index < len(want) {
			want[index] = `"` + terms[index] + `"*`
		}
	}
	for _, test := range []struct {
		name, query string
		want        []string
	}{
		{"empty", "!!!", []string{`""`}},
		{"normalized distinct terms", "Café CAFE café repo-path REPO", []string{`"café"*`, `"cafe"*`, `"repo"*`, `"path"*`}},
		{"at cap", strings.Join(terms[:32], " "), want},
		{"over cap", strings.Join(terms, " "), want},
		{"duplicates do not consume cap", strings.Repeat("TERM0 ", 40) + strings.Join(terms, " "), want},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeSearchExpressions(test.query); !slices.Equal(got, test.want) {
				t.Fatalf("expressions=%v want=%v", got, test.want)
			}
		})
	}
}

func TestNativeWorkSearch(t *testing.T) {
	f := newPostgresWorkViewFixture(t)
	user := f.user(t, "searcher", "member", "searcher@example.test", "read", "")
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	first := seedPostgresSearchIssue(t, f, 1, "Repository association deadlocks", "Unicorns decorate mock fixture data", at)
	second := seedPostgresSearchIssue(t, f, 2, "Repository association", "Other content", at)
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			for {
				more, err := searchstore.NewContent(f.service.runDetailStore()).RefreshOne(t.Context(), tenantSearchRepository{f.repository}, "org_security")
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
		}
		for _, test := range []struct {
			name, query, include string
			want                 tracker.NativeWorkItemID
		}{
			{"title rank", "repository association deadlocks", "summary", first},
			{"body substring", "corns decor", "summary", first},
			{"number", "#2", "summary", second},
			{"work projection", "unicorn", "work", first},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				response := f.request(t, user, http.MethodGet, f.base+"/work-items?"+url.Values{"q": {test.query}, "include": {test.include}, "limit": {"1"}}.Encode(), nil)
				if mode == "pending" {
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
					var failure apiErrorResponse
					decodeHubResponse(t, response, &failure)
					if failure.Code != "not_ready" {
						t.Fatalf("pending search code=%q", failure.Code)
					}
					return
				}
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.NativeIssuePage
				decodeHubResponse(t, response, &page)
				items := page.Items
				if test.include == "work" {
					if page.Work == nil {
						t.Fatal("work projection missing")
					}
					items = page.Work.Items
				}
				if len(items) != 1 || items[0].WorkItemID != test.want || items[0].Snippet == "" {
					t.Fatalf("search results=%+v", items)
				}
			})
		}
	}
	request := httptest.NewRequest(http.MethodGet, f.base+"/work-items", nil)
	request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
	credential, _, err := f.service.appCredential(f.service.echo.NewContext(request, httptest.NewRecorder()))
	if err != nil {
		t.Fatal(err)
	}
	identity := operatorIdentity(credential, "org_security")
	mcpContext := operatortool.WithConnection(t.Context(), operatortool.Connection{ID: "search-connection", Client: "search-client", Identity: identity, Resolve: func(ctx context.Context) (operatortool.Authority, error) {
		return f.service.operatorCurrentAuthority(ctx, credential, "org_security")
	}})
	mcpContext = context.WithValue(mcpContext, operatorCatalogKey{}, operatorCatalogAuthority{identity: identity, credential: credential})
	terms := make([]string, 32)
	for index := range terms {
		terms[index] = "t" + strconv.Itoa(index)
	}
	terms[31] = "unicorn"
	for _, transport := range []string{"stdio", "http"} {
		t.Run("mcp/"+transport, func(t *testing.T) {
			send := hostedContextProtocol(t, f.service, mcpContext, transport)
			for _, test := range []struct {
				name, query, state, snippet string
				want                        tracker.NativeWorkItemID
				dateFilters                 bool
			}{
				{"query and date filters", "unicorn", "Todo", "unicorn", first, true},
				{"number", "#2", "Todo", "", second, false},
				{"excluded state", "#2", "Done", "", "", false},
				{"at term cap", strings.Join(terms, " "), "", "unicorn", first, false},
				{"past term cap", "excluded " + strings.Join(terms, " "), "", "", "", false},
				{"duplicates before term cap", strings.Repeat("t0 ", 40) + strings.Join(terms, " "), "", "unicorn", first, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					args := map[string]any{"project_id": f.project, "query": test.query, "limit": 5}
					if test.state != "" {
						args["state"] = test.state
					}
					if test.dateFilters {
						args["created_after"], args["created_before"], args["updated_after"] = at.Format(time.RFC3339Nano), at.AddDate(0, 0, 1).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)
					}
					raw := hostedContextData(t, send("tools/call", operatortool.WorkList, args), false)
					var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					if test.want == "" {
						if len(result.Data.Items) != 0 {
							t.Fatalf("filtered tool results=%+v", result.Data.Items)
						}
						return
					}
					if len(result.Data.Items) != 1 || result.Data.Items[0].WorkItemID != test.want || result.Data.Items[0].Snippet == "" || !strings.Contains(strings.ToLower(result.Data.Items[0].Snippet), test.snippet) {
						t.Fatalf("tool search results=%+v", result.Data.Items)
					}
				})
			}
		})
	}
	for _, query := range []string{"\" OR NOT * : ()", "!!!"} {
		requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base+"/work-items?"+url.Values{"q": {query}}.Encode(), nil), http.StatusOK)
	}
}

func TestNativeWorkDateFilters(t *testing.T) {
	f := newPostgresWorkViewFixture(t)
	user := f.user(t, "dates", "member", "dates@example.test", "read", "")
	stamps := []string{"2026-10-11T23:59:59.999999Z", "2026-10-12T00:00:00Z", "2026-10-12T06:00:00+05:00", "2026-10-13T00:00:00Z", "2026-10-12T00:00:00.000001Z", "2026-10-12T00:00:00.001Z", "2026-10-12T00:00:00.0015Z"}
	ids := make([]tracker.NativeWorkItemID, len(stamps))
	for index, stamp := range stamps {
		date, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		ids[index] = seedPostgresSearchIssue(t, f, index+1, fmt.Sprintf("Date fixture %d", index), "", date)
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
		{"before microsecond", url.Values{"created_before": {"2026-10-12T00:00:00.000001Z"}}, []int{0, 1}, 200},
		{"fractional precision", url.Values{"updated_after": {"2026-10-12T00:00:00.0015Z"}}, []int{2, 3, 6}, 200},
		{"board rejects date selectors", url.Values{"include": {"board"}, "created_after": {"2026-10-12T00:00:00Z"}}, nil, 422},
		{"invalid date", url.Values{"created_after": {"private invalid date"}}, nil, 422},
		{"invalid before", url.Values{"created_before": {"today"}}, nil, 422},
		{"invalid updated", url.Values{"updated_after": {"2026-10-12"}}, nil, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, user, http.MethodGet, f.base+"/work-items?"+test.params.Encode(), nil)
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

func seedPostgresSearchIssue(t *testing.T, f postgresWorkViewFixture, number int, title, body string, at time.Time) tracker.NativeWorkItemID {
	t.Helper()
	id := tracker.NativeWorkItemID(fmt.Sprintf("wi_search_%d", number))
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: id, Number: number, Revision: 1}, Title: title, Body: body, State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: at, UpdatedAt: at}
	payload, err := viewstore.NewContent(f.service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	document, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: id, ID: "issue/" + string(id), SourceID: string(id), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: at}, searchstore.Text{Title: title, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_security", ProjectID: f.project, NativeID: id, Number: int64(number), Revision: 1, Title: title, ActorKind: new("human"), ActorPrincipalID: new("example"), WorkflowStateName: new("Todo"), CreatedAt: at, UpdatedAt: at, NativeCreatedAt: at, NativeUpdatedAt: at}); err != nil {
			return err
		}
		if err := w.PutSearch(t.Context(), document, 0); err != nil {
			return err
		}
		record := viewstore.BoardRecord{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: id, SourceRevision: 1, SourceProjectRevision: project.Core.ContentSettingsRevision, Position: viewstore.BoardPosition{Lane: 1, State: "Todo", Activity: &at, Identifier: fmt.Sprintf("%s#%d", f.project, number)}, ActorPrincipalID: "example", CreatedAt: at, UpdatedAt: at, Payload: payload}
		return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: f.project, WorkItemID: id, ExpectedRevision: 1, ExpectedProjectRevision: project.Core.ContentSettingsRevision, Current: &record})
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
