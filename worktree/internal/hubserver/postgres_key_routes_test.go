package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresKeyRoutesPreserveTenantAndCurrentAuthority(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(38)
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	entryRevoked := false
	cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
		if entryRevoked {
			return cloudassert.Claims{}, operatortool.ErrAccessDenied
		}
		return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: "org_startup", ProviderOrganization: "provider_org_startup", Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		seedStartupOrganization(t, store.postgres, org)
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x35}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
				return err
			}
			for _, user := range []string{"first", "second"} {
				id := "credential_" + user
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: user, Hash: apikey.HashToken(string(org) + user), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: user, MembershipID: "membership_" + user, KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: "owner", Active: true, PrincipalID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			project := tracker.ProjectID("project_" + string(org))
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "credential_first", ProjectID: project}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "first", ProjectID: project, CanWrite: true, ManageRunner: true})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	request := func(path, user, connection string, arguments any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		claims := cloudassert.Claims{Issuer: "entry", Audience: "org_startup", ProviderOrganization: "provider_org_startup", Subject: user, Role: "owner", Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: time.Now(), ExpiresAt: time.Now().Add(20 * time.Second), ID: id, Key: &apikey.KeyAuthority{ID: "key_" + user, CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}
		if mutate != nil {
			mutate(&claims)
		}
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(cloudassert.Header, assertion)
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, req)
		return response
	}
	catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
	requireNativeStatus(t, catalog, http.StatusOK)
	var definitions []operatortool.Definition
	decodeHubResponse(t, catalog, &definitions)
	found := false
	for _, definition := range definitions {
		found = found || definition.Name == "list_projects"
		if !definition.Annotations.ReadOnly {
			t.Fatalf("unsupported mutation exposed: %s", definition.Name)
		}
	}
	if !found {
		t.Fatal("project catalog absent")
	}
	call := func(user, connection, name string, args any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		return request("/internal/v1/keys/call", user, connection, map[string]any{"name": name, "arguments": args, "connection_id": connection}, mutate)
	}
	listed := call("first", "connection", "list_projects", map[string]any{}, nil)
	requireNativeStatus(t, listed, http.StatusOK)
	if !strings.Contains(listed.Body.String(), "project_org_startup") || strings.Contains(listed.Body.String(), "project_org_other") {
		t.Fatalf("tenant project projection incorrect: %s", listed.Body.String())
	}
	requireNativeStatus(t, call("first", "connection", "list_projects", map[string]any{}, func(c *cloudassert.Claims) { c.Key.CredentialBinding = strings.Repeat("b", 64) }), http.StatusForbidden)
	entryRevoked = true
	requireNativeStatus(t, call("first", "connection", "list_projects", map[string]any{}, nil), http.StatusForbidden)
	entryRevoked = false
	requireNativeStatus(t, call("first", "connection", operatortool.WorkList, map[string]any{"project_id": "project_org_startup"}, nil), http.StatusOK)
	requireNativeStatus(t, call("first", "blocked", "get_native_project", map[string]any{"project_id": "project_org_startup"}, func(c *cloudassert.Claims) { c.Key.BlockedProjects = []string{"project_org_startup"} }), http.StatusForbidden)
	requireNativeStatus(t, call("second", "connection", "list_projects", map[string]any{}, nil), http.StatusForbidden)
	requireNativeStatus(t, call("first", "other", "get_native_project", map[string]any{"project_id": "project_org_other"}, nil), http.StatusForbidden)
	requireNativeStatus(t, call("first", "other", "list_projects", map[string]any{}, func(c *cloudassert.Claims) { c.Audience = "org_other" }), http.StatusUnauthorized)
	requireNativeStatus(t, call("first", "other", operatortool.BillingCheckout, map[string]any{}, nil), http.StatusForbidden)
	memberCredential := apiCredential{Hosted: &auth.HostedIdentity{Subject: "first"}, HostedRole: "member"}
	if err := service.requireHostedRunnerProjects(t.Context(), nil, memberCredential, nil); err != nil {
		t.Fatalf("member manage_runner grant refused: %v", err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), "first")
		if err != nil {
			return err
		}
		member.Role = "member"
		if err := w.UpdateAccessMember(t.Context(), member); err != nil {
			return err
		}
		return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "first", ProjectID: "project_org_startup", CanWrite: true, ManageRunner: false})
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.requireHostedRunnerProjects(t.Context(), nil, memberCredential, nil); err == nil {
		t.Fatal("member runner administration retained after grant revocation")
	}
	changed := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
	requireNativeStatus(t, changed, http.StatusOK)
	decodeHubResponse(t, changed, &definitions)
	for _, definition := range definitions {
		if definition.Name == operatortool.ListRunnerRouting {
			t.Fatal("runner tool retained after manage_runner revocation")
		}
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_first", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, call("first", "connection", "list_projects", map[string]any{}, nil), http.StatusForbidden)
}
