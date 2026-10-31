package cloudentry

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSharedEntryReadableSlugMigration(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE organizations(id TEXT PRIMARY KEY,name TEXT NOT NULL); INSERT INTO organizations VALUES('org_activity','Activity'),('org_api','API'),('org_unnamed','!!!')"); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/registry/20261007221000_migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(migration), "-- +goose Down")
	if _, err := db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id,name) VALUES('org_new_activity','Activity'),('org_new_api','API'),('org_new_unnamed','!!!')"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ id, want string }{
		{"org_activity", "activity-organization"},
		{"org_api", "api-organization"},
		{"org_new_activity", "activity-organization-2"},
		{"org_new_api", "api-organization-2"},
		{"org_unnamed", "organization-organization"},
		{"org_new_unnamed", "organization-organization-2"},
	} {
		t.Run(test.id, func(t *testing.T) {
			var slug string
			if err := db.QueryRowContext(t.Context(), "SELECT slug FROM organizations WHERE id=?", test.id).Scan(&slug); err != nil || slug != test.want {
				t.Fatalf("slug=%q want=%q err=%v", slug, test.want, err)
			}
		})
	}
}

func TestSharedEntryReadableIssueURLs(t *testing.T) {
	f := newEntryFixture(t)
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, raw := alice.get("/organizations/org_alpha/app/bootstrap")
	var bootstrap struct {
		CSRF     string `json:"csrf_token"`
		API      string `json:"api_base"`
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(raw), &bootstrap); err != nil || len(bootstrap.Projects) != 1 {
		t.Fatalf("bootstrap=%s err=%v", raw, err)
	}
	send := func(method, path string, input any, csrf string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, testPublicURL+path, jsonBody(t, input))
		origin, err := url.Parse(testPublicURL)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range alice.jar.Cookies(origin) {
			request.AddCookie(cookie)
		}
		request.Header.Set("Origin", testPublicURL)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", csrf)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		return response
	}
	approveEntryFixturePolicy(t, alice, "org_alpha", bootstrap.Projects[0].ID)
	response := send(http.MethodPost, bootstrap.API+"/projects/"+bootstrap.Projects[0].ID+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "readable"}, Title: "Readable issue", Body: issueContractTestSections, State: "Todo"}, bootstrap.CSRF)
	if response.Code != http.StatusOK {
		t.Fatalf("create=%d %s", response.Code, response.Body.String())
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(response.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if issue.WebURL != testPublicURL+"/alpha/alpha-secret-project/issues/1" {
		t.Fatalf("URL=%q", issue.WebURL)
	}
	legacy, _ := alice.get("/organizations/org_alpha/work/i/" + string(issue.WorkItemID))
	if legacy.StatusCode != http.StatusPermanentRedirect || legacy.Header.Get("Location") != issue.WebURL {
		t.Fatalf("legacy=%+v", legacy)
	}
	anonymous := newBrowser(t, f.service.Handler())
	denied, _ := anonymous.get("/alpha/alpha-secret-project/issues/1")
	absent, _ := anonymous.get("/missing/secret-project/issues/1")
	if denied.StatusCode != http.StatusNotFound || absent.StatusCode != http.StatusNotFound || denied.Body != absent.Body {
		t.Fatalf("denied=%+v missing=%+v", denied, absent)
	}
	f.service.config.clientFS = fstest.MapFS{entryClientShell: {Data: []byte(`<html><head></head><body></body></html>`)}}
	for _, test := range []struct{ path, location string }{
		{"/alpha", "/alpha/work"},
		{"/alpha/", "/alpha/work"},
		{"/alpha/?collapsed=Triage", "/alpha/work?collapsed=Triage"},
		{"/organizations/org_alpha/work?collapsed=Triage", "/alpha/work?collapsed=Triage"},
		{"/organizations/org_alpha/settings", "/alpha/settings"},
	} {
		response, _ := alice.get(test.path)
		if response.StatusCode/100 != 3 || response.Header.Get("Location") != test.location {
			t.Fatalf("%s=%+v want %s", test.path, response, test.location)
		}
	}
	html := map[string]string{"Accept": "text/html"}
	for _, test := range []struct{ path, returnPath string }{
		{"/alpha/work", "/alpha/work"},
		{"/alpha", "/alpha"},
		{"/missing/work", "/missing/work"},
		{"/missing", "/missing"},
		{"/alpha/alpha-secret-project/issues/1", "/alpha/alpha-secret-project/issues/1"},
		{"/alpha/logout", "/organizations"},
	} {
		response := anonymous.do(http.MethodGet, test.path, nil, html)
		want := "/auth/oidc/start?" + url.Values{"return": {test.returnPath}}.Encode()
		if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != want {
			t.Fatalf("signed-out navigation %s=%+v want %s", test.path, response, want)
		}
	}
	signedOut := newBrowser(t, f.service.Handler())
	start := signedOut.do(http.MethodGet, "/alpha/alpha-secret-project/issues/1", nil, html).Header.Get("Location")
	if landed := signedOut.login(start, "user_alice:porg_alpha"); landed != "/alpha/alpha-secret-project/issues/1" {
		t.Fatalf("signed-out issue link landed on %q", landed)
	}
	otherOrganization := newBrowser(t, f.service.Handler())
	otherOrganization.login("/organizations/org_beta/organization", "user_alice:porg_beta")
	start = otherOrganization.do(http.MethodGet, "/alpha/alpha-secret-project/issues/1", nil, html).Header.Get("Location")
	if landed := otherOrganization.login(start, "user_alice:porg_alpha"); landed != "/organizations/org_alpha/alpha-secret-project/issues/1" {
		t.Fatalf("issue link from another organization landed on %q", landed)
	}
	if issuePage := otherOrganization.do(http.MethodGet, "/organizations/org_alpha/alpha-secret-project/issues/1", nil, html); issuePage.StatusCode != http.StatusPermanentRedirect || issuePage.Header.Get("Location") != "/alpha/alpha-secret-project/issues/1" {
		t.Fatalf("issue link from another organization=%+v", issuePage)
	}
	if member := alice.do(http.MethodGet, "/alpha/work", nil, html); member.StatusCode == http.StatusSeeOther {
		t.Fatalf("member navigation=%+v", member)
	}
	accountOnly := newBrowser(t, f.service.Handler())
	accountOnly.login("/organizations", "user_alice:")
	nonMember := newBrowser(t, f.service.Handler())
	nonMember.login("/organizations", "user_bob:")
	for _, test := range []struct{ path, returnPath string }{
		{"/alpha", "/organizations/org_alpha/work"},
		{"/alpha/", "/organizations/org_alpha/work"},
		{"/alpha/settings", "/organizations/org_alpha/settings"},
	} {
		response := accountOnly.do(http.MethodGet, test.path, nil, html)
		want := "/auth/oidc/start?" + url.Values{"organization": {"org_alpha"}, "return": {test.returnPath}}.Encode()
		if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != want {
			t.Fatalf("account-only member %s=%+v want %s", test.path, response, want)
		}
		denied := nonMember.do(http.MethodGet, test.path, nil, html)
		absent := nonMember.do(http.MethodGet, "/missing"+strings.TrimPrefix(test.path, "/alpha"), nil, html)
		if denied.StatusCode != http.StatusNotFound || denied.Body != absent.Body || denied.Header.Get("Location") != "" {
			t.Fatalf("non-member %s=%+v missing=%+v", test.path, denied, absent)
		}
	}
	for _, path := range []string{"/alpha", "/alpha/"} {
		denied, _ := anonymous.get(path)
		absent, _ := anonymous.get("/missing" + strings.TrimPrefix(path, "/alpha"))
		if denied.StatusCode != http.StatusNotFound || denied.Body != absent.Body || denied.Header.Get("Location") != "" {
			t.Fatalf("anonymous %s=%+v missing=%+v", path, denied, absent)
		}
	}
	slugPath := "/api/cloud/organizations/org_alpha/slug"
	for _, test := range []struct {
		slug, expected, csrf string
		status               int
	}{
		{"work", "alpha", bootstrap.CSRF, http.StatusBadRequest},
		{"activity", "alpha", bootstrap.CSRF, http.StatusBadRequest},
		{"beta", "alpha", bootstrap.CSRF, http.StatusConflict},
		{"example-organization", "alpha", "bad-csrf", http.StatusForbidden},
		{"example-organization", "alpha", bootstrap.CSRF, http.StatusOK},
		{"second-name", "alpha", bootstrap.CSRF, http.StatusConflict},
		{"second-name", "example-organization", bootstrap.CSRF, http.StatusOK},
	} {
		response := send(http.MethodPut, slugPath, map[string]string{"slug": test.slug, "expected_slug": test.expected}, test.csrf)
		if response.Code != test.status {
			t.Fatalf("slug %s status=%d want=%d body=%s", test.slug, response.Code, test.status, response.Body.String())
		}
	}
	for _, old := range []string{"alpha", "example-organization"} {
		response, _ := alice.get("/" + old + "/alpha-secret-project/issues/1")
		if response.StatusCode != http.StatusPermanentRedirect || response.Header.Get("Location") != "/second-name/alpha-secret-project/issues/1" {
			t.Fatalf("old %s=%+v", old, response)
		}
	}
	response = send(http.MethodGet, bootstrap.API+"/work-items/"+string(issue.WorkItemID), nil, "")
	if err := json.Unmarshal(response.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if issue.WebURL != testPublicURL+"/second-name/alpha-secret-project/issues/1" {
		t.Fatalf("edited URL=%q", issue.WebURL)
	}
	for _, state := range []string{"requested", "allocating", "failed", "disabled", "deleting", "deleted"} {
		t.Run(state, func(t *testing.T) {
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state=?,managed=1 WHERE id=?", state, "org_alpha"); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				missing := anonymous.do(method, "/missing/secret-project/issues/1", nil, nil)
				for _, visitor := range []struct {
					name    string
					browser *browser
				}{{"anonymous", anonymous}, {"member", alice}} {
					response := visitor.browser.do(method, "/second-name/alpha-secret-project/issues/1", nil, nil)
					if response.StatusCode != http.StatusNotFound || response.Body != missing.Body || response.Header.Get("Location") != "" || response.Header.Get("Content-Type") != missing.Header.Get("Content-Type") {
						t.Errorf("%s %s response=%+v missing=%+v", visitor.name, method, response, missing)
					}
				}
			}
		})
	}
}
