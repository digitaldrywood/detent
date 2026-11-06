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
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/releasenotes"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeyBootstrapRead(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(38)
	cfg := startupConfig(connection)
	plans := pilotHostedPlans()
	cfg.Hosted.Plans = &plans
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	currentOrganization := "org_startup"
	selected := false
	entryRevoked := false
	currentRole := "member"
	currentPermission := apikey.ScopeRead
	var currentBlocked []string
	cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
		if entryRevoked {
			return cloudassert.Claims{}, operatortool.ErrAccessDenied
		}
		selection := apikey.ProjectContext{Access: "all"}
		if selected {
			selection = apikey.ProjectContext{Access: "selected", Projects: []string{"hidden_project"}}
		}
		return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: currentOrganization, ProviderOrganization: "provider_" + currentOrganization, Subject: origin.Subject, Role: currentRole, Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: currentPermission, BlockedProjects: currentBlocked, ProjectContext: selection}}, nil
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
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: "member", Active: true, PrincipalID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "hidden_project", Name: "Private example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			project := tracker.ProjectID("project_" + string(org))
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "credential_first", ProjectID: project}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "first", ProjectID: project, CanWrite: true, ManageRunner: false})
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
	firstService := service
	service.releaseNotes = []releasenotes.Build{{Version: "v1.0.1", BuildTime: now}, {Version: "v1.0.0", BuildTime: now.Add(-time.Hour)}}
	if err := service.config.AccessRepository.(accountstore.Repository).SetReleasePreference(t.Context(), "org_startup", "first", "membership_first", "credential_first", accountstore.ReleasePreference{Version: "v1.0.0", BuildTime: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := firstService.Close(); err != nil {
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
		claims := cloudassert.Claims{Issuer: "entry", Audience: currentOrganization, ProviderOrganization: "provider_" + currentOrganization, Subject: user, Role: "owner", Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: time.Now(), ExpiresAt: time.Now().Add(20 * time.Second), ID: id, Key: &apikey.KeyAuthority{ID: "key_" + user, CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: currentPermission, ProjectContext: apikey.ProjectContext{Access: "all"}}}
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
	call := func(name string, args any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		return request("/internal/v1/keys/call", "first", "bootstrap_read", map[string]any{"name": name, "arguments": args, "connection_id": "bootstrap_read"}, mutate)
	}
	catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
	requireNativeStatus(t, catalog, http.StatusOK)
	var definitions []operatortool.Definition
	decodeHubResponse(t, catalog, &definitions)
	found := false
	for _, definition := range definitions {
		if definition.Name == operatortool.AppBootstrapPayload {
			found = definition.Annotations.ReadOnly
		}
		if definition.Name == operatortool.AppUpdates {
			t.Error("read key without runner grants advertised updates")
		}
	}
	if !found {
		t.Error("bootstrap read missing from signed catalog")
	}
	response := call(operatortool.AppBootstrapPayload, map[string]any{}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap call status=%d", response.Code)
	}
	check := func(response *httptest.ResponseRecorder, role string, count int) {
		t.Helper()
		requireNativeStatus(t, response, http.StatusOK)
		var payload appBootstrap
		decodeHubResponse(t, response, &payload)
		if payload.Organization.ID != currentOrganization || payload.Organization.Slug != "example-"+strings.TrimPrefix(currentOrganization, "org_") || payload.Actor.Role != role || payload.Actor.Email != "first@example.test" || payload.Plan == nil || payload.Plan.ID != plans.Base.ID || payload.Plan.Source != "base" || payload.Plan.WindowEndsAt == "" {
			t.Fatal("bootstrap identity or canonical plan missing")
		}
		if len(payload.Projects) != count || len(payload.Organizations) != 0 || payload.CSRFToken != "" || payload.Actor.CanManageRunners {
			t.Fatal("bootstrap project visibility or private context incorrect")
		}
		for _, project := range payload.Projects {
			if project.ID != "project_"+currentOrganization || project.CanWrite || project.CanManageRunners {
				t.Fatal("bootstrap leaked project or key capabilities")
			}
		}
	}
	check(response, "member", 1)
	var unread appBootstrap
	decodeHubResponse(t, response, &unread)
	if !unread.HasUnread {
		t.Fatal("bootstrap lost current release preference")
	}
	selected = true
	check(call(operatortool.AppBootstrapPayload, map[string]any{}, nil), "member", 0)
	selected = false
	currentOrganization = "org_other"
	otherConfig := cfg
	otherHosted := *cfg.Hosted
	otherEntry := *cfg.Hosted.SharedEntry
	otherHosted.SharedEntry = &otherEntry
	otherConfig.Hosted = &otherHosted
	otherConfig.Hosted.OrganizationID = currentOrganization
	otherConfig.Hosted.WorkOSOrganizationID = "provider_" + currentOrganization
	otherService, err := Open(t.Context(), otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := otherService.Close(); err != nil {
			t.Error(err)
		}
	})
	service = otherService
	check(call(operatortool.AppBootstrapPayload, map[string]any{}, nil), "member", 1)
	otherCatalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
	requireNativeStatus(t, otherCatalog, http.StatusOK)
	var otherDefinitions []operatortool.Definition
	decodeHubResponse(t, otherCatalog, &otherDefinitions)
	otherFound := false
	for _, definition := range otherDefinitions {
		otherFound = otherFound || definition.Name == operatortool.AppBootstrapPayload
	}
	if !otherFound {
		t.Fatal("second tenant bootstrap catalog unavailable")
	}
	service = firstService
	currentOrganization = "org_startup"
	selected = false
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), "first")
		if err != nil {
			return err
		}
		member.Role = "viewer"
		member.UpdatedAt = time.Now()
		return w.UpdateAccessMember(t.Context(), member)
	}); err != nil {
		t.Fatal(err)
	}
	check(call(operatortool.AppBootstrapPayload, map[string]any{}, nil), "viewer", 1)
	currentBlocked = []string{"project_org_startup"}
	check(call(operatortool.AppBootstrapPayload, map[string]any{}, nil), "viewer", 0)
	currentBlocked = nil
	for _, test := range []struct {
		name   string
		args   any
		mutate func(*cloudassert.Claims)
	}{
		{"malformed", map[string]any{"organization_id": "org_other"}, nil},
		{"invalid scope", map[string]any{}, func(c *cloudassert.Claims) { c.Key.Permission = apikey.Scope("invalid") }},
		{"cross organization", map[string]any{}, func(c *cloudassert.Claims) { c.Audience = "org_other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if call(operatortool.AppBootstrapPayload, test.args, test.mutate).Code == http.StatusOK {
				t.Fatal("invalid bootstrap request accepted")
			}
		})
	}
	unsigned := httptest.NewRecorder()
	service.Handler().ServeHTTP(unsigned, httptest.NewRequest(http.MethodPost, "/internal/v1/keys/call", strings.NewReader(`{}`)))
	if unsigned.Code == http.StatusOK {
		t.Fatal("unsigned request accepted")
	}
	entryRevoked = true
	if call(operatortool.AppBootstrapPayload, map[string]any{}, nil).Code == http.StatusOK {
		t.Fatal("revoked key accepted")
	}
	entryRevoked = false
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "first", "project_org_startup")
	}); err != nil {
		t.Fatal(err)
	}
	check(call(operatortool.AppBootstrapPayload, map[string]any{}, nil), "viewer", 0)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), "first", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if call(operatortool.AppBootstrapPayload, map[string]any{}, nil).Code == http.StatusOK {
		t.Fatal("revoked member accepted")
	}
}
