package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
	states := nativeFixtureStates()
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
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: strings.Repeat("Full issue content. ", 80) + "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks.", State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

func TestNativeIssueWebURL(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		prefix string
	}{
		{"local", ""},
		{"hosted", "http://127.0.0.1:7777/work/i/"},
		{"shared", "https://hub.example.test/organizations/org_security/work/i/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var base string
			var request func(string, string, any) *httptest.ResponseRecorder
			switch test.name {
			case "local":
				f := newDefaultNativeFixture(t, Config{})
				base = f.base
				request = func(method, path string, body any) *httptest.ResponseRecorder {
					return performHubAPIRequest(t, f.service, method, "https://untrusted.example"+path, f.token, body)
				}
			case "hosted":
				f := newHostedSecurityFixture(t)
				owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
				base = f.base
				request = func(method, path string, body any) *httptest.ResponseRecorder {
					return f.request(t, owner, method, "https://untrusted.example"+path, body)
				}
			case "shared":
				f := newHostedSharedFixture(t)
				owner := f.member(t, "owner", "owner", "write")
				base = f.base
				request = func(method, path string, body any) *httptest.ResponseRecorder {
					var encoded []byte
					if body != nil {
						var err error
						encoded, err = json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
					}
					return f.serve(t, hostedSharedRequest{user: &owner, method: method, target: "https://untrusted.example" + path, body: string(encoded), csrf: cloudassert.CSRFToken("shared-user_owner", "org_security"), headers: map[string]string{
						"X-Forwarded-Host": "forwarded.example", "X-Forwarded-Proto": "http", "Forwarded": "host=forwarded.example;proto=http",
					}})
				}
			}
			assertURL := func(t *testing.T, body []byte) tracker.NativeIssue {
				t.Helper()
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				if _, ok := fields["web_url"]; !ok {
					t.Fatal("work item response is missing web_url")
				}
				var issue tracker.NativeIssue
				if err := json.Unmarshal(body, &issue); err != nil {
					t.Fatal(err)
				}
				want := ""
				if test.prefix != "" {
					want = test.prefix + string(issue.WorkItemID)
				}
				if issue.WebURL != want {
					t.Fatalf("web_url = %q, want %q", issue.WebURL, want)
				}
				return issue
			}
			create := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-url"}, Title: "Linked work item", State: "Todo"}
			response := request(http.MethodPost, base+"/work-items", create)
			requireNativeStatus(t, response, http.StatusOK)
			issue := assertURL(t, response.Body.Bytes())
			path := base + "/work-items/" + string(issue.WorkItemID)
			title := "Edited work item"
			update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "update-url"}, ExpectedRevision: issue.Revision, Title: &title}
			for _, endpoint := range []struct {
				name, method, path string
				body               any
				list               bool
			}{
				{"create retry", http.MethodPost, base + "/work-items", create, false},
				{"get", http.MethodGet, path, nil, false},
				{"list", http.MethodGet, base + "/work-items", nil, true},
				{"update", http.MethodPatch, path, update, false},
				{"update retry", http.MethodPatch, path, update, false},
				{"version", http.MethodGet, path + "/versions/1", nil, false},
			} {
				t.Run(endpoint.name, func(t *testing.T) {
					response := request(endpoint.method, endpoint.path, endpoint.body)
					requireNativeStatus(t, response, http.StatusOK)
					if endpoint.list {
						var page tracker.Page[json.RawMessage]
						decodeHubResponse(t, response, &page)
						if len(page.Items) != 1 {
							t.Fatalf("list contains %d items, want 1", len(page.Items))
						}
						assertURL(t, page.Items[0])
					} else {
						got := assertURL(t, response.Body.Bytes())
						if got.WorkItemID != issue.WorkItemID {
							t.Fatalf("work_item_id = %q, want %q", got.WorkItemID, issue.WorkItemID)
						}
					}
				})
			}
		})
	}
}

func TestNativeIssueOrganizationLookup(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "granted")
	granted := f.create(t, "readable")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "ungranted")
	ungranted := other.create(t, "private")
	for _, test := range []struct {
		name         string
		organization tracker.OrganizationID
		item         tracker.NativeWorkItemID
		want         int
	}{
		{"granted project", f.project.OrganizationID, granted.WorkItemID, http.StatusOK},
		{"ungranted project", f.project.OrganizationID, ungranted.WorkItemID, http.StatusNotFound},
		{"unknown ID", f.project.OrganizationID, "wi_unknown", http.StatusNotFound},
		{"other organization", "org_other", granted.WorkItemID, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/api/v2/organizations/" + string(test.organization) + "/work-items/" + string(test.item)
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, test.want)
			if test.want != http.StatusOK {
				requireNativeCode(t, response, http.StatusNotFound, "not_found")
				return
			}
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			if issue.WorkItemID != granted.WorkItemID || issue.ProjectID != f.project.ID || issue.Title != granted.Title {
				t.Fatalf("issue = %#v", issue)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("issue lookup must not be cached")
			}
		})
	}
}

func TestNativeWorkflowRefusalCode(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		name := "local"
		if hosted {
			name = "hosted"
		}
		t.Run(name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "workflow-refusal")
			response := performHubAPIRequest(t, f.service, http.MethodPost,
				f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token,
				tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "disallowed-move"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "user_requested"})
			failure := requireNativeCode(t, response, http.StatusUnprocessableEntity, "transition_not_allowed")
			if failure.Message != "Workflow transition is not allowed" {
				t.Fatalf("message = %q", failure.Message)
			}
			if got := readWorkItem(t, f, issue.WorkItemID, ""); got.State != issue.State || got.Revision != issue.Revision {
				t.Fatalf("issue changed after refused move: %#v", got)
			}
			if !hosted {
				return
			}
			recorded := httptest.NewRecorder()
			context := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/workflow", nil), recorded)
			service := &Service{config: Config{Hosted: &HostedConfig{}}}
			if err := service.nativeAPIError(context, &nativeError{Code: failure.Code, Message: failure.Message, status: http.StatusUnprocessableEntity}); err != nil {
				t.Fatal(err)
			}
			redacted := requireNativeCode(t, recorded, http.StatusUnprocessableEntity, "transition_not_allowed")
			if redacted.Message != "The requested operation is unavailable" {
				t.Fatalf("hosted message = %q", redacted.Message)
			}
		})
	}
}

func TestNativeIssueMutationConcurrencyAndHistory(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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
	t.Run("activity", func(t *testing.T) {
		f := newPullRequestFixture(t, true)
		path := f.base + "/work-items/" + string(f.issue.WorkItemID)
		linked := f.create(t, "activity-linked")
		unrelated := f.create(t, "activity-unrelated")
		worker := f.worker(t, "activity-worker")
		lease := claimNativeAttempt(t, f.nativeFixture, worker, "activity-machine", "activity-session", f.issue.WorkItemID)
		start := nativeStartedEvent(lease)
		var comment, workpad tracker.NativeComment
		var version tracker.ChangeVersion
		changePath := f.changeFixture.path
		if !f.issue.LastActivityAt.Equal(f.now) {
			t.Fatalf("creation activity = %s, want %s", f.issue.LastActivityAt, f.now)
		}
		for index, test := range []struct {
			name        string
			editsIssue  bool
			linkedEvent bool
			apply       func(*testing.T, tracker.NativeIssue)
		}{
			{"issue edit", true, false, func(t *testing.T, issue tracker.NativeIssue) {
				title := "Activity edit"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, ExpectedRevision: issue.Revision, Title: &title}), http.StatusOK)
			}},
			{"lane move", true, false, func(t *testing.T, issue tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, ExpectedRevision: issue.Revision, State: "In Progress", Reason: "user_requested"}), http.StatusOK)
			}},
			{"comment", false, false, func(t *testing.T, _ tracker.NativeIssue) {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, Body: "Activity comment"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &comment)
			}},
			{"comment edit", false, false, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, ExpectedRevision: comment.Revision, Body: "Edited activity comment"}), http.StatusOK)
			}},
			{"Workpad creation", false, false, func(t *testing.T, _ tracker.NativeIssue) {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, Body: "## Codex Workpad\nPlan: implement activity"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &workpad)
			}},
			{"Workpad update", false, false, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+workpad.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, ExpectedRevision: workpad.Revision, Body: "## Codex Workpad\nValidation: activity verified"}), http.StatusOK)
			}},
			{"run start", false, false, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
			}},
			{"run finish", false, false, func(t *testing.T, _ tracker.NativeIssue) {
				finish := start
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "activity-finish", 2, "succeeded"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
			}},
			{"change creation", false, true, func(t *testing.T, _ tracker.NativeIssue) {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, Title: "Activity change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				changePath = path + "/changes/" + change.ID
			}},
			{"change version", false, true, func(t *testing.T, _ tracker.NativeIssue) {
				input := changeTestInput()
				input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: pullRequestTestURL}
				response := performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
			}},
			{"change discussion", false, true, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/discussion", f.token, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, Body: "Activity discussion"}), http.StatusOK)
			}},
			{"change review", false, true, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, Decision: "approved"}), http.StatusOK)
			}},
			{"change check", false, true, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
			}},
			{"PR event", false, true, func(t *testing.T, _ tracker.NativeIssue) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET issue_id = NULL, updated_at = ?, synchronized_at = ? WHERE repository_id = ?", formatHubTime(f.now), formatHubTime(f.now), f.repositoryID); err != nil {
					t.Fatal(err)
				}
			}},
			{"change landing", true, true, func(t *testing.T, _ tracker.NativeIssue) {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions/"+version.ID+"/landing", f.token, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: t.Name()}, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}), http.StatusOK)
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				before := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
				f.now = f.now.Add(time.Duration(index+1) * 10 * time.Millisecond)
				test.apply(t, before)
				after := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
				wantUpdated := before.UpdatedAt
				if test.editsIssue {
					wantUpdated = f.now
				}
				if !after.LastActivityAt.Equal(f.now) || !after.UpdatedAt.Equal(wantUpdated) {
					t.Fatalf("activity = %s, updated = %s; want %s, %s", after.LastActivityAt, after.UpdatedAt, f.now, wantUpdated)
				}
				linkedAfter := readWorkItem(t, f.nativeFixture, linked.WorkItemID, "")
				if test.linkedEvent && (!linkedAfter.LastActivityAt.Equal(f.now) || !linkedAfter.UpdatedAt.Equal(linked.UpdatedAt)) {
					t.Fatalf("linked issue activity = %s, updated = %s", linkedAfter.LastActivityAt, linkedAfter.UpdatedAt)
				}
				if got := readWorkItem(t, f.nativeFixture, unrelated.WorkItemID, ""); !got.LastActivityAt.Equal(unrelated.LastActivityAt) {
					t.Fatalf("unrelated activity moved to %s", got.LastActivityAt)
				}
				var page tracker.NativeIssuePage
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work", f.token, nil), &page)
				found := false
				for _, item := range page.Items {
					if item.WorkItemID == after.WorkItemID {
						found = true
						if !item.LastActivityAt.Equal(after.LastActivityAt) {
							t.Fatalf("list activity = %s, item activity = %s", item.LastActivityAt, after.LastActivityAt)
						}
					}
				}
				if !found || page.Work == nil {
					t.Fatal("list omitted the issue or work projection")
				}
				for _, item := range page.Work.Items {
					if item.WorkItemID == after.WorkItemID && !item.LastActivityAt.Equal(after.LastActivityAt) {
						t.Fatalf("compact activity = %s, item activity = %s", item.LastActivityAt, after.LastActivityAt)
					}
				}
				for _, item := range []operatortool.NativeItem{operatortool.NativeItemView(string(f.project.ID), after), operatortool.NativeItemPage(string(f.project.ID), tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{after}}).Items[0]} {
					raw, err := json.Marshal(item)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil || string(fields["last_activity_at"]) != strconv.Quote(formatHubTime(f.now)) {
						t.Fatalf("operator activity = %s, error = %v", fields["last_activity_at"], err)
					}
				}
			})
		}
		latest := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "").LastActivityAt
		for _, stamp := range []time.Time{latest.Add(-time.Nanosecond), latest.Truncate(time.Second), latest.Add(-time.Hour), latest} {
			before := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
			f.now = stamp
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "older-" + formatHubTime(stamp)}, Body: "Older activity"}), http.StatusOK)
			after := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
			if !after.LastActivityAt.Equal(before.LastActivityAt) || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("older event changed activity %s -> %s or updated %s -> %s", before.LastActivityAt, after.LastActivityAt, before.UpdatedAt, after.UpdatedAt)
			}
			title := "Older edit"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "older-edit-" + formatHubTime(stamp)}, ExpectedRevision: after.Revision, Title: &title}), http.StatusOK)
			edited := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
			if !edited.LastActivityAt.Equal(latest) || !edited.UpdatedAt.Equal(stamp) {
				t.Fatalf("older edit activity = %s, updated = %s; want %s, %s", edited.LastActivityAt, edited.UpdatedAt, latest, stamp)
			}
		}
	})
}

func TestNativeCommentsProvenanceAndIdempotency(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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
	f := newDefaultNativeFixture(t, Config{})
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
	f := newDefaultNativeFixture(t, Config{})
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
	f := newDefaultNativeFixture(t, Config{})
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
	f := newDefaultNativeFixture(t, Config{})
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
		{"malformed list query", f.base + "/work-items?label=bug%ZZ", f.token, http.StatusUnprocessableEntity},
		{"malformed comment query", f.base + "/work-items/" + string(one.WorkItemID) + "/comments?cursor=%ZZ", f.token, http.StatusUnprocessableEntity},
		{"malformed history query", f.base + "/work-items/" + string(one.WorkItemID) + "/history?cursor=%ZZ", f.token, http.StatusUnprocessableEntity},
		{"malformed attempts query", f.base + "/work-items/" + string(one.WorkItemID) + "/attempts?cursor=%ZZ", f.token, http.StatusUnprocessableEntity},
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
	f := newDefaultNativeFixture(t, Config{})
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
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	reference := fmt.Sprintf("%s#%d", other.project.ID, blocker.Number)
	if _, err := resolveNativeBlockerReference(t.Context(), f.service.database.db, scope, reference); err == nil {
		t.Fatal("recorded predicate resolved without its project grant")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+credential.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": other.project.ID}), http.StatusNoContent)
	resolved, err := resolveNativeBlockerReference(t.Context(), f.service.database.db, scope, reference)
	if err != nil || resolved.WorkItemID != blocker.WorkItemID || resolved.Terminal {
		t.Fatalf("granted native predicate resolution = %+v, %v", resolved, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id = ? AND project_id = ?", credential.ID, other.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveNativeBlockerReference(t.Context(), f.service.database.db, scope, reference); err == nil {
		t.Fatal("recorded predicate retained removed project authority")
	}
}

func TestNativeDependenciesAndTransitions(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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
		name, state, reason, detail string
		want                        int
	}{
		{"invalid state", "Unknown", "worker_progress", "", http.StatusUnprocessableEntity},
		{"raw reason", "Done", "raw prompt contents", "", http.StatusUnprocessableEntity},
		{"oversized reason detail", "Done", "worker_progress", strings.Repeat("x", 8<<10+1), http.StatusUnprocessableEntity},
		{"valid state", "Done", "worker_progress", "The worker completed the requested work", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: 1, State: test.state, Reason: test.reason, ReasonDetail: test.detail}
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
