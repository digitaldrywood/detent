package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func reopenCredentialFixture(t *testing.T, f *browserHostedFixture, maintenance bool) {
	t.Helper()
	cfg := f.service.config
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.CredentialMaintenance = maintenance
	cfg.ListenAddress = "127.0.0.1:0"
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
}

func testMaintenanceCheckLifecycle(t *testing.T, f *browserHostedFixture, path string, created tokenResponse, version tracker.ChangeVersion) {
	t.Helper()
	request := changeTestResult(version)
	for _, test := range []struct {
		name, method, suffix string
		status               int
	}{
		{"rotation", http.MethodPost, "/rotate", http.StatusOK},
		{"revocation", http.MethodDelete, "", http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			reopenCredentialFixture(t, f, true)
			response := performHubAPIRequest(t, f.service, test.method, "/api/v1/tokens/"+created.ID+test.suffix, testHubAdminToken, nil)
			requireNativeStatus(t, response, test.status)
			oldToken := created.Token
			if test.name == "rotation" {
				var rotated tokenResponse
				decodeHubResponse(t, response, &rotated)
				if rotated.ID != created.ID || rotated.Token == oldToken {
					t.Fatal("rotation changed principal or retained the old secret")
				}
				created = rotated
			}
			reopenCredentialFixture(t, f, false)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, oldToken, request), http.StatusUnauthorized)
			if test.name == "rotation" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, created.Token, request), http.StatusOK)
			}
		})
	}
}

func TestCredentialMaintenanceAccess(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	t.Cleanup(func() {
		if err := f.service.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, project := range []string{f.project, f.privateProject} {
		requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {project}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	}
	binding := runnerauth.NewBinding()
	path := "/api/v2/organizations/org_browser_preview/runner-enrollments"
	response := f.setupRequest(t, "owner", http.MethodPost, path, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	runnerToken, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/redeem", enrollment.Token, runnerauth.Redemption{Binding: binding, Credential: runnerToken, Hostname: "maintenance-test", DisplayName: "Maintenance test", Capacity: 1, Version: "test"}), http.StatusCreated)
	f.service.config.InitialAdminToken = []byte("replacement-bootstrap-secret")
	reopenCredentialFixture(t, f, true)
	var scopedAdmin, reporter, worker string
	for _, scope := range []apiScope{apiScopeAdmin, apiScopeOperator, apiScopeWorker} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, tokenRequest{Name: string(scope), Scope: scope})
		requireNativeStatus(t, response, http.StatusCreated)
		var token tokenResponse
		decodeHubResponse(t, response, &token)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", testHubAdminToken, map[string]string{"organization_id": "org_browser_preview", "project_id": f.project}), http.StatusNoContent)
		switch scope {
		case apiScopeAdmin:
			scopedAdmin = token.Token
		case apiScopeOperator:
			reporter = token.Token
		case apiScopeWorker:
			worker = token.Token
		}
	}
	for _, test := range []struct {
		name, bearer, cookie, origin string
	}{
		{name: "anonymous loopback"},
		{name: "invalid", bearer: "invalid"},
		{name: "reporter", bearer: reporter},
		{name: "worker", bearer: worker},
		{name: "enrolled runner", bearer: runnerToken},
		{name: "replacement bootstrap", bearer: "replacement-bootstrap-secret"},
		{name: "project administrator", bearer: scopedAdmin},
		{name: "owner cookie", cookie: "owner"},
		{name: "staff cookie", cookie: "staff"},
		{name: "support cookie", cookie: "support-viewer"},
		{name: "browser origin", bearer: testHubAdminToken, origin: f.server.URL},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, route := range []struct{ method, path string }{
				{http.MethodPost, "/api/v1/tokens"},
				{http.MethodPost, "/api/v1/tokens/example/rotate"},
				{http.MethodDelete, "/api/v1/tokens/example"},
				{http.MethodPost, "/api/v2/tokens/example/grants"},
			} {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set("Content-Type", "application/json")
				if test.bearer != "" {
					request.Header.Set("Authorization", "Bearer "+test.bearer)
				}
				if test.cookie != "" {
					request.AddCookie(f.cookies[test.cookie])
				}
				if test.origin != "" {
					request.Header.Set("Origin", test.origin)
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
					t.Fatalf("%s %s: status %d", route.method, route.path, response.Code)
				}
			}
		})
	}
	for _, path := range []string{"/", "/health", "/api/v1/work-items", "/api/v2/organizations/org_browser_preview/projects"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusNotFound)
	}
	var events int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE event = 'credential_maintenance' AND actual_actor = $1 AND effective_user = $1 AND organization_id = $2", "test_hub_admin", "org_browser_preview").Scan(&events); err != nil || events != 6 {
		t.Fatalf("attributed maintenance events = %d, error = %v", events, err)
	}
	t.Run("current issuer", func(t *testing.T) { testMaintenanceCurrentIssuer(t, f) })
	t.Run("tenant isolation", func(t *testing.T) { testMaintenanceTenantIsolation(t, f) })
	t.Run("self target", func(t *testing.T) { testMaintenanceSelfTarget(t, f) })
	reopenCredentialFixture(t, f, false)
	requireNativeStatus(t, maintenanceHTTP(t.Context(), f.service, f, "", http.MethodPost, "/api/v1/tokens", testHubAdminToken, "owner", tokenRequest{Name: "public-denied", Scope: apiScopeOperator}), http.StatusNotFound)
	var publicTokens int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens WHERE organization_id=$1 AND name=$2", f.service.accessOrganization(), "public-denied").Scan(&publicTokens); err != nil || publicTokens != 0 {
		t.Fatalf("normal-mode credential create rows=%d error=%v", publicTokens, err)
	}
	t.Run("lock order", func(t *testing.T) { testMaintenanceLockOrder(t) })
	t.Run("invitation order", func(t *testing.T) { testMaintenanceInvitationOrder(t) })
	t.Run("member preparation", func(t *testing.T) { testMaintenanceMemberPreparation(t) })
}

func TestCredentialMaintenanceConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		edit func(*Config)
	}{
		{"non-hosted", func(c *Config) { c.Hosted = nil }},
		{"public listener", func(c *Config) { c.ListenAddress = "0.0.0.0:7778" }},
		{"public TLS listener", func(c *Config) { c.ListenAddress = "0.0.0.0:7778"; c.TLSCertFile = "cert"; c.TLSKeyFile = "key" }},
		{"proxy", func(c *Config) { c.TrustedProxy = true }},
		{"unspecified provider", func(c *Config) { c.Hosted.WorkOSOrganizationID = "" }},
		{"wrong organization", func(c *Config) { c.Hosted.OrganizationID = "org_other" }},
		{"wrong provider", func(c *Config) { c.Hosted.WorkOSOrganizationID = "org_other" }},
		{"wrong origin", func(c *Config) { c.Hosted.PublicURL = "https://other.example.test" }},
		{"wrong bootstrap", func(c *Config) { c.Hosted.BootstrapSubject = "other-owner" }},
		{"unbound", func(c *Config) {}},
		{"wrong deployment", func(c *Config) {}},
		{"wrong generation", func(c *Config) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBrowserHostedFixture(t, true)
			cfg := f.service.config
			admin := hostedPostgresAdmin(t, f.service)
			var statement string
			switch test.name {
			case "unbound":
				statement = "DELETE FROM hosted_tenant WHERE organization_id=$1"
			case "wrong deployment":
				statement = "UPDATE hosted_tenant SET deployment='shared' WHERE organization_id=$1"
			case "wrong generation":
				statement = "UPDATE hosted_tenant SET allocation_generation=1 WHERE organization_id=$1"
			}
			if statement != "" {
				if _, err := admin.ExecContext(t.Context(), statement, f.service.accessOrganization()); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.CredentialMaintenance = true
			test.edit(&cfg)
			service, err := Open(t.Context(), cfg)
			if err == nil {
				service.Close()
				t.Fatal("invalid maintenance configuration was accepted")
			}
			if strings.HasPrefix(test.name, "wrong ") || test.name == "unbound" {
				if !errors.Is(err, ErrHostedDatabaseBinding) {
					t.Fatalf("binding error = %v", err)
				}
			}
		})
	}
}

func TestCredentialMaintenanceAuditOutcomes(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	t.Cleanup(func() {
		if err := f.service.Close(); err != nil {
			t.Error(err)
		}
	})
	reopenCredentialFixture(t, f, true)
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, tokenRequest{Name: "audit-token", Scope: apiScopeOperator})
	requireNativeStatus(t, response, http.StatusCreated)
	var created tokenResponse
	decodeHubResponse(t, response, &created)
	for _, test := range []struct {
		name, method, path string
		body               any
		status             int
	}{
		{"create", http.MethodPost, "/api/v1/tokens", tokenRequest{Name: "second-token", Scope: apiScopeOperator}, http.StatusCreated},
		{"rotate", http.MethodPost, "/api/v1/tokens/" + created.ID + "/rotate", nil, http.StatusOK},
		{"revoke", http.MethodDelete, "/api/v1/tokens/" + created.ID, nil, http.StatusNoContent},
		{"duplicate", http.MethodPost, "/api/v1/tokens", tokenRequest{Name: "audit-token", Scope: apiScopeOperator}, http.StatusConflict},
		{"invalid body", http.MethodPost, "/api/v1/tokens", map[string]string{"unknown": "field"}, http.StatusUnprocessableEntity},
		{"unknown token", http.MethodPost, "/api/v1/tokens/unknown/rotate", nil, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, test.method, test.path, testHubAdminToken, test.body), test.status)
			var status int
			var actor, route string
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT status, actual_actor, route FROM hosted_audit WHERE organization_id=$1 AND event = 'credential_maintenance' ORDER BY id DESC LIMIT 1", f.service.accessOrganization()).Scan(&status, &actor, &route); err != nil {
				t.Fatal(err)
			}
			if status != test.status || actor != "test_hub_admin" || route != test.method+" "+test.path {
				t.Fatalf("audit status=%d actor=%s route=%s", status, actor, route)
			}
		})
	}
}

func maintenanceCredential(t *testing.T, s *Service, id, secret string, administrator bool) accessstore.Credential {
	t.Helper()
	now := s.config.now().UTC().Truncate(time.Microsecond)
	row := accessstore.Credential{OrganizationID: s.accessOrganization(), ID: id, Name: id, Hash: apikey.HashToken(secret), Fingerprint: tokenFingerprint(apikey.HashToken(secret)), Scope: "operator", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}
	if administrator {
		row.Scope, row.KeyScope, row.ProjectAccess = "admin", "admin", "all"
	}
	if err := s.accessWrite(t.Context(), row.OrganizationID, func(w tenantstore.Writer) error { return w.CreateAccessCredential(t.Context(), row) }); err != nil {
		t.Fatal(err)
	}
	return maintenanceReadCredential(t, s, id)
}

func maintenanceReadCredential(t *testing.T, s *Service, id string) accessstore.Credential {
	t.Helper()
	var row accessstore.Credential
	if err := s.accessRead(t.Context(), s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		row, err = r.AccessCredential(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func maintenanceOpen(t *testing.T, s *Service) *Service {
	t.Helper()
	cfg := s.config
	cfg.CredentialMaintenance, cfg.ListenAddress = true, "127.0.0.1:0"
	opened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	})
	return opened
}

func testMaintenanceCurrentIssuer(t *testing.T, f *browserHostedFixture) {
	t.Helper()
	s := f.service
	repository := s.config.AccessRepository
	for _, fault := range []string{"revoked", "rotated", "expired", "role", "native", "hosted", "member", "selected", "kind", "outage"} {
		for _, operation := range []string{"create", "rotate", "revoke", "grant"} {
			t.Run(fault+"/"+operation, func(t *testing.T) {
				id := strings.ReplaceAll(t.Name(), "/", "_")
				issuer := maintenanceCredential(t, s, "issuer_"+id, "secret_"+id, true)
				target := maintenanceCredential(t, s, "target_"+id, "target_secret_"+id, false)
				method, path, body := http.MethodPost, "/api/v1/tokens", any(tokenRequest{Name: "new_" + id, Scope: apiScopeOperator})
				switch operation {
				case "rotate":
					path, body = "/api/v1/tokens/"+target.ID+"/rotate", nil
				case "revoke":
					method, path, body = http.MethodDelete, "/api/v1/tokens/"+target.ID, nil
				case "grant":
					path, body = "/api/v2/tokens/"+target.ID+"/grants", map[string]string{"organization_id": string(s.accessOrganization()), "project_id": f.project}
				}
				changed := false
				wrapped := &projectUpdateBeforeWriteRepository{Repository: repository, before: func() error {
					changed = true
					return repository.WriteTenant(t.Context(), s.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
						switch fault {
						case "revoked":
							return w.RevokeAccessCredential(t.Context(), issuer.ID, s.config.now())
						case "rotated":
							return w.RotateAccessCredential(t.Context(), issuer.ID, apikey.HashToken("replacement_"+id), "replacement", s.config.now())
						case "member":
							return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: s.accessOrganization(), UserID: "user_" + id, Email: id + "@example.test", MembershipID: "membership_" + id, Role: "owner", Active: true, PrincipalID: issuer.ID, CreatedAt: s.config.now(), UpdatedAt: s.config.now()})
						case "outage":
							return nil
						default:
							return nil
						}
					})
				}}
				underlyingBefore := wrapped.before
				wrapped.before = func() error {
					if err := underlyingBefore(); err != nil {
						return err
					}
					statement := map[string]string{
						"expired": "expires_at=created_at", "role": "scope='operator'", "native": "native_only=true", "hosted": "hosted_user_id='user_changed'", "selected": "operator_project_access='selected'", "kind": "key_kind='personal'",
					}[fault]
					if statement == "" {
						return nil
					}
					result, err := hostedPostgresAdmin(t, s).ExecContext(t.Context(), "UPDATE api_tokens SET "+statement+" WHERE organization_id=$1 AND id=$2 AND token_hash=$3", s.accessOrganization(), issuer.ID, issuer.Hash)
					if err != nil {
						return err
					}
					rows, err := result.RowsAffected()
					if err == nil && rows != 1 {
						return errors.New("issuer fixture CAS failed")
					}
					return err
				}
				var active tenantstore.Repository = wrapped
				if fault == "outage" {
					active = &maintenanceObservedRepository{Repository: wrapped, wrap: func(ctx context.Context, w tenantstore.Writer) tenantstore.Writer {
						return &maintenanceObservedWriter{Writer: w, authorize: func(accessstore.Authority) error { return tenantauth.ErrUnavailable }}
					}}
				}
				s.config.AccessRepository = active
				response := performHubAPIRequest(t, s, method, path, "secret_"+id, body)
				s.config.AccessRepository = repository
				if !changed {
					t.Fatal("request never crossed admitted-issuer boundary")
				}
				want := http.StatusForbidden
				if fault == "outage" {
					want = http.StatusServiceUnavailable
				}
				requireNativeStatus(t, response, want)
				after := maintenanceReadCredential(t, s, target.ID)
				if !reflect.DeepEqual(target, after) {
					t.Fatal("refused maintenance changed target credential")
				}
				var created int
				if err := hostedPostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens WHERE organization_id=$1 AND name=$2", s.accessOrganization(), "new_"+id).Scan(&created); err != nil || created != 0 {
					t.Fatalf("refused create rows=%d error=%v", created, err)
				}
				if err := s.accessRead(t.Context(), s.accessOrganization(), func(r tenantstore.Reader) error {
					_, err := r.AccessTokenGrant(t.Context(), target.ID, tracker.ProjectID(f.project))
					return err
				}); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("refused maintenance grant error=%v", err)
				}
			})
		}
	}
}

func testMaintenanceTenantIsolation(t *testing.T, f *browserHostedFixture) {
	t.Helper()
	first := f.service
	organization := tracker.OrganizationID("org_maintenance_second")
	cfg := first.config
	hosted := *cfg.Hosted
	hosted.OrganizationID, hosted.WorkOSOrganizationID, hosted.BootstrapSubject = string(organization), "provider_maintenance_second", "owner_maintenance_second"
	cfg.Hosted = &hosted
	if err := first.config.AccessRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := first.config.now()
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Second maintenance organization", Slug: "second-maintenance", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: hosted.WorkOSOrganizationID, BootstrapSubject: hosted.BootstrapSubject, PublicURL: hosted.PublicURL, Deployment: "origin"}); err != nil {
			return err
		}
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "second_admin", Name: "Second administrator", Hash: apikey.HashToken("second_admin_secret"), Scope: "admin", KeyScope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	seedHostedPostgresProject(t, second, "project_maintenance_second")
	for _, current := range []struct {
		s               *Service
		secret, project string
	}{{first, testHubAdminToken, f.project}, {second, "second_admin_secret", "project_maintenance_second"}} {
		response := performHubAPIRequest(t, current.s, http.MethodPost, "/api/v1/tokens", current.secret, tokenRequest{Name: "same-logical-name", Scope: apiScopeOperator})
		requireNativeStatus(t, response, http.StatusCreated)
		var token tokenResponse
		decodeHubResponse(t, response, &token)
		path := "/api/v2/tokens/" + token.ID + "/grants"
		requireNativeStatus(t, performHubAPIRequest(t, current.s, http.MethodPost, path, current.secret, map[string]string{"organization_id": string(current.s.accessOrganization()), "project_id": current.project}), http.StatusNoContent)
		otherOrg, otherProject := organization, "project_maintenance_second"
		if current.s == second {
			otherOrg, otherProject = first.accessOrganization(), f.project
		}
		requireNativeStatus(t, performHubAPIRequest(t, current.s, http.MethodPost, path, current.secret, map[string]string{"organization_id": string(otherOrg), "project_id": otherProject}), http.StatusForbidden)
		if err := current.s.accessRead(t.Context(), otherOrg, func(r tenantstore.Reader) error {
			_, err := r.AccessTokenGrant(t.Context(), token.ID, tracker.ProjectID(otherProject))
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("foreign grant error=%v", err)
		}
		if err := current.s.accessRead(t.Context(), current.s.accessOrganization(), func(r tenantstore.Reader) error {
			_, err := r.AccessTokenGrant(t.Context(), token.ID, tracker.ProjectID(current.project))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func testMaintenanceSelfTarget(t *testing.T, f *browserHostedFixture) {
	t.Helper()
	s := f.service
	row := maintenanceCredential(t, s, "self_maintenance", "self_secret", true)
	if err := s.accessWrite(t.Context(), s.accessOrganization(), func(w tenantstore.Writer) error {
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: s.accessOrganization(), TokenID: row.ID, ProjectID: tracker.ProjectID(f.project)})
	}); err != nil {
		t.Fatal(err)
	}
	expiry := s.config.now().Add(time.Hour).Truncate(time.Microsecond)
	if _, err := hostedPostgresAdmin(t, s).ExecContext(t.Context(), "UPDATE api_tokens SET expires_at=$3 WHERE organization_id=$1 AND id=$2", s.accessOrganization(), row.ID, expiry); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, s, http.MethodPost, "/api/v1/tokens/"+row.ID+"/rotate", "self_secret", nil)
	requireNativeStatus(t, response, http.StatusOK)
	var rotated tokenResponse
	decodeHubResponse(t, response, &rotated)
	if rotated.ID != row.ID || rotated.Token == "self_secret" || rotated.ExpiresAt == nil || !rotated.ExpiresAt.Equal(expiry) {
		t.Fatal("self rotation changed immutable identity or expiry")
	}
	requireNativeStatus(t, performHubAPIRequest(t, s, http.MethodPost, "/api/v1/tokens", "self_secret", tokenRequest{Name: "old-self-refused", Scope: apiScopeOperator}), http.StatusUnauthorized)
	requireNativeStatus(t, performHubAPIRequest(t, s, http.MethodDelete, "/api/v1/tokens/"+row.ID, rotated.Token, nil), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, s, http.MethodPost, "/api/v1/tokens", rotated.Token, tokenRequest{Name: "revoked-self-refused", Scope: apiScopeOperator}), http.StatusUnauthorized)
	var audits int
	if err := hostedPostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND actual_actor=$2 AND effective_user=$2 AND event='credential_maintenance' AND status IN (200,204)", s.accessOrganization(), row.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("self-target completion audits=%d error=%v", audits, err)
	}
	if err := s.accessRead(t.Context(), s.accessOrganization(), func(r tenantstore.Reader) error {
		_, err := r.AccessTokenGrant(t.Context(), row.ID, tracker.ProjectID(f.project))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type maintenanceRequestKey struct{}

type maintenanceObservedRepository struct {
	tenantstore.Repository
	wrap   func(context.Context, tenantstore.Writer) tenantstore.Writer
	before func(context.Context) error
	after  func(context.Context, tenantstore.Writer, error) error
}

func (r *maintenanceObservedRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if r.before != nil {
		if err := r.before(ctx); err != nil {
			return err
		}
	}
	var observed tenantstore.Writer
	err := r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		observed = w
		if r.wrap != nil {
			observed = r.wrap(ctx, w)
		}
		return write(observed)
	})
	if r.after != nil {
		return errors.Join(err, r.after(ctx, observed, err))
	}
	return err
}

type maintenanceObservedWriter struct {
	tenantstore.Writer
	event      func(context.Context, string, string) error
	authorize  func(accessstore.Authority) error
	modelsRead bool
}

func (w *maintenanceObservedWriter) observe(ctx context.Context, event, id string) error {
	if w.event == nil {
		return nil
	}
	return w.event(ctx, event, id)
}

func (w *maintenanceObservedWriter) LockAccessOrganization(ctx context.Context) error {
	if err := w.observe(ctx, "org_requested", ""); err != nil {
		return err
	}
	if err := w.Writer.LockAccessOrganization(ctx); err != nil {
		return err
	}
	return w.observe(ctx, "org_acquired", "")
}

func (w *maintenanceObservedWriter) AuthorizeAccess(ctx context.Context, authority accessstore.Authority, now time.Time) (accessstore.Member, error) {
	if w.authorize != nil {
		if err := w.authorize(authority); err != nil {
			return accessstore.Member{}, err
		}
	}
	row, err := w.Writer.AuthorizeAccess(ctx, authority, now)
	if err == nil {
		err = w.observe(ctx, "principal_read", authority.CredentialID)
	}
	return row, err
}

func (w *maintenanceObservedWriter) AccessCredential(ctx context.Context, id string) (accessstore.Credential, error) {
	row, err := w.Writer.AccessCredential(ctx, id)
	if err == nil {
		err = w.observe(ctx, "principal_read", id)
	}
	return row, err
}

func (w *maintenanceObservedWriter) RotateAccessCredential(ctx context.Context, id, hash, fingerprint string, now time.Time) error {
	if err := w.observe(ctx, "target_entered", id); err != nil {
		return err
	}
	return w.Writer.RotateAccessCredential(ctx, id, hash, fingerprint, now)
}

func (w *maintenanceObservedWriter) RevokeAccessCredential(ctx context.Context, id string, now time.Time) error {
	if err := w.observe(ctx, "target_entered", id); err != nil {
		return err
	}
	return w.Writer.RevokeAccessCredential(ctx, id, now)
}

func (w *maintenanceObservedWriter) OrganizationModels(ctx context.Context) (policystore.OrganizationModels, error) {
	row, err := w.Writer.OrganizationModels(ctx)
	if err == nil {
		w.modelsRead = true
	}
	return row, err
}

func (w *maintenanceObservedWriter) AccessInvitation(ctx context.Context, id string) (accessstore.Invitation, error) {
	if err := w.observe(ctx, "invitation_requested", id); err != nil {
		return accessstore.Invitation{}, err
	}
	return w.Writer.AccessInvitation(ctx, id)
}

func (w *maintenanceObservedWriter) AcceptAccessInvitation(ctx context.Context, id, email, subject string, now time.Time) error {
	if err := w.Writer.AcceptAccessInvitation(ctx, id, email, subject, now); err != nil {
		return err
	}
	return w.observe(ctx, "invitation_updated", id)
}

func (w *maintenanceObservedWriter) CreateThread(ctx context.Context, row threadstore.Record) error {
	if err := w.observe(ctx, "thread_insert", row.ID); err != nil {
		return err
	}
	if err := w.Writer.CreateThread(ctx, row); err != nil {
		return err
	}
	return w.observe(ctx, "thread_inserted", row.ID)
}

type maintenanceSignal struct {
	once  sync.Once
	ready chan struct{}
}

func newMaintenanceSignal() *maintenanceSignal { return &maintenanceSignal{ready: make(chan struct{})} }
func (s *maintenanceSignal) release()          { s.once.Do(func() { close(s.ready) }) }
func (s *maintenanceSignal) wait(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func maintenanceHTTP(ctx context.Context, s *Service, f *browserHostedFixture, actor, method, path, bearer, account string, value any) *httptest.ResponseRecorder {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(raw))).WithContext(context.WithValue(ctx, maintenanceRequestKey{}, actor))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if account != "" {
		request.Header.Set("Origin", f.server.URL)
		request.AddCookie(f.cookies[account])
		request.Header.Set("X-CSRF-Token", hostedCSRF(f.cookies[account].Value))
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	return response
}

func maintenanceFreshOwner(t *testing.T, f *browserHostedFixture, other bool) string {
	t.Helper()
	account, subject, email := "owner", "user_browser_owner", "owner@example.test"
	if other {
		account, subject, email = "other-owner", "user_maintenance_owner", "maintenance-owner@example.test"
		membership, err := f.provider.CreateMembership(t.Context(), subject, f.provider.organization.ID, "owner")
		if err != nil {
			t.Fatal(err)
		}
		identity := f.provider.identity(subject, email, f.provider.organization.ID, "")
		if err := f.service.storeHostedMember(t.Context(), identity, membership, ""); err != nil {
			t.Fatal(err)
		}
	}
	identity := f.provider.identity(subject, email, f.provider.organization.ID, "")
	cookie, _ := browserHostedSession(t, f.service, identity)
	f.cookies[account] = &http.Cookie{Name: hostedCookie, Value: cookie}
	return account
}

func testMaintenanceLockOrder(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"rotate", "revoke"} {
		for _, family := range []string{"notification", "slug", "organization model", "project model", "thread FK"} {
			t.Run(family+"/"+operation, func(t *testing.T) {
				f := newBrowserHostedFixture(t, true)
				normal := f.service
				requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {f.project}, "write": {"true"}}), http.StatusSeeOther)
				maintenance := maintenanceOpen(t, normal)
				var member accessstore.Member
				if err := normal.accessRead(t.Context(), normal.accessOrganization(), func(r tenantstore.Reader) error {
					var err error
					member, err = r.AccessMember(t.Context(), "user_browser_owner")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				principal := member.PrincipalID
				if err := normal.accessWrite(t.Context(), normal.accessOrganization(), func(w tenantstore.Writer) error {
					return w.RotateAccessCredential(t.Context(), principal, apikey.HashToken("model_principal_secret"), "model", normal.config.now())
				}); err != nil {
					t.Fatal(err)
				}
				before := maintenanceReadCredential(t, normal, principal)
				var models policystore.OrganizationModels
				var projectRevision int64
				var projectPayload [4]any
				if err := normal.config.TenantRepository.ReadTenant(t.Context(), normal.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					models, err = r.OrganizationModels(t.Context())
					if err != nil {
						return err
					}
					row, err := r.ProjectRecord(t.Context(), tracker.ProjectID(f.project))
					projectRevision = row.Settings.ModelRevision
					projectPayload = [4]any{row.Settings.ModelKey, row.Settings.ModelSHA256, row.Settings.ModelBytes, row.Settings.ModelContentBytes}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				orgHeld, releaseMaintenance, normalOrg, normalRead, releaseNormal, targetEntered, planningCommitted, releasePlanning := newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal()
				armed := newMaintenanceSignal()
				if family != "project model" {
					armed.release()
					releasePlanning.release()
				}
				var wg sync.WaitGroup
				defer func() {
					cancel()
					releaseMaintenance.release()
					releaseNormal.release()
					releasePlanning.release()
					wg.Wait()
				}()
				var errorMu sync.Mutex
				var failures []error
				record := func(err error) {
					if err != nil {
						errorMu.Lock()
						failures = append(failures, err)
						errorMu.Unlock()
					}
				}
				observe := func(ctx context.Context, event, id string) error {
					actor, _ := ctx.Value(maintenanceRequestKey{}).(string)
					if actor == "maintenance" {
						if event == "org_acquired" {
							orgHeld.release()
							return releaseMaintenance.wait(ctx)
						}
						if event == "target_entered" && id == principal {
							targetEntered.release()
						}
					}
					if actor == "normal" {
						select {
						case <-armed.ready:
						default:
							return nil
						}
						if event == "org_requested" {
							normalOrg.release()
						}
						if event == "principal_read" && id == principal {
							normalRead.release()
							return releaseNormal.wait(ctx)
						}
					}
					return nil
				}
				wrap := func(ctx context.Context, w tenantstore.Writer) tenantstore.Writer {
					return &maintenanceObservedWriter{Writer: w, event: observe}
				}
				normalOwner := normal.config.TenantRepository
				normalRepository := &maintenanceObservedRepository{Repository: normalOwner, wrap: wrap, after: func(ctx context.Context, w tenantstore.Writer, err error) error {
					record(err)
					if ctx.Value(maintenanceRequestKey{}) == "normal" && family == "project model" && err == nil {
						if observed, ok := w.(*maintenanceObservedWriter); ok && observed.modelsRead {
							planningCommitted.release()
							return releasePlanning.wait(ctx)
						}
					}
					return nil
				}}
				normal.config.TenantRepository, normal.config.AccessRepository = normalRepository, normalRepository
				maintenanceRepository := &maintenanceObservedRepository{Repository: maintenance.config.TenantRepository, wrap: wrap, after: func(_ context.Context, _ tenantstore.Writer, err error) error { record(err); return nil }}
				maintenance.config.TenantRepository, maintenance.config.AccessRepository = maintenanceRepository, maintenanceRepository
				base := "/api/v2/organizations/org_browser_preview"
				method, path, account, bearer, body := http.MethodPut, base+"/issue-notification-preference", "owner", "", any(map[string]bool{"enabled": false})
				switch family {
				case "slug":
					path, body = base+"/slug", map[string]string{"slug": "maintenance-changed", "expected_slug": "local-organization"}
				case "organization model":
					path, body = base+"/model-selection", cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: "raced-model"}, ExpectedRevision: tracker.Revision(models.Revision), Selection: &config.ModelSelection{NormalModel: new("maintenance-model")}}
				case "project model":
					path, body = base+"/projects/"+f.project+"/model-selection", cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: "raced-model"}, ExpectedRevision: tracker.Revision(projectRevision), Selection: &config.ModelSelection{NormalModel: new("maintenance-project-model")}}
				case "thread FK":
					method, path, body = http.MethodPost, base+"/threads", teamThreadInput{Scope: "general", Title: "Maintenance FK control", Key: "maintenance-thread"}
				}
				normalResult, maintenanceResult := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
				startNormal := func() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						normalResult <- maintenanceHTTP(ctx, normal, f, "normal", method, path, bearer, account, body)
					}()
				}
				if family == "project model" {
					startNormal()
					select {
					case <-planningCommitted.ready:
					case response := <-normalResult:
						t.Fatalf("normal request stopped before committed planning: status=%d", response.Code)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				maintenanceMethod, maintenanceSuffix, maintenanceStatus := http.MethodPost, "/rotate", http.StatusOK
				if operation == "revoke" {
					maintenanceMethod, maintenanceSuffix, maintenanceStatus = http.MethodDelete, "", http.StatusNoContent
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					maintenanceResult <- maintenanceHTTP(ctx, maintenance, f, "maintenance", maintenanceMethod, "/api/v1/tokens/"+principal+maintenanceSuffix, testHubAdminToken, "", nil)
				}()
				select {
				case <-orgHeld.ready:
				case response := <-maintenanceResult:
					t.Fatalf("maintenance stopped before organization lock: status=%d", response.Code)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if family == "project model" {
					armed.release()
					releasePlanning.release()
				} else {
					startNormal()
				}
				if family == "thread FK" {
					releaseNormal.release()
					if err := normalRead.wait(ctx); err != nil {
						t.Fatal(err)
					}
					var completed *httptest.ResponseRecorder
					testenv.WaitUntil(t, ctx, func() error {
						select {
						case completed = <-normalResult:
							return nil
						default:
						}
						var waiting bool
						if err := hostedPostgresAdmin(t, normal).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity n JOIN pg_stat_activity m ON m.pid=ANY(pg_blocking_pids(n.pid)) WHERE n.datname=current_database() AND n.wait_event_type='Lock' AND n.query LIKE 'INSERT INTO team_threads%' AND m.query LIKE 'SELECT organization_id FROM organizations%')`).Scan(&waiting); err != nil {
							return err
						}
						if waiting {
							t.Log("observed actual thread organization FK wait before releasing maintenance target")
							return nil
						}
						return errors.New("thread has neither committed nor reached its organization FK wait")
					})
					releaseMaintenance.release()
					if completed != nil {
						normalResult <- completed
					}
				} else {
					select {
					case <-normalOrg.ready:
						releaseNormal.release()
						releaseMaintenance.release()
					case <-normalRead.ready:
						releaseMaintenance.release()
						if err := targetEntered.wait(ctx); err != nil {
							t.Fatal(err)
						}
						releaseNormal.release()
					case response := <-normalResult:
						t.Fatalf("normal request stopped before lock-order boundary: status=%d", response.Code)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				m := <-maintenanceResult
				n := <-normalResult
				wg.Wait()
				requireNativeStatus(t, m, maintenanceStatus)
				want := http.StatusForbidden
				if family == "notification" && operation == "rotate" {
					want = http.StatusOK
				}
				if family == "thread FK" {
					want = http.StatusCreated
				}
				requireNativeStatus(t, n, want)
				errorMu.Lock()
				defer errorMu.Unlock()
				for _, failure := range failures {
					var sqlState interface{ SQLState() string }
					if errors.As(failure, &sqlState) && sqlState.SQLState() == "40P01" {
						t.Fatalf("actual delegated PostgreSQL deadlock: %v", failure)
					}
				}
				after := maintenanceReadCredential(t, normal, principal)
				if after.ID != before.ID || !reflect.DeepEqual(after.ExpiresAt, before.ExpiresAt) || after.Kind != before.Kind {
					t.Fatal("maintenance changed principal identity, expiry or kind")
				}
				if operation == "rotate" && (after.Hash == before.Hash || after.RevokedAt != nil) || operation == "revoke" && after.RevokedAt == nil {
					t.Fatal("maintenance target transition missing")
				}
				if err := normalOwner.ReadTenant(t.Context(), normal.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					current, err := r.AccessMember(t.Context(), member.UserID)
					if err != nil {
						return err
					}
					if family == "notification" && current.NotifyIssues != (operation == "revoke") {
						return errors.New("notification preference changed on refusal or missed successful update")
					}
					if current.PrincipalID != principal || current.MembershipID != member.MembershipID {
						return errors.New("member identity changed")
					}
					if family == "slug" {
						slug, err := r.AccessOrganization(t.Context())
						if err != nil {
							return err
						}
						if slug.Slug != "local-organization" {
							return errors.New("refused slug changed canonical name")
						}
						alias, err := r.AccessOrganizationSlug(t.Context(), "maintenance-changed")
						if err != nil {
							return err
						}
						if alias {
							return errors.New("refused slug wrote alias")
						}
					}
					if strings.Contains(family, "model") {
						current, err := r.OrganizationModels(t.Context())
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(current, models) {
							return errors.New("refused model mutation changed organization policy")
						}
						project, err := r.ProjectRecord(t.Context(), tracker.ProjectID(f.project))
						if err != nil {
							return err
						}
						if project.Settings.ModelRevision != projectRevision || !reflect.DeepEqual([4]any{project.Settings.ModelKey, project.Settings.ModelSHA256, project.Settings.ModelBytes, project.Settings.ModelContentBytes}, projectPayload) {
							return errors.New("refused model mutation changed project revision or payload")
						}
						var receipts int
						if err := hostedPostgresAdmin(t, normal).QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND actor_id=$2 AND command_key=$3", normal.accessOrganization(), principal, "raced-model").Scan(&receipts); err != nil {
							return err
						}
						if receipts != 0 {
							return errors.New("refused model mutation published receipt")
						}
					}
					if family == "thread FK" {
						thread, err := r.ThreadByRequest(t.Context(), principal, "maintenance-thread")
						if err != nil {
							return err
						}
						var response teamThread
						if err := json.Unmarshal(n.Body.Bytes(), &response); err != nil {
							return err
						}
						if thread.ID != response.ID || thread.Creator != principal || thread.Scope != "general" || thread.Title != "Maintenance FK control" {
							return errors.New("thread identity or scope changed")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var audits int
				if err := hostedPostgresAdmin(t, normal).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND actual_actor=$2 AND effective_user=$2 AND event='credential_maintenance' AND route=$3 AND status=$4", normal.accessOrganization(), "test_hub_admin", maintenanceMethod+" /api/v1/tokens/"+principal+maintenanceSuffix, maintenanceStatus).Scan(&audits); err != nil || audits != 1 {
					t.Fatalf("maintenance completion attribution count=%d error=%v", audits, err)
				}
				if err := normalOwner.ReadTenant(t.Context(), normal.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.AccessTokenGrant(t.Context(), principal, tracker.ProjectID(f.project))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if family == "slug" || strings.Contains(family, "model") {
					normal.config.TenantRepository, normal.config.AccessRepository = normalOwner, normalOwner
					account = maintenanceFreshOwner(t, f, operation == "revoke")
					if family == "project model" && operation == "rotate" {
						var rotated tokenResponse
						decodeHubResponse(t, m, &rotated)
						if rotated.ID != principal || rotated.Token == "" || apikey.HashToken(rotated.Token) != after.Hash {
							t.Fatal("rotation response does not match current principal")
						}
					}
					bearer = ""
					positive := maintenanceHTTP(t.Context(), normal, f, "positive", method, path, bearer, account, body)
					requireNativeStatus(t, positive, http.StatusOK)
					if strings.Contains(family, "model") {
						replay := maintenanceHTTP(t.Context(), normal, f, "positive", method, path, bearer, account, body)
						requireNativeStatus(t, replay, http.StatusOK)
						if positive.Body.String() != replay.Body.String() {
							t.Fatal("model receipt replay changed response")
						}
						changed := body.(cloudModelSelectionRequest)
						changed.Selection = &config.ModelSelection{NormalModel: new("conflicting-model")}
						requireNativeStatus(t, maintenanceHTTP(t.Context(), normal, f, "positive", method, path, bearer, account, changed), http.StatusConflict)
						stale := body.(cloudModelSelectionRequest)
						stale.IdempotencyKey += "-stale"
						requireNativeStatus(t, maintenanceHTTP(t.Context(), normal, f, "positive", method, path, bearer, account, stale), http.StatusConflict)
						var saved cloudModelSelection
						decodeHubResponse(t, positive, &saved)
						if saved.Revision != body.(cloudModelSelectionRequest).ExpectedRevision+1 {
							t.Fatal("model revision did not advance exactly once")
						}
						if family == "project model" && operation == "rotate" {
							testMaintenanceModelRevisionFence(t, f, path, bearer, account)
						}
					} else {
						requireNativeStatus(t, maintenanceHTTP(t.Context(), normal, f, "positive", method, path, bearer, account, map[string]string{"slug": "stale-alias", "expected_slug": "local-organization"}), http.StatusUnprocessableEntity)
					}
				}
			})
		}
	}
}

type maintenanceInvitationProvider struct {
	*browserHostedProvider
	accepted func(context.Context, string, string) error
	reads    int
	accepts  int
	readErr  error
	lookup   func(context.Context, string) error
}

func (p *maintenanceInvitationProvider) Invitation(ctx context.Context, token string) (auth.Invitation, error) {
	p.reads++
	if p.readErr != nil {
		return auth.Invitation{}, p.readErr
	}
	row, err := p.browserHostedProvider.Invitation(ctx, token)
	if err == nil && p.lookup != nil {
		err = p.lookup(ctx, token)
	}
	return row, err
}
func (p *maintenanceInvitationProvider) InvitationByID(ctx context.Context, token string) (auth.Invitation, error) {
	return p.Invitation(ctx, token)
}
func (p *maintenanceInvitationProvider) AcceptInvitation(ctx context.Context, token, user string) error {
	p.accepts++
	if err := p.browserHostedProvider.AcceptInvitation(ctx, token, user); err != nil {
		return err
	}
	if p.accepted != nil {
		return p.accepted(ctx, token, user)
	}
	return nil
}
func (p *maintenanceInvitationProvider) AcceptInvitationByID(ctx context.Context, token, user string) error {
	return p.AcceptInvitation(ctx, token, user)
}

func testMaintenanceInvitationOrder(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"rotate", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			f := newBrowserHostedFixture(t, true)
			normal := f.service
			for _, project := range []string{f.project, f.privateProject} {
				requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {project}, "write": {"true"}}), http.StatusSeeOther)
			}
			base := "/api/v2/organizations/org_browser_preview/members/invitations"
			response := f.setupRequest(t, "owner", http.MethodPost, base, map[string]any{"email": "ordered-invitee@example.test", "role": "member", "idempotency_key": "ordered-invitation", "grants": []hostedMemberGrant{{ProjectID: f.project, Write: true}}})
			requireNativeStatus(t, response, http.StatusCreated)
			var invitation hostedInvitationView
			decodeHubResponse(t, response, &invitation)
			var editor accessstore.Member
			if err := normal.accessRead(t.Context(), normal.accessOrganization(), func(r tenantstore.Reader) error {
				var err error
				editor, err = r.AccessMember(t.Context(), "user_browser_owner")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			maintenance := maintenanceOpen(t, normal)
			acceptConfig := normal.config
			acceptConfig.ListenAddress = "127.0.0.1:0"
			accepting, err := Open(t.Context(), acceptConfig)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), accepting.config.ShutdownTimeout)
				defer cancel()
				if err := accepting.CloseContext(cleanup); err != nil {
					t.Error(err)
				}
			})
			identity := f.provider.identity("user_ordered_invitee", "ordered-invitee@example.test", f.provider.organization.ID, "")
			ctx, cancel := context.WithCancel(t.Context())
			editPrepared, releaseEdit, orgHeld, releaseMaintenance, acceptOrg, acceptUpdated, releaseAccept, editorInvitation, maintenanceTarget := newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal(), newMaintenanceSignal()
			var wg sync.WaitGroup
			defer func() {
				cancel()
				releaseEdit.release()
				releaseMaintenance.release()
				releaseAccept.release()
				wg.Wait()
			}()
			providerPreflight := newMaintenanceSignal()
			provider := &maintenanceInvitationProvider{browserHostedProvider: f.provider, lookup: func(ctx context.Context, id string) error {
				if ctx.Value(maintenanceRequestKey{}) == "edit" && id == invitation.ID {
					providerPreflight.release()
				}
				return nil
			}}
			normal.config.Hosted.Provider = provider
			accepting.config.Hosted.Provider = provider
			var errorMu sync.Mutex
			var failures []error
			wrap := func(ctx context.Context, w tenantstore.Writer) tenantstore.Writer {
				return &maintenanceObservedWriter{Writer: w, event: func(ctx context.Context, event, id string) error {
					switch ctx.Value(maintenanceRequestKey{}) {
					case "maintenance":
						if event == "org_acquired" {
							orgHeld.release()
							return releaseMaintenance.wait(ctx)
						}
						if event == "target_entered" && id == editor.PrincipalID {
							maintenanceTarget.release()
						}
					case "accept":
						if event == "org_requested" {
							acceptOrg.release()
						}
						if event == "invitation_updated" {
							acceptUpdated.release()
							return releaseAccept.wait(ctx)
						}
					case "edit":
						if event == "invitation_requested" && id == invitation.ID {
							editorInvitation.release()
						}
					}
					return nil
				}}
			}
			owner := normal.config.TenantRepository
			record := func(_ context.Context, _ tenantstore.Writer, err error) error {
				if err != nil {
					errorMu.Lock()
					failures = append(failures, err)
					errorMu.Unlock()
				}
				return nil
			}
			repository := &maintenanceObservedRepository{Repository: owner, wrap: wrap, before: func(ctx context.Context) error {
				if ctx.Value(maintenanceRequestKey{}) == "edit" {
					select {
					case <-providerPreflight.ready:
						editPrepared.release()
						return releaseEdit.wait(ctx)
					default:
					}
				}
				return nil
			}, after: record}
			normal.config.AccessRepository, normal.config.TenantRepository = repository, repository
			acceptRepository := &maintenanceObservedRepository{Repository: accepting.config.TenantRepository, wrap: wrap, after: record}
			accepting.config.AccessRepository, accepting.config.TenantRepository = acceptRepository, acceptRepository
			mRepository := &maintenanceObservedRepository{Repository: maintenance.config.TenantRepository, wrap: wrap, after: record}
			maintenance.config.AccessRepository, maintenance.config.TenantRepository = mRepository, mRepository
			edited := []hostedMemberGrant{{ProjectID: f.privateProject, Write: true}}
			editorResult, maintenanceResult, acceptResult := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1), make(chan error, 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				editorResult <- maintenanceHTTP(ctx, normal, f, "edit", http.MethodPut, base+"/"+invitation.ID, "", "owner", map[string]any{"grants": edited})
			}()
			select {
			case <-editPrepared.ready:
			case response := <-editorResult:
				t.Fatalf("editor stopped before completed pending preflight: status=%d", response.Code)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			method, suffix, status := http.MethodPost, "/rotate", http.StatusOK
			if operation == "revoke" {
				method, suffix, status = http.MethodDelete, "", http.StatusNoContent
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				maintenanceResult <- maintenanceHTTP(ctx, maintenance, f, "maintenance", method, "/api/v1/tokens/"+editor.PrincipalID+suffix, testHubAdminToken, "", nil)
			}()
			select {
			case <-orgHeld.ready:
			case response := <-maintenanceResult:
				t.Fatalf("maintenance stopped before held organization lock: status=%d", response.Code)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				acceptResult <- accepting.acceptHostedInvitationFor(context.WithValue(ctx, maintenanceRequestKey{}, "accept"), identity, invitation.ID)
			}()
			select {
			case <-acceptOrg.ready:
				releaseAccept.release()
				releaseEdit.release()
				requireNativeStatus(t, <-editorResult, http.StatusNoContent)
				releaseMaintenance.release()
			case <-acceptUpdated.ready:
				releaseEdit.release()
				if err := editorInvitation.wait(ctx); err != nil {
					t.Fatal(err)
				}
				releaseMaintenance.release()
				if err := maintenanceTarget.wait(ctx); err != nil {
					t.Fatal(err)
				}
				releaseAccept.release()
				requireNativeStatus(t, <-editorResult, http.StatusNoContent)
			case err := <-acceptResult:
				t.Fatalf("acceptance stopped before organization/invitation boundary: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			requireNativeStatus(t, <-maintenanceResult, status)
			if err := <-acceptResult; err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if provider.accepts != 1 {
				t.Fatalf("provider accept calls=%d", provider.accepts)
			}
			if err := owner.ReadTenant(t.Context(), normal.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.AccessInvitation(t.Context(), invitation.ID)
				if err != nil {
					return err
				}
				if stored.AcceptedUserID != identity.Subject || stored.Role != "member" {
					return errors.New("invitation acceptance identity changed")
				}
				member, err := r.AccessMember(t.Context(), identity.Subject)
				if err != nil {
					return err
				}
				if member.PrincipalID != "hosted_"+apikey.HashToken(identity.Subject)[:32] || member.MembershipID != "membership_"+identity.Subject || !member.Active {
					return errors.New("accepted member identity changed")
				}
				grants, err := r.AccessTokenGrants(t.Context(), member.PrincipalID, "", 10)
				if err != nil {
					return err
				}
				if len(grants) != 1 || string(grants[0].ProjectID) != f.privateProject {
					return errors.New("acceptance missed committed replacement grants")
				}
				grant, err := r.AccessProjectGrant(t.Context(), identity.Subject, tracker.ProjectID(f.privateProject))
				if err != nil {
					return err
				}
				if !grant.CanWrite || grant.ManageRunner {
					return errors.New("accepted grant permissions changed")
				}
				_, err = r.AccessProjectGrant(t.Context(), identity.Subject, tracker.ProjectID(f.project))
				if !errors.Is(err, corestore.ErrNotFound) {
					return errors.New("acceptance retained replaced grant")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var members, principals int
			if err := hostedPostgresAdmin(t, normal).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND user_id=$2), (SELECT count(*) FROM api_tokens WHERE organization_id=$1 AND id=$3)`, normal.accessOrganization(), identity.Subject, "hosted_"+apikey.HashToken(identity.Subject)[:32]).Scan(&members, &principals); err != nil || members != 1 || principals != 1 {
				t.Fatalf("canonical member/principal counts=%d/%d error=%v", members, principals, err)
			}
			errorMu.Lock()
			defer errorMu.Unlock()
			for _, failure := range failures {
				var state interface{ SQLState() string }
				if errors.As(failure, &state) && state.SQLState() == "40P01" {
					t.Fatalf("delegated three-owner deadlock: %v", failure)
				}
			}
		})
	}
}

func testMaintenanceMemberPreparation(t *testing.T) {
	t.Helper()
	for _, scenario := range []string{"provider read failure", "pending random failure", "accepted random failure reconciliation", "concurrent principal", "existing principal", "revoked principal", "missing preparation", "wrong preparation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newBrowserHostedFixture(t, true)
			s := f.service
			identity := f.provider.identity("user_prepared_invitee", "prepared-invitee@example.test", f.provider.organization.ID, "")
			base := "/api/v2/organizations/org_browser_preview/members/invitations"
			response := f.setupRequest(t, "owner", http.MethodPost, base, map[string]any{"email": identity.Email, "role": "member", "idempotency_key": "prepare-invitation", "grants": []hostedMemberGrant{}})
			requireNativeStatus(t, response, http.StatusCreated)
			var invitation hostedInvitationView
			decodeHubResponse(t, response, &invitation)
			principal := "hosted_" + apikey.HashToken(identity.Subject)[:32]
			provider := &maintenanceInvitationProvider{browserHostedProvider: f.provider}
			s.config.Hosted.Provider = provider
			originalGenerator := s.config.generateToken
			generated := 0
			generationFailure := errors.New("fixture random unavailable")
			s.config.generateToken = func() (string, error) { generated++; return "", generationFailure }
			if scenario == "provider read failure" {
				provider.readErr = tenantauth.ErrUnavailable
			}
			if scenario == "accepted random failure reconciliation" {
				if err := f.provider.AcceptInvitation(t.Context(), invitation.ID, identity.Subject); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "provider read failure" || scenario == "pending random failure" || scenario == "accepted random failure reconciliation" {
				err := s.acceptHostedInvitationFor(t.Context(), identity, invitation.ID)
				if scenario == "provider read failure" {
					if !errors.Is(err, tenantauth.ErrUnavailable) || generated != 0 {
						t.Fatalf("provider failure=%v generated=%d", err, generated)
					}
				} else if !errors.Is(err, generationFailure) || generated != 1 {
					t.Fatalf("preparation failure=%v generated=%d", err, generated)
				}
				if provider.accepts != 0 {
					t.Fatal("preparation failure issued external acceptance")
				}
				if err := s.accessRead(t.Context(), s.accessOrganization(), func(r tenantstore.Reader) error { _, err := r.AccessMember(t.Context(), identity.Subject); return err }); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("preparation failure wrote member: %v", err)
				}
				if err := s.accessRead(t.Context(), s.accessOrganization(), func(r tenantstore.Reader) error { _, err := r.AccessCredential(t.Context(), principal); return err }); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("preparation failure wrote principal: %v", err)
				}
				if scenario != "accepted random failure reconciliation" {
					return
				}
				s.config.generateToken = originalGenerator
				if err := s.acceptHostedInvitationFor(t.Context(), identity, invitation.ID); err != nil {
					t.Fatal(err)
				}
				if provider.accepts != 0 {
					t.Fatal("accepted reconciliation reissued provider acceptance")
				}
			} else {
				membership, err := f.provider.CreateMembership(t.Context(), identity.Subject, f.provider.organization.ID, "member")
				if err != nil {
					t.Fatal(err)
				}
				prepared := accessMemberPreparation{}
				if scenario == "concurrent principal" {
					s.config.generateToken = originalGenerator
					prepared, err = s.prepareAccessMember(t.Context(), identity)
					if err != nil {
						t.Fatal(err)
					}
				}
				var stored accessstore.Credential
				if scenario == "existing principal" || scenario == "revoked principal" || scenario == "concurrent principal" {
					stored = maintenanceCredential(t, s, principal, "existing_principal_secret", false)
					if scenario == "revoked principal" {
						if err := s.accessWrite(t.Context(), s.accessOrganization(), func(w tenantstore.Writer) error {
							return w.RevokeAccessCredential(t.Context(), principal, s.config.now())
						}); err != nil {
							t.Fatal(err)
						}
						stored = maintenanceReadCredential(t, s, principal)
					}
					if scenario != "concurrent principal" {
						prepared, err = s.prepareAccessMember(t.Context(), identity)
						if err != nil || generated != 0 {
							t.Fatalf("existing preparation error=%v generated=%d", err, generated)
						}
					}
				}
				if scenario == "wrong preparation" {
					prepared = accessMemberPreparation{organization: "org_foreign", subject: identity.Subject, principal: principal, hash: "untrusted", fingerprint: "untrusted"}
				}
				err = s.accessWrite(t.Context(), s.accessOrganization(), func(w tenantstore.Writer) error {
					return s.storeAccessMember(t.Context(), w, identity, membership, prepared)
				})
				if scenario == "missing preparation" || scenario == "wrong preparation" {
					if !errors.Is(err, auth.ErrHostedIdentity) {
						t.Fatalf("invalid construction accepted: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				current := maintenanceReadCredential(t, s, principal)
				if !reflect.DeepEqual(stored, current) {
					t.Fatal("member publication replaced or revived existing principal")
				}
			}
			var count int
			if err := hostedPostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND user_id=$2 AND principal_id=$3", s.accessOrganization(), identity.Subject, principal).Scan(&count); err != nil || count != 1 {
				t.Fatalf("canonical publication count=%d error=%v", count, err)
			}
		})
	}
}

func testMaintenanceModelRevisionFence(t *testing.T, f *browserHostedFixture, path, bearer, account string) {
	t.Helper()
	s := f.service
	owner := s.config.TenantRepository
	peerConfig := s.config
	peerConfig.ListenAddress = "127.0.0.1:0"
	peer, err := Open(t.Context(), peerConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), peer.config.ShutdownTimeout)
		defer cancel()
		if err := peer.CloseContext(cleanup); err != nil {
			t.Error(err)
		}
	})
	var projectBefore int64
	if err := owner.ReadTenant(t.Context(), s.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.ProjectRecord(t.Context(), tracker.ProjectID(f.project))
		projectBefore = project.Settings.ModelRevision
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	planned, resume := newMaintenanceSignal(), newMaintenanceSignal()
	var once sync.Once
	var wg sync.WaitGroup
	defer func() {
		cancel()
		resume.release()
		wg.Wait()
		s.config.TenantRepository, s.config.AccessRepository = owner, owner
	}()
	repository := &maintenanceObservedRepository{Repository: owner, wrap: func(_ context.Context, w tenantstore.Writer) tenantstore.Writer {
		return &maintenanceObservedWriter{Writer: w}
	}, after: func(ctx context.Context, w tenantstore.Writer, err error) error {
		if ctx.Value(maintenanceRequestKey{}) == "model-fence" && err == nil {
			if observed, ok := w.(*maintenanceObservedWriter); ok && observed.modelsRead {
				once.Do(planned.release)
				return resume.wait(ctx)
			}
		}
		return nil
	}}
	s.config.TenantRepository, s.config.AccessRepository = repository, repository
	input := cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: "shared-revision-fence"}, ExpectedRevision: tracker.Revision(projectBefore), Selection: &config.ModelSelection{NormalModel: new("fenced-project-model")}}
	result := make(chan *httptest.ResponseRecorder, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		result <- maintenanceHTTP(ctx, s, f, "model-fence", http.MethodPut, path, bearer, account, input)
	}()
	select {
	case <-planned.ready:
	case response := <-result:
		t.Fatalf("project request stopped before committed model planning: status=%d", response.Code)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	organizationPath := "/api/v2/organizations/org_browser_preview/model-selection"
	freshAccount := maintenanceFreshOwner(t, f, true)
	var before cloudModelSelection
	response := f.setupRequest(t, freshAccount, http.MethodGet, organizationPath, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &before)
	changed := cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: "organization-between-project-phases"}, ExpectedRevision: before.Revision, Selection: &config.ModelSelection{NormalModel: new("new-shared-model")}}
	requireNativeStatus(t, maintenanceHTTP(t.Context(), peer, f, "organization-fence", http.MethodPut, organizationPath, "", freshAccount, changed), http.StatusOK)
	resume.release()
	requireNativeStatus(t, <-result, http.StatusConflict)
	wg.Wait()
	if err := owner.ReadTenant(t.Context(), s.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.ProjectRecord(t.Context(), tracker.ProjectID(f.project))
		if err != nil {
			return err
		}
		if project.Settings.ModelRevision != projectBefore {
			return errors.New("stale organization revision changed project model")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := hostedPostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$2", s.accessOrganization(), input.IdempotencyKey).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale shared revision receipts=%d error=%v", count, err)
	}
}
