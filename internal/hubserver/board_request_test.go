package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardModelCardPreservesCollections(t *testing.T) {
	for _, role := range []string{"operator", "reporter"} {
		for _, populated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/populated=%t", role, populated), func(t *testing.T) {
				issue := tracker.NativeIssue{}
				if populated {
					issue.Labels, issue.Assignees = []string{"example"}, []string{"owner"}
					issue.Dependencies = []tracker.NativeWorkItemID{"wi_dependency"}
					issue.Blockers = []tracker.NativeDependency{{ID: "wi_dependency"}}
					issue.Dependents, issue.Related = issue.Blockers, issue.Blockers
					issue.ExternalReferences = []tracker.ExternalReference{{Provider: "github", ID: "1"}}
				}
				reporter := &viewstore.BoardReporterRelations{Dependencies: issue.Dependencies, Blockers: issue.Blockers, Dependents: issue.Dependents, Related: issue.Related}
				card, err := boardModelCard(nativeScope{credential: apiCredential{HostedRole: role}}, viewstore.BoardRecord{PageIssue: &issue, ReporterRelations: reporter})
				if err != nil {
					t.Fatal(err)
				}
				if populated && !reflect.DeepEqual(card.Issue, issue) {
					t.Fatal("populated collections changed")
				}
				raw, err := json.Marshal(card.Issue)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				for _, field := range []string{"labels", "assignees", "dependencies", "blockers", "dependents", "related", "external_references"} {
					if len(fields[field]) == 0 || fields[field][0] != '[' {
						t.Errorf("%s is not an array: %s", field, fields[field])
					}
				}
				compact := operatortool.NativeItemPage("project_example", tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{card.Issue}})
				raw, err = json.Marshal(compact.Items[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["external_references"]) != "[]" {
					t.Errorf("work_list external_references must be [], got %s", fields["external_references"])
				}
			})
		}
	}
}

func TestBoardSearchFallbackHasFixedSQLBudget(t *testing.T) {
	expectedCounts := map[string][2]int{}
	for _, size := range []int{2, 50} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			details := &memoryRunDetails{}
			service := newPostgresNativeService(t, Config{RunDetails: details})
			original := service.database.postgres
			value, _ := projectRecordDatabases.Load(original)
			admin := value.(*sql.DB)
			wire := &boardWire{}
			measured := boardWireRepository(t, admin, wire)
			t.Cleanup(func() { service.config.TenantRepository = original; service.config.AccessRepository = original })

			scopes := []nativeScope{}
			for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				fixture := newNativeFixture(t, service, org, "Example search")
				service.localOrganization = string(org)
				credential, _, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(fixture.token), "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: org, project: fixture.project.ID, credential: credential}
				now := service.config.now().UTC().Truncate(time.Microsecond)
				for seeded := 1; seeded <= size; seeded++ {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_search_%04d", seeded))
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, Number: seeded, Revision: 1}, Title: "Release audit credential", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
					payload, err := viewstore.NewContent(details).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
					if err != nil {
						t.Fatal(err)
					}
					document, err := searchstore.NewContent(details).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, ID: "issue/" + string(id), SourceID: string(id), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: now}, searchstore.Text{Title: issue.Title, Body: "Resume\nfingerprint: example-key\n"})
					if err != nil {
						t.Fatal(err)
					}
					if err := original.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
						principal := "example"
						if seeded == 1 {
							principal = "another-owner"
						}
						item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.project, NativeID: id, Number: int64(seeded), Title: issue.Title, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: &principal})
						if err != nil {
							return err
						}
						if err := w.PutSearch(t.Context(), document, 0); err != nil {
							return err
						}
						row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%04d", scope.project, seeded)}, CreatedAt: now, UpdatedAt: now, Payload: payload, ActorPrincipalID: "example", PageIssue: &issue, ListIssue: &issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: seeded, Actor: issue.Actor}}
						return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
					}); err != nil {
						t.Fatal(err)
					}
				}
				for {
					more, err := searchstore.NewContent(details).RefreshOne(t.Context(), tenantSearchRepository{original}, org)
					if err != nil {
						t.Fatal(err)
					}
					if !more {
						break
					}
				}
				scopes = append(scopes, scope)
			}
			if _, err := admin.ExecContext(t.Context(), `ANALYZE search_documents, search_postings, native_board_items, issues, projects`); err != nil {
				t.Fatal(err)
			}
			service.config.TenantRepository, service.config.AccessRepository = measured, measured
			details.beforeRead = func(context.Context) error { return errors.New("search page read an object") }
			for _, scope := range scopes {
				org := scope.organization
				service.localOrganization = string(org)
				for _, role := range []string{"operator", "reporter"} {
					selected := scope
					if role == "reporter" {
						selected.credential.HostedRole, selected.credential.HostedPrincipal = role, "example"
					}
					for _, include := range []string{"summary", "board", "work"} {
						for _, filter := range []struct {
							name, key, value string
							match            bool
						}{{"query", "q", "resume", true}, {"no query match", "q", "absentuniqueterm", false}, {"fingerprint", "fingerprint", "example-key", true}, {"no fingerprint match", "fingerprint", "absent-key", false}} {
							key := role + "/" + include + "/" + filter.name
							t.Run(fmt.Sprintf("%s/%s", org, key), func(t *testing.T) {
								if err := measured.Ping(t.Context()); err != nil {
									t.Fatal(err)
								}
								wire.reset()
								page, err := service.readBoardRecords(t.Context(), selected, url.Values{"include": {include}, "limit": {"200"}, filter.key: {filter.value}})
								exchanges, statements := wire.stop()
								if err != nil {
									t.Fatal(err)
								}
								want := 0
								if filter.match {
									want = size
									if role == "reporter" {
										want--
									}
								}
								if len(page.Items) != want {
									t.Fatalf("items=%d want=%d", len(page.Items), want)
								}
								items := slices.Clone(page.Items)
								if include == "work" {
									if page.Work == nil || len(page.Work.Items) != want {
										t.Fatalf("work page=%+v want=%d", page.Work, want)
									}
									items = append(items, page.Work.Items...)
								}
								for _, item := range items {
									if item.OrganizationID != org || item.ProjectID != scope.project || role == "reporter" && item.WorkItemID == "wi_search_0001" {
										t.Fatalf("search leaked unauthorized item: %+v", item)
									}
									if filter.key == "q" && !strings.Contains(strings.ToLower(item.Snippet), "credential") {
										t.Fatalf("search snippet=%q", item.Snippet)
									}
								}
								if exchanges > 9 || statements > 16 {
									t.Errorf("wire exchanges=%d statements=%d", exchanges, statements)
								}
								if expected, ok := expectedCounts[key]; ok && expected != [2]int{exchanges, statements} {
									t.Errorf("SQL/wire cost changed with data size or tenant: got %d/%d want %v", exchanges, statements, expected)
								}
								expectedCounts[key] = [2]int{exchanges, statements}
								t.Logf("wire exchanges=%d statements=%d", exchanges, statements)
							})
						}
					}
				}
			}
		})
	}
}

func TestBoardRequestHasFixedSQLBudget(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	original := service.database.postgres
	repository := service.config.TenantRepository
	value, _ := projectRecordDatabases.Load(original)
	wire := &boardWire{}
	if delay, err := strconv.Atoi(os.Getenv("A5_BOARD_RTT_MS")); err == nil {
		wire.delay = time.Duration(delay) * time.Millisecond
	}
	measured := boardWireRepository(t, value.(*sql.DB), wire)
	t.Cleanup(func() { service.config.TenantRepository = original; service.config.AccessRepository = original })
	expectedCounts := map[string][2]int{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		service.config.TenantRepository = original
		service.config.AccessRepository = original
		details.beforeRead = nil
		fixture := newNativeFixture(t, service, org, "Example")
		service.localOrganization = string(org)
		scope := nativeScope{organization: org, project: fixture.project.ID, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		now := service.config.now().UTC().Truncate(time.Microsecond)
		seeded := 0
		for _, size := range []int{20, 500} {
			service.config.TenantRepository = original
			service.config.AccessRepository = original
			details.beforeRead = nil
			for seeded < size {
				seeded++
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_example_%04d", seeded))
				card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, Number: seeded, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}}
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
				if err != nil {
					t.Fatal(err)
				}
				err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.project, NativeID: id, Number: int64(seeded), Title: "Example", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("example")})
					if err != nil {
						return err
					}
					row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%04d", scope.project, seeded)}, Dispatchable: true, CreatedAt: now, UpdatedAt: now, Payload: payload, ActorPrincipalID: "example", PageIssue: &card.Issue, ListIssue: &card.Issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{DependencyReady: true}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: "Example", Number: seeded, Actor: tracker.Actor{Kind: "human"}}}
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			service.config.TenantRepository = measured
			service.config.AccessRepository = measured
			for _, limit := range []int{1, 10, 200} {
				for _, include := range []string{"board", "work", "summary", ""} {
					t.Run(fmt.Sprintf("%s/%d/%d/%s", org, size, limit, include), func(t *testing.T) {
						details.beforeRead = func(context.Context) error { return errors.New("page request read an object") }
						statements := []string{}
						ctx := postgres.WithTransactionTiming(t.Context(), func(string, time.Time) {}, func(_ time.Time, query string) { statements = append(statements, query) })
						if err := measured.Ping(t.Context()); err != nil {
							t.Fatal(err)
						}
						wire.reset()
						page := boardHandlerArrayContract(t, service, ctx, scope, url.Values{"include": {include}, "limit": {fmt.Sprint(limit)}})
						if len(page.Items) == 0 || len(page.Items) > min(limit, size) || page.Total != -1 {
							t.Fatalf("items=%d total=%d", len(page.Items), page.Total)
						}
						if include == "work" && ((len(page.Work.Items) == 0 || len(page.Work.Items) > min(limit, size)) || page.Work.Items[0].Number != size) {
							t.Fatalf("work page=%+v", page.Work)
						}
						for _, item := range page.Items {
							if item.OrganizationID != org {
								t.Fatal("cross-tenant item")
							}
						}
						exchanges, actualStatements := wire.stop()
						if exchanges > 9 || actualStatements > 16 {
							t.Errorf("wire exchanges=%d statements=%d", exchanges, actualStatements)
						}
						t.Logf("wire exchanges=%d statements=%d", exchanges, actualStatements)
						reporter := scope
						reporter.credential.HostedRole, reporter.credential.HostedPrincipal = "reporter", "example"
						reporterPage := boardHandlerArrayContract(t, service, t.Context(), reporter, url.Values{"include": {include}, "limit": {fmt.Sprint(limit)}})
						if len(reporterPage.Items) != len(page.Items) {
							t.Fatal("reporter lost owned items")
						}
						boardHandlerArrayContract(t, service, t.Context(), scope, url.Values{"include": {include}, "state": {"Backlog"}})
						key := fmt.Sprintf("%s/%d", include, limit)
						if expected, ok := expectedCounts[key]; ok {
							if expected != [2]int{exchanges, actualStatements} {
								t.Errorf("SQL/wire cost changed with data size or tenant: got %d/%d want %v", exchanges, actualStatements, expected)
							}
						} else {
							expectedCounts[key] = [2]int{exchanges, actualStatements}
						}

						if samples, err := strconv.Atoi(os.Getenv("A5_BOARD_RTT_SAMPLES")); err == nil && samples > 0 && limit == 10 {
							durations := make([]time.Duration, 0, samples)
							for range samples {
								wire.reset()
								started := time.Now()
								_, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {include}, "limit": {"10"}})
								durations = append(durations, time.Since(started))
								wire.stop()
								if err != nil {
									t.Fatal(err)
								}
							}
							slices.Sort(durations)
							t.Logf("RTT=%s samples=%d p50=%s p95=%s", wire.delay, samples, durations[len(durations)/2], durations[(len(durations)-1)*95/100])
							if durations[(len(durations)-1)*95/100] > time.Second || durations[len(durations)/2] > 250*time.Millisecond {
								t.Error("page latency exceeds p50=250ms or p95=1s")
							}
						}
						count := len(statements) + 2
						t.Logf("statements=%d", count)
						modelReads := 0
						for _, query := range statements {
							if strings.Contains(query, "native_board_sequences") || strings.Contains(query, "WITH page AS MATERIALIZED") {
								modelReads++
							}
							if strings.Contains(query, "RelationshipBatch") || strings.Contains(query, "FROM issue_dependency_reasons") || strings.Contains(query, "count(*)") {
								t.Errorf("serving query derives page data: %s", strings.Split(query, "\n")[0])
							}
						}
						if modelReads != 2 {
							t.Errorf("model reads=%d want 2", modelReads)
						}
						if count > 16 {
							for _, query := range statements {
								t.Log(strings.Split(query, "\n")[0])
							}
							t.Errorf("SQL statements=%d, want <=16", count)
						}
					})
				}
			}
			t.Run(fmt.Sprintf("%s/%d/legacy", org, size), func(t *testing.T) {
				details.beforeRead = func(context.Context) error { return errors.New("legacy board accessed an object") }
				wire.reset()
				result, err := service.readWorkBoardProject(t.Context(), scope, savedWorkState{View: "board", Lanes: []string{"Backlog", "Todo"}, CompletedWindow: "24h"})
				exchanges, statements := wire.stop()
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Cards) != size || len(result.Issues) != size {
					t.Errorf("legacy cards=%d issues=%d want=%d", len(result.Cards), len(result.Issues), size)
				}
				if result.Work == nil || len(result.Work.Lanes) != 1 || result.Work.Lanes[0].State != "Todo" || result.Work.Lanes[0].Total != size || len(result.Work.ShippedWindows) != 3 || result.BacklogTotal != 0 {
					t.Errorf("legacy summary=%+v backlog=%d", result.Work, result.BacklogTotal)
				}
				for _, card := range result.Cards {
					if card.Issue.OrganizationID != org {
						t.Fatal("legacy tenant leak")
					}
				}
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var old struct {
					Cards        []json.RawMessage          `json:"cards"`
					Work         *tracker.NativeWorkSummary `json:"work"`
					BacklogTotal int                        `json:"backlogTotal"`
				}
				if err := json.Unmarshal(raw, &old); err != nil || len(old.Cards) != size || old.Work == nil {
					t.Errorf("old client cards=%d error=%v", len(old.Cards), err)
				}
				if expected, ok := expectedCounts["legacy"]; ok && expected != [2]int{exchanges, statements} {
					t.Errorf("legacy SQL/wire changed: %d/%d want %v", exchanges, statements, expected)
				}
				expectedCounts["legacy"] = [2]int{exchanges, statements}
				t.Logf("legacy wire exchanges=%d statements=%d", exchanges, statements)
			})
			for _, part := range []string{"counts", "status"} {
				t.Run(fmt.Sprintf("%s/%d/%s", org, size, part), func(t *testing.T) {
					boardPartBudget(t, service, measured, wire, details, scope, size, part, expectedCounts)
				})
			}
		}
	}
}

func boardHandlerArrayContract(t *testing.T, service *Service, ctx context.Context, scope nativeScope, params url.Values) tracker.NativeIssuePage {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/work-items?"+params.Encode(), nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(request, recorder)
	c.Set("native_scope", scope)
	if err := service.listNativeIssues(c); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("board handler status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var page tracker.NativeIssuePage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || params.Get("include") == "board" && page.Cards == nil {
		t.Fatal("empty page collections must be arrays")
	}
	if params.Get("include") == "board" || params.Get("include") == "work" {
		if page.Work == nil || page.Work.Items == nil || page.Work.Lanes == nil || page.Work.ShippedWindows == nil {
			t.Fatal("empty work summary collections must be arrays")
		}
	}
	var check func(json.RawMessage)
	check = func(raw json.RawMessage) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if _, issue := object["work_item_id"]; issue {
			for _, field := range []string{"labels", "assignees", "dependencies", "blockers", "dependents", "related", "external_references"} {
				if string(object[field]) != "[]" {
					t.Errorf("%s must be [], got %s", field, object[field])
				}
			}
		}
		for _, field := range []string{"items", "cards", "lanes", "shipped_windows"} {
			if value, exists := object[field]; exists {
				var values []json.RawMessage
				if err := json.Unmarshal(value, &values); err != nil || values == nil {
					t.Fatalf("%s must be an array, got %s", field, value)
				}
				for _, item := range values {
					check(item)
				}
			}
		}
		for _, field := range []string{"issue", "work"} {
			if value, exists := object[field]; exists {
				check(value)
			}
		}
	}
	check(recorder.Body.Bytes())
	operator, err := json.Marshal(operatortool.NativeWorkPageView(string(scope.project), page))
	if err != nil {
		t.Fatal(err)
	}
	check(operator)
	if directory := os.Getenv("DETENT_TEST_BOARD_CONTRACT_DIR"); directory != "" {
		name := fmt.Sprintf("%s-%s-%s-%s-%s.json", scope.organization, scope.credential.HostedRole, params.Get("include"), params.Get("limit"), params.Get("state"))
		if err := os.WriteFile(filepath.Join(directory, name), recorder.Body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "operator-"+name), operator, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return page
}

func boardPartBudget(t *testing.T, service *Service, measured *postgres.Repository, wire *boardWire, details *memoryRunDetails, scope nativeScope, size int, part string, expectedCounts map[string][2]int) {
	t.Helper()
	details.beforeRead = func(context.Context) error { return errors.New("page part read an object") }
	statements := []string{}
	ctx := postgres.WithTransactionTiming(t.Context(), func(string, time.Time) {}, func(_ time.Time, query string) { statements = append(statements, query) })
	params := url.Values{}
	for index := 1; index <= 10; index++ {
		params.Add("item", fmt.Sprintf("wi_example_%04d", index))
	}
	read := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/board/"+part+"?"+params.Encode(), nil).WithContext(ctx)
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.Set("native_scope", scope)
		if err := service.boardModelEndpoint(c, part == "status"); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	if err := measured.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	wire.reset()
	response := read()
	exchanges, actualStatements := wire.stop()
	if exchanges > 9 || actualStatements > 16 {
		t.Fatalf("wire exchanges=%d statements=%d", exchanges, actualStatements)
	}
	if expected, ok := expectedCounts[part]; ok && expected != [2]int{exchanges, actualStatements} {
		t.Errorf("SQL/wire cost changed with data size or tenant: got %d/%d want %v", exchanges, actualStatements, expected)
	}
	expectedCounts[part] = [2]int{exchanges, actualStatements}
	t.Logf("wire exchanges=%d statements=%d", exchanges, actualStatements)
	modelReads := 0
	for _, query := range statements {
		if strings.Contains(query, "FROM native_board_page_summaries c") || strings.Contains(query, "b.page_projected_at") {
			modelReads++
		}
	}
	if modelReads != 1 {
		t.Errorf("model reads=%d want 1", modelReads)
	}
	if part == "counts" {
		var envelope viewstore.BoardSummaryPage
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range envelope.Rows {
			if row.Kind == "total" {
				found = row.Total == int64(size)
			}
		}
		if !found {
			t.Fatalf("missing total=%d in %+v", size, envelope.Rows)
		}
	} else {
		var rows []viewstore.BoardStatusRecord
		if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil || len(rows) != 10 {
			t.Fatalf("status rows=%d error=%v", len(rows), err)
		}
	}
	if samples, err := strconv.Atoi(os.Getenv("A5_BOARD_RTT_SAMPLES")); err == nil && samples > 0 {
		durations := make([]time.Duration, 0, samples)
		for range samples {
			wire.reset()
			started := time.Now()
			read()
			durations = append(durations, time.Since(started))
			wire.stop()
		}
		slices.Sort(durations)
		t.Logf("RTT=%s samples=%d p50=%s p95=%s", wire.delay, samples, durations[len(durations)/2], durations[(len(durations)-1)*95/100])
		if durations[(len(durations)-1)*95/100] > time.Second {
			t.Error("page part latency exceeds p95=1s")
		}
	}
}

func TestNativeIssueChangePageHasFixedSQLBudget(t *testing.T) {
	var expected [2]int
	for _, size := range []int{2, 50} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			details := &memoryRunDetails{}
			service := newPostgresNativeService(t, Config{RunDetails: details})
			original := service.database.postgres
			value, ok := projectRecordDatabases.Load(original)
			if !ok {
				t.Fatal("fixture database is unavailable")
			}
			admin, ok := value.(*sql.DB)
			if !ok {
				t.Fatal("fixture database has an invalid type")
			}
			wire := &boardWire{}
			measured := boardWireRepository(t, admin, wire)
			t.Cleanup(func() { service.config.TenantRepository = original })
			scopes := []nativeScope{}
			for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				fixture := newNativeFixture(t, service, org, "Example Change")
				scope := nativeScope{organization: org, project: fixture.project.ID}
				scopes = append(scopes, scope)
				now := service.config.now().UTC().Truncate(time.Microsecond)
				for number := 1; number <= size; number++ {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_change_%04d", number))
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, Number: number, Revision: 1}, Title: "Example Change", State: "Todo", Change: &tracker.NativeIssueChange{Connector: tracker.NativeChangeConnectorNone, ChangeID: "change_" + string(org) + "_" + string(id), HeadSHA: strings.Repeat("a", 40)}}
					payload, err := viewstore.NewContent(details).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
					if err != nil {
						t.Fatal(err)
					}
					if err := original.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
						item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.project, NativeID: id, Number: int64(number), Title: issue.Title, WorkflowStateName: new("Todo")})
						if err != nil {
							return err
						}
						row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%d", number)}, CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, ChangePrincipalID: "example"}}
						return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			service.config.TenantRepository = measured
			details.beforeRead = func(context.Context) error { return errors.New("Change page read an object") }
			for _, scope := range scopes {
				for _, role := range []string{"operator", "reporter"} {
					t.Run(fmt.Sprintf("%s/%s", scope.organization, role), func(t *testing.T) {
						selected := scope
						selected.credential.HostedRole, selected.credential.HostedPrincipal = role, "example"
						if err := measured.Ping(t.Context()); err != nil {
							t.Fatal(err)
						}
						issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: "wi_change_0002", Revision: 1}}
						wire.reset()
						err := service.readNativeIssueChangePage(t.Context(), selected, &issue)
						exchanges, statements := wire.stop()
						if err != nil || issue.Change == nil || issue.Change.ChangeID != "change_"+string(scope.organization)+"_wi_change_0002" || issue.Change.HeadSHA != strings.Repeat("a", 40) {
							t.Fatalf("projected Change=%+v, error=%v", issue.Change, err)
						}
						cost := [2]int{exchanges, statements}
						if exchanges > 3 || statements > 4 {
							t.Errorf("Change read cost=%v", cost)
						}
						if expected != [2]int{} && expected != cost {
							t.Errorf("Change SQL/wire cost varies by size or tenant: got %v want %v", cost, expected)
						}
						expected = cost
					})
				}
			}
		})
	}
}
