package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

func requestWithCookie(name, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if name != "" {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	return r
}

func TestDashboardTheme(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		value  string
		want   string
	}{
		{name: "no cookie", want: ""},
		{name: "light", cookie: themeCookieName, value: "light", want: "light"},
		{name: "dark stays default", cookie: themeCookieName, value: "dark", want: ""},
		{name: "garbage ignored", cookie: themeCookieName, value: "sepia", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dashboardTheme(requestWithCookie(tt.cookie, tt.value)); got != tt.want {
				t.Fatalf("dashboardTheme() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDashboardDensity(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		value  string
		want   string
	}{
		{name: "no cookie", want: ""},
		{name: "cozy", cookie: densityCookieName, value: "cozy", want: "cozy"},
		{name: "compact stays default", cookie: densityCookieName, value: "compact", want: ""},
		{name: "garbage ignored", cookie: densityCookieName, value: "roomy", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dashboardDensity(requestWithCookie(tt.cookie, tt.value)); got != tt.want {
				t.Fatalf("dashboardDensity() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyDashboardPreferences(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sidebarStateCookieName, Value: "false"})
	r.AddCookie(&http.Cookie{Name: themeCookieName, Value: "light"})
	r.AddCookie(&http.Cookie{Name: densityCookieName, Value: "cozy"})

	var data templates.DashboardData
	applyDashboardPreferences(r, &data)
	if !data.SidebarCollapsed {
		t.Fatalf("expected sidebar collapsed")
	}
	if data.Theme != "light" {
		t.Fatalf("theme = %q, want light", data.Theme)
	}
	if data.Density != "cozy" {
		t.Fatalf("density = %q, want cozy", data.Density)
	}
}
