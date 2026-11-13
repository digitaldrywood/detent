package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/releasenotes"
)

func testReleaseBuilds() []releasenotes.Build {
	return []releasenotes.Build{
		{Version: "v1.2.1", BuildTime: time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC), Entries: []releasenotes.Entry{{Category: "Fixed", Text: "Retain state", PRNumber: 103, PRURL: "https://github.com/digitaldrywood/detent/pull/103"}}},
		{Version: "v1.2.0-op.r.1.g123456789abc", Patch: true, BuildTime: time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC), Entries: []releasenotes.Entry{}, InternalCount: 5},
		{Version: "v1.2.0", BuildTime: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), Entries: []releasenotes.Entry{}},
	}
}

func TestReleaseNotesPreferences(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	alice := f.user(t, "alice", "viewer", "alice@example.test", "read", "")
	bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
	fresh := alice
	token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), alice.identity)
	if err != nil {
		t.Fatal(err)
	}
	fresh.token = token
	f.service.releaseNotes = testReleaseBuilds()[2:]
	f.service.config.Version = "1.2.0"
	path := "/api/v2/organizations/org_security/release-notes"
	for _, user := range []hostedSecurityUser{alice, bob} {
		response := f.request(t, user, http.MethodPut, path+"/seen", nil)
		requireNativeStatus(t, response, http.StatusOK)
	}
	f.service.releaseNotes = testReleaseBuilds()
	f.service.config.Version = "1.2.1"
	oldest := f.request(t, bob, http.MethodGet, path+"?cursor="+base64.RawURLEncoding.EncodeToString([]byte(f.service.releaseNotes[1].Version)), nil)
	requireNativeStatus(t, oldest, http.StatusOK)
	var olderPage releaseNotesPage
	decodeHubResponse(t, oldest, &olderPage)
	if !olderPage.HasUnread || len(olderPage.Builds) != 1 || olderPage.Builds[0].Unread {
		t.Fatalf("unread outside page=%+v", olderPage)
	}
	for _, test := range []struct {
		name         string
		user         hostedSecurityUser
		method, path string
		unread       bool
	}{
		{"new build", alice, http.MethodGet, path, true},
		{"other member", bob, http.MethodGet, path, true},
		{"mark seen", alice, http.MethodPut, path + "/seen", false},
		{"another device", fresh, http.MethodGet, path, false},
		{"other member unchanged", bob, http.MethodGet, path, true},
		{"other member seen", bob, http.MethodPut, path + "/seen", false},
		{"repeat mark", alice, http.MethodPut, path + "/seen", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, test.user, test.method, test.path, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page releaseNotesPage
			decodeHubResponse(t, response, &page)
			if len(page.Builds) != 3 || page.HasUnread != test.unread || page.Builds[0].Unread != test.unread || page.Builds[1].Unread != test.unread || page.Builds[2].Unread || !page.Builds[0].Running || page.Builds[1].Running || !page.Builds[1].Patch || page.Builds[1].InternalCount != 5 || page.Builds[0].Entries[0].PRNumber != 103 {
				t.Fatalf("page=%+v", page)
			}
			response = f.request(t, test.user, http.MethodGet, "/app/bootstrap", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var bootstrap appBootstrap
			decodeHubResponse(t, response, &bootstrap)
			if bootstrap.HasUnread != test.unread {
				t.Fatalf("bootstrap unread=%t", bootstrap.HasUnread)
			}
		})
	}
	charlie := f.user(t, "charlie", "viewer", "charlie@example.test", "read", "")
	for _, user := range []hostedSecurityUser{charlie, fresh} {
		response := f.request(t, user, http.MethodGet, path+"?limit=1", nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page releaseNotesPage
		decodeHubResponse(t, response, &page)
		if page.HasUnread || page.Builds[0].Unread || page.NextCursor == "" {
			t.Fatalf("page=%+v", page)
		}
		response = f.request(t, user, http.MethodGet, path+"?limit=2&cursor="+page.NextCursor, nil)
		requireNativeStatus(t, response, http.StatusOK)
		page = releaseNotesPage{}
		decodeHubResponse(t, response, &page)
		if len(page.Builds) != 2 || page.Builds[0].Version != "v1.2.0-op.r.1.g123456789abc" || page.NextCursor != "" {
			t.Fatalf("older=%+v", page)
		}
	}
	for _, query := range []string{"?limit=-1", "?limit=101", "?limit=bad", "?cursor=bad!", "?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("unknown"))} {
		requireNativeStatus(t, f.request(t, alice, http.MethodGet, path+query, nil), http.StatusUnprocessableEntity)
	}
	f.service.config.Version = "1.2.0"
	requireNativeStatus(t, f.request(t, alice, http.MethodPut, path+"/seen", nil), http.StatusOK)
	var member accessstore.Member
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), alice.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	preference, err := f.service.config.AccessRepository.(accountstore.Repository).ReleasePreference(t.Context(), "org_security", alice.identity.Subject, member.MembershipID, member.PrincipalID)
	saved := preference.Version
	if err != nil || saved != "v1.2.1" {
		t.Fatalf("rollback preference=%q error=%v", saved, err)
	}
	var count int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_release_preferences").Scan(&count); err != nil || count != 2 {
		t.Fatalf("preferences=%d error=%v", count, err)
	}
}

func TestReleaseNotesMCP(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"stdio", "http"} {
		for _, test := range []struct {
			name   string
			denied bool
		}{
			{"local admin", true},
			{"local operator", true},
			{"local worker", true},
			{"personal entry", false},
			{"service entry", true},
		} {
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				contexts := make(chan context.Context, 1)
				var service *Service
				var ctx context.Context
				const route = "/api/v2/organizations/:organization/release-mcp-admission"
				capture := func(c echo.Context) error {
					contexts <- operatortool.BindConnection(c.Request().Context(), "release-admission", "test")
					return c.NoContent(http.StatusOK)
				}
				switch test.name {
				case "local admin", "local operator", "local worker":
					f := newNativeFixture(t, nil, "", "Example project")
					service = f.service
					service.echo.POST(route, capture, service.operatorAuthority)
					token := testHubAdminToken
					if test.name == "local operator" {
						token = f.token
					}
					if test.name == "local worker" {
						token = f.worker(t, "release worker")
					}
					response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/release-mcp-admission", token, nil)
					if test.name == "local worker" {
						requireNativeStatus(t, response, http.StatusForbidden)
						return
					}
					requireNativeStatus(t, response, http.StatusOK)
					ctx = <-contexts
				case "personal entry", "service entry":
					f := newEntryKeyFixture(t)
					user := f.user(t, "viewer")
					service = f.service
					service.echo.POST(route, capture, service.operatorAuthority)
					key := apikey.KeyAuthority{ID: "release_entry", Kind: "personal", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}
					if test.name == "service entry" {
						key.Kind = "service"
					}
					response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/api/v2/organizations/org_security/release-mcp-admission", mutate: func(claims *cloudassert.Claims) {
						claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "provider_org_security", "viewer"
						if key.Kind == "service" {
							claims.Subject, claims.Role = "service:"+key.ID, "admin"
						}
					}})
					requireNativeStatus(t, response, http.StatusOK)
					ctx = <-contexts
				}
				service.releaseNotes = testReleaseBuilds()
				service.config.Version = "1.2.1"
				send := hostedContextProtocol(t, service, ctx, transport)
				raw := hostedContextData(t, send("tools/call", operatortool.ReleaseNotes, map[string]any{"limit": 1}), test.denied)
				if test.denied {
					return
				}
				var page releaseNotesPage
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				if page.HasUnread || len(page.Builds) != 1 || page.Builds[0].Version != "v1.2.1" || !page.Builds[0].Running || page.Builds[0].Unread || page.NextCursor == "" {
					t.Fatalf("personal entry release baseline=%+v", page)
				}
				var older releaseNotesPage
				if err := json.Unmarshal(hostedContextData(t, send("tools/call", operatortool.ReleaseNotes, map[string]any{"limit": 2, "cursor": page.NextCursor}), false), &older); err != nil {
					t.Fatal(err)
				}
				if older.HasUnread || len(older.Builds) != 2 || older.Builds[0].Version != "v1.2.0-op.r.1.g123456789abc" || older.NextCursor != "" {
					t.Fatalf("personal entry release pagination=%+v", older)
				}
			})
		}
	}
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(deployment+"/"+transport, func(t *testing.T) {
				f, ctx := newHostedKeyMCPFixture(t, deployment, "viewer")
				f.service.releaseNotes = testReleaseBuilds()
				f.service.config.Version = "1.2.1"
				send := hostedContextProtocol(t, f.service, ctx, transport)
				raw := hostedContextData(t, send("tools/call", operatortool.ReleaseNotes, map[string]any{"limit": 1}), false)
				var mcpPage, browserPage releaseNotesPage
				if err := json.Unmarshal(raw, &mcpPage); err != nil {
					t.Fatal(err)
				}
				response := f.browser(http.MethodGet, "/api/v2/organizations/org_security/release-notes?limit=1", nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &browserPage)
				if !reflect.DeepEqual(mcpPage, browserPage) || len(mcpPage.Builds) != 1 || !mcpPage.Builds[0].Running {
					t.Fatalf("mcp=%+v browser=%+v", mcpPage, browserPage)
				}
				hostedContextData(t, send("tools/call", operatortool.ReleaseNotes, map[string]any{"limit": 101}), true)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), f.user.identity.Subject)
					if err != nil {
						return err
					}
					member.Active = false
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
				hostedContextData(t, send("tools/call", operatortool.ReleaseNotes, map[string]any{}), true)
			})
		}
	}
}

func TestReleaseNotesAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, user, path string }{
		{"unauthenticated", "", "/api/v2/organizations/org_security/release-notes"},
		{"wrong organization", "member", "/api/v2/organizations/org_other/release-notes"},
		{"revoked member", "revoked", "/api/v2/organizations/org_security/release-notes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			f.service.releaseNotes = testReleaseBuilds()
			f.service.config.Version = "1.2.1"
			user := hostedSecurityUser{}
			if test.user != "" {
				user = f.user(t, test.user, "member", "member@example.test", "read", "")
			}
			if test.user == "revoked" {
				if err := f.provider.RevokeMembership(t.Context(), "membership_"+user.identity.Subject); err != nil {
					t.Fatal(err)
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				path := test.path
				if method == http.MethodPut {
					path += "/seen"
				}
				response := f.request(t, user, method, path, nil)
				if response.Code == http.StatusOK {
					t.Fatalf("unauthorized %s succeeded", method)
				}
			}
		})
	}
}
