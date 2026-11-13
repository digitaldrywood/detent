package harness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func CreateProject(t *testing.T, browser *Browser, name, csrf string) string {
	t.Helper()
	response := Request(t, browser, http.MethodPost, "/api/v2/organizations/org_alpha/projects", JSONBody(t, map[string]any{"name": name, "grant_access": true, "idempotency_key": name}), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
	var project struct {
		ID string `json:"project_id"`
	}
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &project) != nil || project.ID == "" {
		t.Fatalf("create project=%d %s", response.Code, response.Body)
	}
	return project.ID
}

func StateOf(t *testing.T, response Page) string {
	t.Helper()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || location.Query().Get("state") == "" {
		t.Fatalf("no provider redirect: %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	return location.Query().Get("state")
}

func AssertEntryFallback(t *testing.T, browser *Browser, body string) {
	t.Helper()
	for _, want := range []string{`class="detent-sign-in detent-entry"`, `href="/static/css/entry.css"`, `class="entry-card"`, `<form method="post" action="/logout">`, `name="csrf"`, `>Sign out</button>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("entry fallback missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `<details`) || strings.Contains(body, `/static/app/`) {
		t.Fatalf("entry fallback depends on workspace chrome or client assets: %s", body)
	}
	if CSRF(t, body) == "" {
		t.Fatal("entry fallback has no logout CSRF token")
	}
	for _, asset := range []string{"/static/css/entry.css", "/static/css/sign-in.css", "/static/fonts/Geist-Variable.woff2"} {
		if response, content := browser.Get(asset); response.StatusCode != http.StatusOK || len(content) == 0 {
			t.Fatalf("entry fallback asset %s = %d", asset, response.StatusCode)
		}
	}
}

func InviteMember(t *testing.T, b *Browser, email, role, csrf string) Page {
	t.Helper()
	response := Request(t, b, http.MethodPost, "/api/v2/organizations/org_alpha/members/invitations", JSONBody(t, map[string]any{"email": email, "role": role, "idempotency_key": "invite-" + email}), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
	return Page{StatusCode: response.Code, Header: response.Header(), Body: response.Body.String()}
}
