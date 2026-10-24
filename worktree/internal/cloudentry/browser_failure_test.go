package cloudentry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestBrowserFailure(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	for _, test := range []struct {
		name, method, path, mode, dest, accept, background string
		failure, status                                    int
		body                                               string
	}{
		{name: "document navigation", mode: "navigate", dest: "document", status: http.StatusSeeOther},
		{name: "head navigation", method: http.MethodHead, mode: "navigate", dest: "document", status: http.StatusSeeOther},
		{name: "event stream", path: "/organizations/org_alpha/projects/prj_example/events", mode: "cors", dest: "empty", accept: "text/event-stream", status: http.StatusUnauthorized},
		{name: "fetch", mode: "cors", dest: "empty", status: http.StatusUnauthorized},
		{name: "HTML fetch", mode: "same-origin", dest: "empty", accept: "text/html", status: http.StatusUnauthorized},
		{name: "missing fetch metadata", accept: "text/html", status: http.StatusUnauthorized},
		{name: "missing destination", mode: "navigate", status: http.StatusUnauthorized},
		{name: "missing mode", dest: "document", status: http.StatusUnauthorized},
		{name: "iframe", mode: "navigate", dest: "iframe", status: http.StatusUnauthorized},
		{name: "HTMX", mode: "navigate", dest: "document", background: "HX-Request", status: http.StatusUnauthorized},
		{name: "XHR", mode: "navigate", dest: "document", background: "X-Requested-With", status: http.StatusUnauthorized},
		{name: "event stream with navigation metadata", mode: "navigate", dest: "document", accept: "text/event-stream", status: http.StatusUnauthorized},
		{name: "JSON", mode: "navigate", dest: "document", accept: "application/json", status: http.StatusUnauthorized, body: `"code":"unauthorized"`},
		{name: "API navigation", path: "/api/v2/organizations/org_alpha/projects", mode: "navigate", dest: "document", status: http.StatusUnauthorized, body: `"code":"unauthorized"`},
		{name: "mutation", method: http.MethodPost, mode: "navigate", dest: "document", status: http.StatusUnauthorized},
		{name: "unavailable document", mode: "navigate", dest: "document", failure: http.StatusServiceUnavailable, status: http.StatusServiceUnavailable, body: "Temporarily unavailable"},
		{name: "unavailable API", path: "/api/cloud/session", failure: http.StatusServiceUnavailable, status: http.StatusServiceUnavailable, body: `"code":"unavailable"`},
		{name: "unavailable JSON", accept: "application/json", failure: http.StatusServiceUnavailable, status: http.StatusServiceUnavailable, body: `"code":"unavailable"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			method, path := test.method, test.path
			if method == "" {
				method = http.MethodGet
			}
			if path == "" {
				path = "/organizations/org_alpha/work"
			}
			request := httptest.NewRequest(method, testPublicURL+path, nil)
			request.Header.Set("Sec-Fetch-Mode", test.mode)
			request.Header.Set("Sec-Fetch-Dest", test.dest)
			request.Header.Set("Accept", test.accept)
			if test.background != "" {
				request.Header.Set(test.background, "true")
			}
			recorder := httptest.NewRecorder()
			if test.failure == http.StatusServiceUnavailable {
				c := echo.New().NewContext(request, recorder)
				c.SetParamNames("organization")
				c.SetParamValues("org_alpha")
				if err := f.service.browserFailure(c, "org_alpha", test.failure); err != nil {
					t.Fatal(err)
				}
			} else {
				f.service.Handler().ServeHTTP(recorder, request)
			}
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			location := recorder.Header().Get("Location")
			if test.status == http.StatusSeeOther {
				if location != "/auth/oidc/start?organization=org_alpha&return=%2Forganizations%2Forg_alpha%2Fwork" {
					t.Fatalf("location = %q", location)
				}
			} else if location != "" {
				t.Fatalf("background request redirected to %q", location)
			}
			if cookies := recorder.Header().Values("Set-Cookie"); len(cookies) != 0 {
				t.Fatalf("failure created cookies: %v", cookies)
			}
			if test.body != "" && !strings.Contains(recorder.Body.String(), test.body) {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), test.body)
			}
			var transactions int
			if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM transactions").Scan(&transactions); err != nil {
				t.Fatal(err)
			}
			if transactions != 0 {
				t.Fatalf("failure created %d login transactions", transactions)
			}
		})
	}
}
