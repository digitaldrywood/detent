package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeFixture struct {
	service *Service
	project tracker.NativeProject
	base    string
	token   string
}

func newNativeFixture(t *testing.T, service *Service, organization tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	if service == nil {
		service = openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	}
	if organization == "" {
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local = 1").Scan(&organization); err != nil {
			t.Fatal(err)
		}
	}
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "project-" + name, "name": name, "states": states})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "operator-" + name, "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	response = performHubAPIRequest(t, service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": organization, "project_id": project.ID})
	requireNativeStatus(t, response, http.StatusNoContent)
	return nativeFixture{service: service, project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID), token: token.Token}
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
}

func (f nativeFixture) create(t *testing.T, name string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: strings.Repeat("Full issue content. ", 80), State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

func TestNativeIssueMutationConcurrencyAndHistory(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "native")
	issue := f.create(t, "one")
	if !strings.HasPrefix(string(issue.WorkItemID), "wi_") || issue.Revision != 1 || len(issue.Body) < 500 {
		t.Fatalf("issue = %#v", issue)
	}
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	for _, test := range []struct {
		name     string
		key      string
		expected tracker.Revision
		want     int
	}{
		{"edit", "edit-one", 1, http.StatusOK},
		{"retry", "edit-one", 1, http.StatusOK},
		{"stale", "edit-two", 1, http.StatusConflict},
		{"missing revision", "edit-three", 0, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			title := "Edited"
			response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: test.key}, ExpectedRevision: test.expected, Title: &title})
			requireNativeStatus(t, response, test.want)
		})
	}
	var wg sync.WaitGroup
	responses := make(chan int, 12)
	for index := range 12 {
		wg.Go(func() {
			title := fmt.Sprintf("Concurrent %d", index)
			response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("concurrent-%d", index)}, ExpectedRevision: 2, Title: &title})
			responses <- response.Code
		})
	}
	wg.Wait()
	close(responses)
	winners := 0
	for status := range responses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusConflict {
			t.Errorf("concurrent status = %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent edits accepted = %d", winners)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &history)
	if len(history.Items) != 3 {
		t.Fatalf("history count = %d", len(history.Items))
	}
	for index, event := range history.Items {
		if event.AggregateSequence != int64(index+1) || event.SchemaVersion != 1 || event.Actor.PrincipalID == "" {
			t.Errorf("event = %#v", event)
		}
	}
}

func TestNativeCommentsProvenanceAndIdempotency(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "comments")
	issue := f.create(t, "discussion")
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/comments"
	sourceTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	request := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Explicit user comment may include sensitive text: example-secret-value", Provenance: &tracker.Provenance{Provider: "github", ExternalID: "source-comment", AuthorID: "external-author", CreatedAt: sourceTime, UpdatedAt: sourceTime, ObservedAt: sourceTime.Add(time.Hour)}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	if comment.Actor.PrincipalID == "external-author" || comment.Provenance.AuthorID != "external-author" || !comment.CreatedAt.After(sourceTime) {
		t.Fatalf("comment provenance = %#v", comment)
	}
	for range 4 {
		retry := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request)
		requireNativeStatus(t, retry, http.StatusOK)
		if retry.Body.String() != response.Body.String() {
			t.Fatal("retry changed committed response")
		}
	}
	request.Body = "Changed payload"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), http.StatusConflict)
	edit := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-comment"}, ExpectedRevision: 1, Body: "Edited content"}
	response = performHubAPIRequest(t, f.service, http.MethodPatch, path+"/"+comment.ID, f.token, edit)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &comment)
	if comment.Revision != 2 || comment.EditedBy == nil || comment.Provenance.AuthorID != "external-author" {
		t.Fatalf("edited comment = %#v", comment)
	}
	request.IdempotencyKey = "repeat-import"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var importedAgain tracker.NativeComment
	decodeHubResponse(t, response, &importedAgain)
	if importedAgain.ID != comment.ID || importedAgain.Body != "Edited content" {
		t.Fatalf("reimport overwrote native edit: %#v", importedAgain)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/"+comment.ID+"/versions/1", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var original tracker.NativeComment
	decodeHubResponse(t, response, &original)
	if !strings.Contains(original.Body, "example-secret-value") || original.Revision != 1 {
		t.Fatalf("original revision = %#v", original)
	}
	for index := range 3 {
		f.create(t, fmt.Sprintf("another-%d", index))
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var comments tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, response, &comments)
	if len(comments.Items) != 1 || comments.Items[0].Body != "Edited content" {
		t.Fatalf("comments = %#v", comments)
	}
}

func TestNativeImportsAndAdministrationBoundaries(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "imports")
	sourceTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "import"}, Title: "Imported issue", Body: "Complete body", State: "Todo", Provenance: &tracker.Provenance{Provider: "github", ExternalID: "external-issue", AuthorID: "source-author", CreatedAt: sourceTime, UpdatedAt: sourceTime, ObservedAt: sourceTime.Add(time.Hour)}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	request.IdempotencyKey = "reimport"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var repeated tracker.NativeIssue
	decodeHubResponse(t, response, &repeated)
	if repeated.WorkItemID != issue.WorkItemID || len(repeated.ExternalReferences) != 1 || repeated.Actor.PrincipalID == "source-author" {
		t.Fatalf("reimport = %#v", repeated)
	}
	for _, test := range []struct {
		name, path string
		want       int
	}{
		{"issue version", f.base + "/work-items/" + string(issue.WorkItemID) + "/versions/1", http.StatusOK},
		{"invalid version", f.base + "/work-items/" + string(issue.WorkItemID) + "/versions/0", http.StatusUnprocessableEntity},
		{"missing version", f.base + "/work-items/" + string(issue.WorkItemID) + "/versions/2", http.StatusNotFound},
		{"instance administration", "/api/v2/organizations", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, test.path, f.token, nil), test.want)
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "tenant-admin", "scope": "admin"})
	requireNativeStatus(t, response, http.StatusCreated)
	var token tokenResponse
	decodeHubResponse(t, response, &token)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID}), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations", token.Token, nil), http.StatusForbidden)
	worker := f.worker(t, "import-denied-worker")
	request.IdempotencyKey = "worker-import"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", worker, request), http.StatusUnprocessableEntity)
}

func TestNativeDependenciesDoNotLeakThroughCompatibility(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "graph-isolation")
	native := f.create(t, "private-native-title")
	_, legacyID := seedProjection(t, f.service.database.db)
	var nativeID int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", native.WorkItemID).Scan(&nativeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at) VALUES (?, ?, 'native', ?, ?)", legacyID, nativeID, testTimestamp, testTimestamp); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, fmt.Sprintf("/api/v1/work-items/%d", legacyID), testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if strings.Contains(response.Body.String(), "private-native-title") {
		t.Fatal("v1 exposed native graph content")
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/dependencies", legacyID), testHubAdminToken, map[string]any{"blocker_work_item_id": nativeID, "action": "add"})
	requireNativeStatus(t, response, http.StatusNotFound)
}

func TestNativeConcurrentDependencyCycle(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "concurrent-graph")
	a, b := f.create(t, "a"), f.create(t, "b")
	start := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, pair := range [][2]tracker.NativeIssue{{a, b}, {b, a}} {
		wg.Go(func() {
			<-start
			request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: string(pair[0].WorkItemID)}, ExpectedRevision: 1, RelatedWorkItemID: pair[1].WorkItemID, Operation: "add"}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(pair[0].WorkItemID)+"/dependencies", f.token, request)
			results <- response.Code
		})
	}
	close(start)
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusUnprocessableEntity] != 1 {
		t.Fatalf("concurrent graph results = %v", counts)
	}
}

func TestNativeTenantIsolationAndCursorBinding(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "tenant-one")
	one := f.create(t, "one")
	f.create(t, "two")
	otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "same-org")
	other := otherProject.create(t, "other-project-issue")
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other tenant"})
	requireNativeStatus(t, response, http.StatusCreated)
	var organization nativeOrganization
	decodeHubResponse(t, response, &organization)
	otherTenant := newNativeFixture(t, f.service, organization.ID, "tenant-two")
	foreign := otherTenant.create(t, "foreign")
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=1", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.Page[tracker.NativeIssue]
	decodeHubResponse(t, response, &page)
	if page.NextCursor == "" || len(page.Items) != 1 {
		t.Fatalf("page = %#v", page)
	}
	for _, test := range []struct {
		name, path, token string
		want              int
	}{
		{"next page", f.base + "/work-items?limit=1&cursor=" + url.QueryEscape(page.NextCursor), f.token, http.StatusOK},
		{"query changed", f.base + "/work-items?state=Done&cursor=" + url.QueryEscape(page.NextCursor), f.token, http.StatusUnprocessableEntity},
		{"project cursor", otherProject.base + "/work-items?cursor=" + url.QueryEscape(page.NextCursor), otherProject.token, http.StatusUnprocessableEntity},
		{"tenant cursor", otherTenant.base + "/work-items?cursor=" + url.QueryEscape(page.NextCursor), otherTenant.token, http.StatusUnprocessableEntity},
		{"guessed project", otherProject.base + "/work-items/" + string(other.WorkItemID), f.token, http.StatusNotFound},
		{"guessed tenant", otherTenant.base + "/work-items/" + string(foreign.WorkItemID), f.token, http.StatusNotFound},
		{"guessed item", f.base + "/work-items/" + string(foreign.WorkItemID), f.token, http.StatusNotFound},
		{"guessed comments", f.base + "/work-items/" + string(other.WorkItemID) + "/comments", f.token, http.StatusNotFound},
		{"v1 downgrade", "/api/v1/work-items", f.token, http.StatusForbidden},
		{"bad cursor", f.base + "/work-items?cursor=modified.invalid", f.token, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, test.path, test.token, nil), test.want)
		})
	}
	var legacyID int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", one.WorkItemID).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, fmt.Sprintf("/api/v1/work-items/%d", legacyID), testHubAdminToken, nil), http.StatusNotFound)
}

func TestNativeDependencyReadPermissions(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "dependency-visibility")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "private-dependency")
	issue := f.create(t, "visible")
	blocker := other.create(t, "private")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "cross-project"}, ExpectedRevision: 1, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", f.token, request), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", testHubAdminToken, request), http.StatusOK)
	for _, test := range []struct {
		name, suffix string
	}{
		{"detail", ""},
		{"history", "/history"},
		{"version", "/versions/2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, token := range []string{f.token, testHubAdminToken} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+test.suffix, token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if visible := strings.Contains(response.Body.String(), string(blocker.WorkItemID)); visible != (token == testHubAdminToken) {
					t.Fatalf("dependency visibility = %v for administrator %v", visible, token == testHubAdminToken)
				}
			}
		})
	}
}

func TestNativeDependenciesAndTransitions(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "dependencies")
	a := f.create(t, "a")
	b := f.create(t, "b")
	c := f.create(t, "c")
	for _, test := range []struct {
		name    string
		issue   tracker.NativeIssue
		blocker tracker.NativeWorkItemID
		want    int
	}{
		{"a depends b", a, b.WorkItemID, http.StatusOK},
		{"b depends c", b, c.WorkItemID, http.StatusOK},
		{"cycle", c, a.WorkItemID, http.StatusUnprocessableEntity},
		{"self", c, c.WorkItemID, http.StatusUnprocessableEntity},
		{"missing", c, tracker.NativeWorkItemID(newNativeID("wi")), http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: 1, RelatedWorkItemID: test.blocker, Operation: "add"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(test.issue.WorkItemID)+"/dependencies", f.token, request), test.want)
		})
	}
	for _, test := range []struct {
		name, state, reason string
		want                int
	}{
		{"invalid state", "Unknown", "worker_progress", http.StatusUnprocessableEntity},
		{"raw reason", "Done", "raw prompt contents", http.StatusUnprocessableEntity},
		{"valid state", "Done", "worker_progress", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: 1, State: test.state, Reason: test.reason}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(c.WorkItemID)+"/workflow", f.token, request), test.want)
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(c.WorkItemID)+"/history?limit=1", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.Page[tracker.CollaborationEvent]
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("history page = %#v", page)
	}
}
