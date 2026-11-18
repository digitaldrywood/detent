package requestmetrics_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }
func TestRequestMetricsAdminReads(t *testing.T) {
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	f := newMetricsHTTPFixture(t, admin, repository)
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	member := f.user(t, "member", "member", "member@example.test", "", "")
	path := "/api/v2/organizations/org_security/diagnostics/requests"
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, path, nil), http.StatusOK)
	requireNativeStatus(t, f.request(t, member, http.MethodGet, path, nil), http.StatusForbidden)
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/api/v2/organizations/org_other/diagnostics/requests", nil), http.StatusNotFound)
	serverTiming := regexp.MustCompile(`^db;dur=[0-9.]+, compute;dur=[0-9.]+, encode;dur=[0-9.]+, total;dur=[0-9.]+(?:, pg_[a-z_]+;dur=[0-9.]+)*$`)
	for _, test := range []struct {
		name     string
		response *httptest.ResponseRecorder
		timed    bool
	}{
		{"authenticated admin", f.request(t, owner, http.MethodGet, path, nil), true},
		{"authenticated without access", f.request(t, member, http.MethodGet, path, nil), true},
		{"anonymous", f.tokenRequest(t, http.MethodGet, path, "", nil), false},
		{"invalid token", f.tokenRequest(t, http.MethodGet, path, "detent_invalid", nil), false},
	} {
		t.Run("server timing/"+test.name, func(t *testing.T) {
			header := test.response.Header().Get("Server-Timing")
			if test.timed != serverTiming.MatchString(header) || !test.timed && header != "" {
				t.Fatalf("status=%d Server-Timing=%q", test.response.Code, header)
			}
		})
	}
	historyResponse := f.request(t, owner, http.MethodGet, path+"?window=24h", nil)
	requireNativeStatus(t, historyResponse, http.StatusOK)
	var history requestmetrics.Response
	decodeHubResponse(t, historyResponse, &history)
	if history.RouteLatency == nil || history.RouteLatency.Bucket != "1h0m0s" {
		t.Fatalf("route latency history: %s", historyResponse.Body.String())
	}
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, path+"?window=1m", nil), http.StatusBadRequest)
	requireNativeStatus(t, f.request(t, member, http.MethodGet, path+"?window=1m", nil), http.StatusForbidden)
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: owner.identity.Subject, ProjectID: f.project}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		access string
		scope  string
		want   int
	}{{"all", "admin", http.StatusOK}, {"selected", "admin", http.StatusForbidden}, {"all", "read", http.StatusForbidden}} {
		token, err := apikey.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		id := "metrics-key-" + string(test.access) + "-" + test.scope
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), owner.identity.Subject)
			if err != nil {
				return err
			}
			scope := "operator"
			if test.scope == "admin" {
				scope = "admin"
			}
			expires := time.Now().Add(time.Hour)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: id, Name: id, Hash: apikey.HashToken(token), Scope: scope, NativeOnly: true, CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: &expires, UserID: member.UserID, MembershipID: member.MembershipID, KeyScope: test.scope, ProjectAccess: string(test.access), Kind: "personal"}); err != nil {
				return err
			}
			if test.access == "selected" {
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: id, ProjectID: f.project})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, f.tokenRequest(t, http.MethodGet, path, token, nil), test.want)
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	if err := f.service.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	report, err := f.collector.Report(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, caller := range report.Callers {
		found = found || caller.Kind == "browser" && caller.Caller == "user_owner"
	}
	if !found {
		t.Fatalf("browser attribution=%+v", report)
	}
}

type metricsProvider struct {
	auth.HostedProvider
	memberships map[string]auth.Membership
}

func (p *metricsProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	return identity, nil
}
func (p *metricsProvider) Memberships(_ context.Context, subject, organization string) ([]auth.Membership, error) {
	out := []auth.Membership{}
	for _, m := range p.memberships {
		if (subject == "" || m.UserID == subject) && (organization == "" || m.OrganizationID == organization) {
			out = append(out, m)
		}
	}
	return out, nil
}
func (p *metricsProvider) Organization(context.Context, string) (auth.Organization, error) {
	return auth.Organization{ID: "org_provider", ExternalID: "org_security", Name: "Example"}, nil
}

type metricsHTTPFixture struct {
	service    *hubserver.Service
	collector  *requestmetrics.Service
	repository *postgres.Repository
	project    tracker.ProjectID
	provider   *metricsProvider
	sessions   *auth.Service
}
type metricsHTTPUser struct {
	identity auth.Identity
	token    string
}

func newMetricsHTTPFixture(t *testing.T, admin *sql.DB, repository *postgres.Repository) *metricsHTTPFixture {
	t.Helper()
	now := time.Now()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider", Deployment: "origin", PublicURL: "https://example.test"})
	}); err != nil {
		t.Fatal(err)
	}
	hubtest.Project(t, repository, "org_security", hubtest.ProjectOptions{ID: "prj_example"})
	provider := &metricsProvider{memberships: map[string]auth.Membership{}}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	pg := hubtest.PostgresConfig(t, admin)
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &pg, TenantRepository: repository, AccessRepository: repository, BillingRepository: repository, RunDetails: hubtest.NewDetails(), SecretKeys: keys, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), InitialAdminToken: []byte("example-admin-token-at-least-32-characters"), Hosted: &hostedplans.HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "https://example.test", Provider: provider}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := service.CloseContext(ctx); err != nil {
			t.Error(err)
		}
	})
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: "https://example.test"}, service)
	if err != nil {
		t.Fatal(err)
	}
	collector, err := requestmetrics.New(requestmetrics.Deps{Repository: repository, Organization: "org_security"})
	if err != nil {
		t.Fatal(err)
	}
	return &metricsHTTPFixture{service: service, collector: collector, repository: repository, project: "prj_example", provider: provider, sessions: sessions}
}
func (f *metricsHTTPFixture) user(t *testing.T, name, role, email, grant, support string) metricsHTTPUser {
	t.Helper()
	now := time.Now()
	row := accessstore.Member{OrganizationID: "org_security", UserID: "user_" + name, Email: email, MembershipID: "membership_" + name, Role: role, Active: true, CreatedAt: now, UpdatedAt: now}
	scope := "write"
	if role == "owner" || role == "admin" {
		scope = "admin"
	}
	principal := hubtest.Credential(t, f.repository, "org_security", hubtest.CredentialOptions{Member: &hubtest.SeededMember{Member: row}, KeyScope: scope, AllProject: true, Now: now})
	row.PrincipalID = principal.ID
	if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { return w.CreateAccessMember(t.Context(), row) }); err != nil {
		t.Fatal(err)
	}
	membership := auth.Membership{ID: row.MembershipID, UserID: row.UserID, OrganizationID: "org_provider", Status: "active"}
	membership.Role.Slug = role
	f.provider.memberships[row.UserID] = membership
	identity := auth.Identity{Subject: row.UserID, Email: email, EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: row.UserID, OrganizationID: "org_provider", SessionID: "session_" + name, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	token, _, err := f.sessions.CreateIdentitySession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	return metricsHTTPUser{identity: identity, token: token}
}
func (f *metricsHTTPFixture) request(t *testing.T, user metricsHTTPUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil).WithContext(t.Context())
	r.AddCookie(&http.Cookie{Name: "detent_hosted_session", Value: user.token})
	w := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(w, r)
	return w
}
func (f *metricsHTTPFixture) tokenRequest(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil).WithContext(t.Context())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(w, r)
	return w
}
func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
	}
}
func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}
