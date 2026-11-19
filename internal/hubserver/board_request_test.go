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
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardModelCardPreservesCollections(t *testing.T) {
	t.Parallel()
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
				card, err := boardModelCard(hubscope.NativeScope{Credential: hubapi.APICredential{HostedRole: role}}, viewstore.BoardRecord{PageIssue: &issue, ReporterRelations: reporter})
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

func TestBoardSearchFallbackScopesResults(t *testing.T) {
	t.Parallel()
	const size = 2
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	original := service.database.postgres
	value, _ := projectRecordDatabases.Load(original)
	admin := value.(*sql.DB)
	scopes := []hubscope.NativeScope{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example search")
		service.localOrganization = string(org)
		credential, _, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(fixture.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := hubscope.NativeScope{Organization: org, Project: fixture.project.ID, Credential: credential}
		now := service.config.Clock().UTC().Truncate(time.Microsecond)
		for seeded := 1; seeded <= size; seeded++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_search_%04d", seeded))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, Number: seeded, Revision: 1}, Title: "Release audit credential", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
			payload, err := viewstore.NewContent(details).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			if err := original.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				principal := "example"
				if seeded == 1 {
					principal = "another-owner"
				}
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.Project, NativeID: id, Number: int64(seeded), Title: issue.Title, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: &principal})
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%04d", scope.Project, seeded)}, CreatedAt: now, UpdatedAt: now, Payload: payload, ActorPrincipalID: "example", PageIssue: &issue, ListIssue: &issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: issue.Title, Number: seeded, Actor: issue.Actor}}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.Project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
			searchable := issue
			searchable.Body = issueorigin.Stamp("Resume", issueorigin.Origin{Kind: "worker", Fingerprint: "example-key"})
			if seeded == 1 {
				searchable.Actor.PrincipalID = "another-owner"
			}
			if err := original.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				return publishTestItemPage(t.Context(), admin, w, searchable)
			}); err != nil {
				t.Fatal(err)
			}
		}
		scopes = append(scopes, scope)
	}
	details.beforeRead = func(context.Context) error { return errors.New("search page read an object") }
	for _, scope := range scopes {
		org := scope.Organization
		service.localOrganization = string(org)
		for _, role := range []string{"operator", "reporter"} {
			selected := scope
			if role == "reporter" {
				selected.Credential.HostedRole, selected.Credential.HostedPrincipal = role, "example"
			}
			for _, include := range []string{"summary", "board", "work"} {
				for _, filter := range []struct {
					name, key, value string
					match            bool
				}{{"query", "q", "resume", true}, {"no query match", "q", "absentuniqueterm", false}, {"fingerprint", "fingerprint", "example-key", true}, {"no fingerprint match", "fingerprint", "absent-key", false}} {
					key := role + "/" + include + "/" + filter.name
					t.Run(fmt.Sprintf("%s/%s", org, key), func(t *testing.T) {
						page, err := service.readBoardRecords(t.Context(), selected, url.Values{"include": {include}, "limit": {"200"}, filter.key: {filter.value}})
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
							if item.OrganizationID != org || item.ProjectID != scope.Project || role == "reporter" && item.WorkItemID == "wi_search_0001" {
								t.Fatalf("search leaked unauthorized item: %+v", item)
							}
							if filter.key == "q" && !strings.Contains(strings.ToLower(item.Snippet), "credential") {
								t.Fatalf("search snippet=%q", item.Snippet)
							}
						}
					})
				}
			}
		}
	}
}

func TestBoardRequestPreservesPageContract(t *testing.T) {
	t.Parallel()
	const size = 11
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	repository := service.config.TenantRepository
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		details.beforeRead = nil
		fixture := newNativeFixture(t, service, org, "Example")
		service.localOrganization = string(org)
		scope := hubscope.NativeScope{Organization: org, Project: fixture.project.ID, Credential: hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}}
		now := service.config.Clock().UTC().Truncate(time.Microsecond)
		for seeded := 1; seeded <= size; seeded++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_example_%04d", seeded))
			card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, Number: seeded, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}}
			payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.Project, NativeID: id, Number: int64(seeded), Title: "Example", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("example")})
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%04d", scope.Project, seeded)}, Dispatchable: true, CreatedAt: now, UpdatedAt: now, Payload: payload, ActorPrincipalID: "example", PageIssue: &card.Issue, ListIssue: &card.Issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{DependencyReady: true}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: "Example", Number: seeded, Actor: tracker.Actor{Kind: "human"}}}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.Project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, limit := range []int{1, 10, 200} {
			for _, include := range []string{"board", "work", "summary", ""} {
				t.Run(fmt.Sprintf("%s/%d/%d/%s", org, size, limit, include), func(t *testing.T) {
					details.beforeRead = func(context.Context) error { return errors.New("page request read an object") }
					page := boardHandlerArrayContract(t, service, t.Context(), scope, url.Values{"include": {include}, "limit": {fmt.Sprint(limit)}})
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
					reporter := scope
					reporter.Credential.HostedRole, reporter.Credential.HostedPrincipal = "reporter", "example"
					reporterPage := boardHandlerArrayContract(t, service, t.Context(), reporter, url.Values{"include": {include}, "limit": {fmt.Sprint(limit)}})
					if len(reporterPage.Items) != len(page.Items) {
						t.Fatal("reporter lost owned items")
					}
					boardHandlerArrayContract(t, service, t.Context(), scope, url.Values{"include": {include}, "state": {"Backlog"}})
				})
			}
		}
		for _, part := range []string{"counts", "status"} {
			t.Run(fmt.Sprintf("%s/%d/%s", org, size, part), func(t *testing.T) {
				boardPartContract(t, service, details, scope, size, part)
			})
		}
	}
}

func boardHandlerArrayContract(t *testing.T, service *Service, ctx context.Context, scope hubscope.NativeScope, params url.Values) tracker.NativeIssuePage {
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
	operator, err := json.Marshal(operatortool.NativeWorkPageView(string(scope.Project), page))
	if err != nil {
		t.Fatal(err)
	}
	check(operator)
	if directory := os.Getenv("DETENT_TEST_BOARD_CONTRACT_DIR"); directory != "" {
		name := fmt.Sprintf("%s-%s-%s-%s-%s.json", scope.Organization, scope.Credential.HostedRole, params.Get("include"), params.Get("limit"), params.Get("state"))
		if err := os.WriteFile(filepath.Join(directory, name), recorder.Body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "operator-"+name), operator, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return page
}

func boardPartContract(t *testing.T, service *Service, details *memoryRunDetails, scope hubscope.NativeScope, size int, part string) {
	t.Helper()
	details.beforeRead = func(context.Context) error { return errors.New("page part read an object") }
	params := url.Values{}
	for index := 1; index <= 10; index++ {
		params.Add("item", fmt.Sprintf("wi_example_%04d", index))
	}
	read := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/board/"+part+"?"+params.Encode(), nil).WithContext(t.Context())
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
	response := read()
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
}

func TestNativeIssueChangePagePreservesScopedChange(t *testing.T) {
	t.Parallel()
	const size = 2
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	original := service.database.postgres
	scopes := []hubscope.NativeScope{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newNativeFixture(t, service, org, "Example Change")
		scope := hubscope.NativeScope{Organization: org, Project: fixture.project.ID}
		scopes = append(scopes, scope)
		now := service.config.Clock().UTC().Truncate(time.Microsecond)
		for number := 1; number <= size; number++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_change_%04d", number))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, Number: number, Revision: 1}, Title: "Example Change", State: "Todo", Change: &tracker.NativeIssueChange{Connector: tracker.NativeChangeConnectorNone, ChangeID: "change_" + string(org) + "_" + string(id), HeadSHA: strings.Repeat("a", 40)}}
			payload, err := viewstore.NewContent(details).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			if err := original.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.Project, NativeID: id, Number: int64(number), Title: issue.Title, WorkflowStateName: new("Todo")})
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.Project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%d", number)}, CreatedAt: now, UpdatedAt: now, Payload: payload, PageIssue: &issue, ListIssue: &issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, ChangePrincipalID: "example"}}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.Project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	details.beforeRead = func(context.Context) error { return errors.New("Change page read an object") }
	for _, scope := range scopes {
		for _, role := range []string{"operator", "reporter"} {
			t.Run(fmt.Sprintf("%s/%s", scope.Organization, role), func(t *testing.T) {
				selected := scope
				selected.Credential.HostedRole, selected.Credential.HostedPrincipal = role, "example"
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: "wi_change_0002", Revision: 1}}
				err := service.readNativeIssueChangePage(t.Context(), selected, &issue)
				if err != nil || issue.Change == nil || issue.Change.ChangeID != "change_"+string(scope.Organization)+"_wi_change_0002" || issue.Change.HeadSHA != strings.Repeat("a", 40) {
					t.Fatalf("projected Change=%+v, error=%v", issue.Change, err)
				}
			})
		}
	}
}
