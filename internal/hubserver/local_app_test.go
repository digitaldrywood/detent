package hubserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalAppAuthentication(t *testing.T) {
	f := newNativeFixture(t, newLocalHTTPService(t, withAppClientFS(appClientBundle())), "", "Example project")
	second := newNativeFixture(t, f.service, f.project.OrganizationID, "Second project")
	workerToken := hubapi.TokenResponse{Token: f.worker(t, "example-worker")}
	for _, test := range []struct {
		name, method, path, token, origin, site string
		bearer                                  bool
		status                                  int
	}{
		{name: "anonymous board", path: "/work", status: 401},
		{name: "anonymous native API challenges browser", path: f.base, status: 401},
		{name: "invalid token", path: "/work", token: "invalid", status: 401},
		{name: "worker denied", path: "/work", token: workerToken.Token, status: 403},
		{name: "scoped operator denied", path: "/work", token: f.token, status: 403},
		{name: "authenticated board", path: "/work", token: testHubAdminToken, status: 200},
		{name: "settings", path: "/settings/integration", token: testHubAdminToken, status: 200},
		{name: "fleet", path: "/fleet", token: testHubAdminToken, status: 200},
		{name: "bearer board", path: "/work", token: testHubAdminToken, bearer: true, status: 200},
		{name: "anonymous bootstrap", path: "/app/bootstrap", status: 401},
		{name: "anonymous chat bootstrap", path: "/chat/bootstrap", status: 401},
		{name: "saved views", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/work-views", token: testHubAdminToken, status: 200},
		{name: "fleet names", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet?include=names", token: testHubAdminToken, status: 200},
		{name: "fleet data", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/fleet", token: testHubAdminToken, status: 200},
		{name: "work preference", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/work-view-preference", token: testHubAdminToken, status: 200},
		{name: "project list", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/projects", token: testHubAdminToken, status: 200},
		{name: "foreign organization", path: "/api/v2/organizations/org_other/work-views", token: testHubAdminToken, status: 404},
		{name: "organization name", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/name", token: testHubAdminToken, status: 200},
		{name: "model selection", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/model-selection", token: testHubAdminToken, status: 200},
		{name: "monthly budget", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/monthly-budget", token: testHubAdminToken, status: 200},
		{name: "project rank", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/project-rank", token: testHubAdminToken, status: 200},
		{name: "Slack integration", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/integrations/slack", token: testHubAdminToken, status: 200},
		{name: "release notes", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/release-notes", token: testHubAdminToken, status: 200},
		{name: "anonymous project rank", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/project-rank", status: 401},
		{name: "local members", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/members", token: testHubAdminToken, status: 200},
		{name: "sprite pool", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/sprite-pool", token: testHubAdminToken, status: 200},
		{name: "secret metadata", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/secrets/fly_sprites_token", token: testHubAdminToken, status: 200},
		{name: "headless operator API", path: f.base, token: f.token, bearer: true, status: 200},
		{name: "updates", path: "/app/updates", token: testHubAdminToken, status: 200},
		{name: "bootstrap", path: "/app/bootstrap", token: testHubAdminToken, status: 200},
		{name: "chat bootstrap", path: "/chat/bootstrap", token: testHubAdminToken, status: 200},
		{name: "native project", path: f.base, token: testHubAdminToken, status: 200},
		{name: "cross site read", path: f.base, token: testHubAdminToken, site: "cross-site", status: 403},
		{name: "cross origin write", method: "PUT", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/name", token: testHubAdminToken, origin: "https://other.example", status: 403},
		{name: "missing origin write", method: "PUT", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/name", token: testHubAdminToken, status: 403},
		{name: "same origin write reaches handler", method: "PUT", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/name", token: testHubAdminToken, origin: "http://example.com", status: 422},
		{name: "bearer write unchanged", method: "PUT", path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/name", token: testHubAdminToken, bearer: true, status: 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := test.method
			if method == "" {
				method = http.MethodGet
			}
			request := httptest.NewRequest(method, test.path, nil)
			if test.token != "" {
				if test.bearer {
					request.Header.Set("Authorization", "Bearer "+test.token)
				} else {
					request.SetBasicAuth("operator", test.token)
				}
			}
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", test.site)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
			if test.status == 401 && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing browser authentication challenge")
			}
			if test.name == "bootstrap" {
				var payload appBootstrap
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Organization.ID != string(f.project.OrganizationID) || len(payload.Projects) != 2 || payload.CSRFToken == "" || !payload.SelfHosted {
					t.Fatalf("bootstrap=%+v", payload)
				}
				for _, project := range []string{string(f.project.ID), string(second.project.ID)} {
					found := false
					for _, entry := range payload.Projects {
						if entry.ID == project {
							found = entry.CanWrite && entry.CanManageRunners
						}
					}
					if !found {
						t.Fatalf("project %s missing or unusable", project)
					}
				}
			}
		})
	}
	t.Run("independent local preferences", func(t *testing.T) {
		other, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "second administrator", Scope: hubapi.APIScopeAdmin})
		if err != nil {
			t.Fatal(err)
		}
		base := "/api/v2/organizations/" + string(f.project.OrganizationID)
		checked := map[string]bool{}
		request := func(token, method, path, body string, want ...int) *httptest.ResponseRecorder {
			t.Helper()
			if key := method + " " + strings.Split(path, "?")[0]; !checked[key] {
				anonymous := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(anonymous, httptest.NewRequest(method, path, strings.NewReader(body)))
				requireNativeStatus(t, anonymous, http.StatusUnauthorized)
				checked[key] = true
			}
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.SetBasicAuth("operator", token)
			req.Header.Set("Origin", "http://example.com")
			rec := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(rec, req)
			status := http.StatusOK
			if len(want) > 0 {
				status = want[0]
			}
			requireNativeStatus(t, rec, status)
			return rec
		}
		f.service.releaseNotes = testReleaseBuilds()
		f.service.config.Version = "1.2.1"
		for _, token := range []string{testHubAdminToken, other.Token} {
			var notes releaseNotesPage
			decodeHubResponse(t, request(token, http.MethodGet, base+"/release-notes?limit=1", ""), &notes)
			if notes.HasUnread || len(notes.Builds) != 1 || notes.Builds[0].Unread || notes.NextCursor == "" {
				t.Fatalf("new local release baseline=%+v", notes)
			}
			var older releaseNotesPage
			decodeHubResponse(t, request(token, http.MethodGet, base+"/release-notes?limit=2&cursor="+notes.NextCursor, ""), &older)
			if older.HasUnread || len(older.Builds) != 2 || older.Builds[0].Version != "v1.2.0-op.r.1.g123456789abc" || older.NextCursor != "" {
				t.Fatalf("local release pagination=%+v", older)
			}
		}
		for _, token := range []string{testHubAdminToken, other.Token} {
			request(token, http.MethodPut, base+"/work-view-preference", `{"query":"view=list"}`)
			view := savedWorkView{Name: "Example view", Scope: "personal", ProjectID: new(string(f.project.ID)), ProjectIDs: []string{string(f.project.ID)}, Favorite: true, State: savedWorkState{View: "board", Tab: "active", Sort: "updated", CompletedWindow: "24h"}}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			request(token, http.MethodPost, base+"/work-views", string(encoded))
			setup, err := json.Marshal(view.State)
			if err != nil {
				t.Fatal(err)
			}
			var board []workBoardProject
			decodeHubResponse(t, request(token, http.MethodGet, base+"/work-board?project="+url.QueryEscape(string(f.project.ID))+"&state="+url.QueryEscape(string(setup)), ""), &board)
			if len(board) != 1 || board[0].Project.ID != f.project.ID {
				t.Fatalf("board=%+v", board)
			}
		}
		project := request(testHubAdminToken, http.MethodPost, base+"/projects", `{"name":"Browser project","grant_access":true,"idempotency_key":"browser-project"}`)
		var created tracker.NativeProject
		decodeHubResponse(t, project, &created)
		if created.Name != "Browser project" || len(created.States) == 0 {
			t.Fatalf("created project=%+v", created)
		}
		f.service.releaseNotes = testReleaseBuilds()[2:]
		f.service.config.Version = "1.2.0"
		for _, token := range []string{testHubAdminToken, other.Token} {
			request(token, http.MethodPut, base+"/release-notes/seen", `{}`)
		}
		f.service.releaseNotes = testReleaseBuilds()
		f.service.config.Version = "1.2.1"
		request(testHubAdminToken, http.MethodPut, base+"/release-notes/seen", `{}`)
		for _, test := range []struct {
			token  string
			unread bool
		}{{testHubAdminToken, false}, {other.Token, true}} {
			var notes releaseNotesPage
			decodeHubResponse(t, request(test.token, http.MethodGet, base+"/release-notes", ""), &notes)
			if notes.HasUnread != test.unread || len(notes.Builds) != 3 {
				t.Fatalf("release notes=%+v", notes)
			}
		}
		f.service.config.Version = "1.2.0"
		var rollback releaseNotesPage
		decodeHubResponse(t, request(testHubAdminToken, http.MethodPut, base+"/release-notes/seen", `{}`), &rollback)
		if rollback.HasUnread || rollback.Builds[0].Unread || len(rollback.Builds) != 3 {
			t.Fatalf("local rollback moved preference backward=%+v", rollback)
		}
		var rank organizationProjectRank
		decodeHubResponse(t, request(testHubAdminToken, http.MethodGet, base+"/project-rank", ""), &rank)
		ids := []tracker.ProjectID{created.ID, second.project.ID, f.project.ID}
		encoded, err := json.Marshal(projectRankChange{ExpectedRevision: rank.Revision, ProjectIDs: ids})
		if err != nil {
			t.Fatal(err)
		}
		request(testHubAdminToken, http.MethodPut, base+"/project-rank", string(encoded))
		decodeHubResponse(t, request(testHubAdminToken, http.MethodGet, base+"/project-rank", ""), &rank)
		if !slices.Equal(rank.ProjectIDs, ids) {
			t.Fatalf("project rank=%+v", rank)
		}
		request(testHubAdminToken, http.MethodPut, base+"/integrations/slack", `{"webhook":"","channel_name":"example-channel"}`)
		var slack slackIntegrationStatus
		decodeHubResponse(t, request(testHubAdminToken, http.MethodGet, base+"/integrations/slack", ""), &slack)
		if slack.ChannelName != "example-channel" || slack.Webhook != "" {
			t.Fatalf("Slack integration=%+v", slack)
		}
		owners := map[string]bool{}
		for _, token := range []string{testHubAdminToken, other.Token} {
			var preference workViewPreference
			decodeHubResponse(t, request(token, http.MethodGet, base+"/work-view-preference", ""), &preference)
			if preference.Query == nil || *preference.Query != "view=list" {
				t.Fatalf("preference=%+v", preference)
			}
			var views savedWorkViewsPage
			decodeHubResponse(t, request(token, http.MethodGet, base+"/work-views", ""), &views)
			if len(views.Views) != 1 || !views.Views[0].Favorite || !views.Views[0].CanEdit {
				t.Fatalf("views=%+v", views)
			}
			owners[views.Views[0].OwnerID] = true
		}
		if len(owners) != 2 {
			t.Fatalf("preferences share an owner: %v", owners)
		}
		for _, identity := range []struct{ id, token string }{{bootstrapTokenID, testHubAdminToken}, {other.ID, other.Token}} {
			var before, after accessstore.Member
			if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
				var err error
				before, err = r.AccessMember(t.Context(), "local_"+identity.id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if before.PrincipalID != identity.id || before.MembershipID != "local_"+identity.id || before.Role != "admin" || !before.Active || !before.NotifyIssues || before.OrganizationID != f.project.OrganizationID {
				t.Fatalf("local administrator membership = %+v", before)
			}
			request(identity.token, http.MethodPut, base+"/work-view-preference", `{"query":"view=list"}`)
			if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
				var err error
				after, err = r.AccessMember(t.Context(), "local_"+identity.id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("repeated local write changed membership: before=%+v after=%+v", before, after)
			}
		}
		f.service.config.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			return spritesTestResponse(`{"sprites":[]}`, http.StatusOK), nil
		})}

		for _, test := range []struct{ method, path, body string }{
			{http.MethodPut, base + "/name", `{"name":"Example organization","idempotency_key":"rename"}`},
			{http.MethodGet, base + "/slug", ""},
			{http.MethodPut, base + "/slug", `{"expected_slug":"local-organization","slug":"example-organization"}`},
			{http.MethodGet, f.base + "/slug", ""},
			{http.MethodPut, f.base + "/slug", `{"expected_slug":"` + f.project.Slug + `","slug":"example-project"}`},
			{http.MethodPut, base + "/secrets/fly_sprites_token", `{"token":"example-org/org-id/token-id/EXAMPLE","idempotency_key":"secret"}`},
			{http.MethodDelete, base + "/secrets/fly_sprites_token", ""},
			{http.MethodPut, base + "/model-selection", `{"idempotency_key":"models","expected_revision":"0","selection":{"normal_model":"example-model"}}`},
			{http.MethodPut, base + "/monthly-budget", `{"idempotency_key":"budget","expected_revision":"0","policy":{"mode":"drain_issue","total_micros":1000000}}`},
			{http.MethodGet, f.base + "/work-view-preference", ""},
			{http.MethodPut, f.base + "/work-view-preference", `{"query":"view=list"}`},
			{http.MethodDelete, f.base + "/work-view-preference", ""},
			{http.MethodDelete, base + "/work-view-preference", ""},
		} {
			t.Run(test.method+" "+test.path, func(t *testing.T) {
				request(testHubAdminToken, test.method, test.path, test.body)
			})
		}
		issue := f.create(t, "Example readable issue")
		for _, path := range []string{base + "/readable/example-project", base + "/readable/example-project/issues/" + strconv.Itoa(issue.Number)} {
			request(testHubAdminToken, http.MethodGet, path, "")
		}

		for _, token := range []string{testHubAdminToken, other.Token} {
			var views savedWorkViewsPage
			decodeHubResponse(t, request(token, http.MethodGet, base+"/work-views", ""), &views)
			view := views.Views[0]
			path := base + "/work-views/" + view.ID
			body, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct{ method, path, body string }{
				{http.MethodGet, path, ""},
				{http.MethodPut, path, string(body)},
				{http.MethodPut, path + "/favorite", `{"favorite":false}`},
			} {
				request(token, test.method, test.path, test.body)
			}
			request(token, http.MethodDelete, path, "", http.StatusNoContent)
		}

	})
	t.Run("project event stream", func(t *testing.T) {
		path := "/projects/" + string(f.project.ID) + "/events"
		anonymous := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, path, nil))
		requireNativeStatus(t, anonymous, http.StatusUnauthorized)
		ctx, cancel := context.WithCancel(t.Context())
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		request.SetBasicAuth("operator", testHubAdminToken)
		input, output := io.Pipe()
		done := make(chan struct{})
		headers := make(chan *http.Response, 1)
		t.Cleanup(func() {
			cancel()
			input.Close()
			<-done
		})
		go func() {
			defer close(done)
			defer output.Close()
			f.service.Handler().ServeHTTP(hostedStreamWriter{ResponseRecorder: httptest.NewRecorder(), output: output, headers: headers}, request)
		}()
		select {
		case response := <-headers:
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("stream response=%d type=%s", response.StatusCode, response.Header.Get("Content-Type"))
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})

}
