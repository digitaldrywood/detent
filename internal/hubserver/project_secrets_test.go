package hubserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const spritesSecretSentinel = "detent-test/org-id/token-id/SPRITES-VALUE-SENTINEL"

type spritesTestTransport func(*http.Request) (*http.Response, error)

func (f spritesTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func secretTestKeys(t *testing.T, active string, versions ...string) *hubsecrets.Keyring {
	t.Helper()
	encoded := map[string]string{}
	for _, v := range versions {
		encoded[v] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte(v), 32))
	}
	raw, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return string(raw)
		}
		return active
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func spritesTestResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestProjectSecretPermissions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		credential hubapi.APICredential
		want       bool
	}{
		{name: "owner", credential: hubapi.APICredential{Hosted: &auth.HostedIdentity{}, HostedRole: "owner"}, want: true},
		{name: "admin", credential: hubapi.APICredential{Hosted: &auth.HostedIdentity{}, HostedRole: "admin"}, want: true},
		{name: "member", credential: hubapi.APICredential{Hosted: &auth.HostedIdentity{}, HostedRole: "member"}},
		{name: "viewer", credential: hubapi.APICredential{Hosted: &auth.HostedIdentity{}, HostedRole: "viewer"}},
		{name: "instance administrator", credential: hubapi.APICredential{Scope: hubapi.APIScopeAdmin}, want: true},
		{name: "scoped administrator", credential: hubapi.APICredential{Scope: hubapi.APIScopeAdmin, NativeOnly: true}},
		{name: "operator", credential: hubapi.APICredential{Scope: hubapi.APIScopeOperator}},
		{name: "worker", credential: hubapi.APICredential{Scope: hubapi.APIScopeWorker}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hubscope.CanManageProjectSecrets(test.credential); got != test.want {
				t.Fatalf("allowed=%t", got)
			}
		})
	}
	f := newBrowserHostedOrganizationFixture(t, true, "org_secret_permissions", func(cfg *Config) {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) { return spritesTestResponse(`{"sprites":[]}`, 200), nil })}
	})
	f.project = f.createProject(t, "Secrets test project")
	base := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/projects/" + f.project + "/secrets/" + flySpritesToken
	t.Run("unconfigured organization pool loads without provider credentials", func(t *testing.T) {
		var pool spritePoolView
		decodeHubResponse(t, f.api(t, "owner", http.MethodGet, "/api/v2/organizations/"+f.service.config.Hosted.OrganizationID+"/sprite-pool", nil, http.StatusOK), &pool)
		if pool.MinRunners != 0 || pool.MaxRunners != 0 || pool.IdleSeconds != 300 || pool.Bootstrap != "" || pool.Revision != 0 || len(pool.Members) != 0 {
			t.Fatalf("unconfigured pool = %+v", pool)
		}
	})
	// Use the existing viewer account with each current provider role. Grants
	// permit writes, so these refusals specifically catch a lost admin boundary.
	f.api(t, "owner", http.MethodPut, "/api/v2/organizations/"+f.service.config.Hosted.OrganizationID+"/members/membership_user_browser_viewer/grants", map[string]any{"project_id": f.project, "write": true, "runner": true}, http.StatusOK)
	for _, role := range []string{"viewer", "member", "admin", "owner"} {
		t.Run("endpoint "+role, func(t *testing.T) {
			f.provider.mu.Lock()
			member := f.provider.members["membership_user_browser_viewer"]
			member.Role.Slug = role
			f.provider.members[member.ID] = member
			f.provider.mu.Unlock()
			if err := updateSecretTestMemberRole(t.Context(), f.service, "user_browser_viewer", role); err != nil {
				t.Fatal(err)
			}
			status := http.StatusForbidden
			if role == "owner" || role == "admin" {
				status = http.StatusOK
			}
			f.api(t, "viewer", http.MethodGet, base, nil, http.StatusOK)
			f.api(t, "viewer", http.MethodPut, base, map[string]any{"token": spritesSecretSentinel}, status)
			f.api(t, "viewer", http.MethodDelete, base, nil, status)
			poolPath := strings.TrimSuffix(base, "/secrets/"+flySpritesToken) + "/sprite-pool"
			var pool spritePoolView
			decodeHubResponse(t, f.api(t, "viewer", http.MethodGet, poolPath, nil, http.StatusOK), &pool)
			f.api(t, "viewer", http.MethodPut, poolPath, spritePoolCommand{Mutation: tracker.Mutation{IdempotencyKey: "project-pool-" + role}, spritePoolSettings: spritePoolSettings{MaxRunners: 2, IdleSeconds: 300, Revision: pool.Revision}, Bootstrap: new("true"), ShareIdleCapacity: new(false)}, status)
			organizationPoolPath := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/sprite-pool"
			decodeHubResponse(t, f.api(t, "viewer", http.MethodGet, organizationPoolPath, nil, http.StatusOK), &pool)
			organizationStatus := status
			if role == "viewer" {
				organizationStatus = http.StatusForbidden
			}
			f.api(t, "viewer", http.MethodPut, organizationPoolPath, spritePoolCommand{Mutation: tracker.Mutation{IdempotencyKey: "organization-pool-" + role}, spritePoolSettings: spritePoolSettings{MaxRunners: 2, IdleSeconds: 300, Revision: pool.Revision}, Bootstrap: new("true"), ShareIdleCapacity: new(false)}, organizationStatus)
			var reloaded spritePoolView
			decodeHubResponse(t, f.api(t, "owner", http.MethodGet, organizationPoolPath, nil, http.StatusOK), &reloaded)
			if status == http.StatusOK && (reloaded.MaxRunners != 2 || reloaded.Bootstrap != "true" || reloaded.Revision != pool.Revision+1) {
				t.Fatalf("saved organization pool = %+v", reloaded)
			}
			if status != http.StatusOK && reloaded.Revision != pool.Revision {
				t.Fatal("forbidden write changed the organization pool")
			}
		})
	}
	t.Run("pool bootstrap is optional and omission preserves saved steps", func(t *testing.T) {
		poolPath := strings.TrimSuffix(base, "/secrets/"+flySpritesToken) + "/sprite-pool"
		for _, test := range []struct {
			name      string
			bootstrap *string
			max       int
			want      string
		}{
			{"enable without bootstrap", new(""), 2, ""},
			{"save extra steps", new("true"), 0, "true"},
			{"omit extra steps", nil, 0, "true"},
		} {
			t.Run(test.name, func(t *testing.T) {
				var pool spritePoolView
				decodeHubResponse(t, f.api(t, "owner", http.MethodGet, poolPath, nil, http.StatusOK), &pool)
				input := map[string]any{"min_runners": 0, "max_runners": test.max, "revision": pool.Revision}
				if test.bootstrap != nil {
					input["bootstrap"] = *test.bootstrap
				}
				decodeHubResponse(t, f.api(t, "owner", http.MethodPut, poolPath, input, http.StatusOK), &pool)
				if pool.Bootstrap != test.want || pool.MaxRunners != test.max {
					t.Fatalf("saved pool = %+v", pool)
				}
			})
		}
	})
	t.Run("older approved project policy keeps saved pool readable", func(t *testing.T) {
		workflow := config.Workflow{Config: config.Default()}
		workflow.Config.Tracker.Kind = config.TrackerHubNative
		descriptor, err := config.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		descriptor.SourceRevision = strings.Repeat("b", 64)
		descriptor = descriptor.WithID()
		organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
		project := tracker.ProjectID(f.project)
		revision, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		revision.ProjectID, revision.ApprovedBy, revision.ApprovedAt = project, "fixture", f.service.config.now()
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			expected := ""
			current, err := w.ProjectPolicy(t.Context(), project)
			if err == nil {
				expected = current.PolicyID
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), revision, expected); err != nil {
				return err
			}
			owner, ok := w.(postgres.SpriteWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			pool, err := owner.SpritePool(t.Context())
			if err != nil {
				return err
			}
			created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "pending-policy-enrollment", RunnerID: "pending-policy-runner", MachineID: "pending-policy-machine", TokenHash: strings.Repeat("c", 64), Scope: "organization", Operations: []string{runnerauth.Read}, CreatedAt: created, ExpiresAt: time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC), CreatedBy: pool.ConfiguredBy}); err != nil {
				return err
			}
			return owner.CreateRunnerSpriteMembership(t.Context(), runnerstore.SpriteMembership{OrganizationID: organization, Name: "pending-policy-sprite", ProviderOrganization: "org", EnrollmentID: "pending-policy-enrollment", State: "deleted", IdleSince: created, CreatedAt: created})
		}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/sprite-pool", strings.TrimSuffix(base, "/secrets/"+flySpritesToken) + "/sprite-pool"} {
			var pool spritePoolView
			decodeHubResponse(t, f.api(t, "owner", http.MethodGet, path, nil, http.StatusOK), &pool)
			if pool.Bootstrap != "true" || pool.Revision == 0 || len(pool.Members) != 1 || pool.Members[0].Name != "pending-policy-sprite" || pool.Decision == nil || strings.Contains(strings.ToLower(pool.Decision.Reason), "approval") || pool.RunnerSetupDeclared != nil {
				t.Fatalf("%s pool = %+v decision = %+v", path, pool, pool.Decision)
			}
		}
	})
	f.api(t, "owner", http.MethodPut, strings.Replace(base, f.project, "prj_other", 1), map[string]any{"token": spritesSecretSentinel}, http.StatusNotFound)
	f.api(t, "wrong-organization", http.MethodPut, base, map[string]any{"token": spritesSecretSentinel}, http.StatusForbidden)
	t.Run("authority removed during validation", func(t *testing.T) {
		f.service.config.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			f.provider.mu.Lock()
			member := f.provider.members["membership_user_browser_viewer"]
			member.Role.Slug = "member"
			f.provider.members[member.ID] = member
			f.provider.mu.Unlock()
			if err := updateSecretTestMemberRole(t.Context(), f.service, "user_browser_viewer", "member"); err != nil {
				t.Fatal(err)
			}
			return spritesTestResponse(`{"sprites":[]}`, 200), nil
		})}
		f.api(t, "viewer", http.MethodPut, base, map[string]any{"token": spritesSecretSentinel}, http.StatusForbidden)
		var state projectSecretStatus
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base, nil, http.StatusOK), &state)
		if state.Present {
			t.Fatal("credential stored after authority was removed")
		}
	})

}

func TestProjectSecretLifecycle(t *testing.T) {
	t.Parallel()
	var logs hostedLogBuffer
	reject := false
	f := newBrowserHostedOrganizationFixture(t, true, "org_secret_lifecycle", func(cfg *Config) {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		cfg.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			if reject {
				return nil, errors.New(spritesSecretSentinel)
			}
			return spritesTestResponse(`{"sprites":[]}`, 200), nil
		})}
	})
	// Exercise the Echo request logger as well as the service error logger. Its
	// record uses route templates and status, never request bodies or headers.
	f.service.echo.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{LogRoutePath: true, LogStatus: true, LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
		f.service.hostedAuthLogger.Info("request", "route", v.RoutePath, "status", v.Status)
		return nil
	}}))
	f.project = f.createProject(t, "Secrets test project")
	base := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/projects/" + f.project + "/secrets/" + flySpritesToken
	for _, step := range []struct {
		name, method string
		body         any
		present      bool
		status       int
	}{
		{name: "unset", method: http.MethodGet, status: 200},
		{name: "set", method: http.MethodPut, body: map[string]any{"token": spritesSecretSentinel}, present: true, status: 200},
		{name: "metadata", method: http.MethodGet, present: true, status: 200},
		{name: "replace", method: http.MethodPut, body: map[string]any{"token": spritesSecretSentinel + "-replacement"}, present: true, status: 200},
		{name: "provider failure", method: http.MethodPut, body: map[string]any{"token": spritesSecretSentinel}, present: true, status: 422},
		{name: "remove", method: http.MethodDelete, status: 200},
		{name: "removed", method: http.MethodGet, status: 200},
	} {
		t.Run(step.name, func(t *testing.T) {
			if step.name == "provider failure" {
				reject = true
			}
			response := f.api(t, "owner", step.method, base, step.body, step.status)
			if strings.Contains(response.Body.String(), "SPRITES-VALUE-SENTINEL") {
				t.Fatal("API exposed token")
			}
			if step.status == 200 {
				var state projectSecretStatus
				browserHostedDecode(t, response, &state)
				if state.Present != step.present || state.Present && (state.OrganizationSlug != "detent-test" || state.KeyVersion != 1) {
					t.Fatalf("status=%+v", state)
				}
			}
			if step.name == "metadata" {
				// The project tool reuses the same authenticated metadata read, never
				// the encrypted envelope or the provider credential setup payload.
				captureds := make(chan context.Context, 1)
				f.service.echo.GET("/api/v2/organizations/:organization/secret-tool-fixture", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
				f.api(t, "owner", http.MethodGet, "/api/v2/organizations/org_secret_lifecycle/secret-tool-fixture", nil, http.StatusOK)
				captured := <-captureds
				result, err := (hubProjectExecutor{f.service}).Execute(captured, operatortool.Call{Name: "project_secret_metadata", Arguments: json.RawMessage(`{"project_id":"` + f.project + `"}`)})
				if err != nil || !strings.Contains(string(result.Content), `"present":true`) || strings.Contains(string(result.Content), "SPRITES-VALUE-SENTINEL") || strings.Contains(string(result.Content), "ciphertext") {
					t.Fatalf("tool metadata=%s %v", result.Content, err)
				}

				scope := hubscope.NativeScope{Organization: tracker.OrganizationID("org_secret_lifecycle"), Project: tracker.ProjectID(f.project), Credential: hubapi.APICredential{ID: "internal-provisioner"}}
				if slug, err := f.service.checkProjectSprites(t.Context(), scope); err != nil || slug != "detent-test" {
					t.Fatalf("internal use=%s %v", slug, err)
				}
				var rows []accessstore.Secret
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					rows, err = r.AccessSecrets(t.Context(), "", "", corestore.MaxPageSize)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				projectRows := rows[:0]
				for _, row := range rows {
					if row.ProjectID == scope.Project && row.Kind == flySpritesToken {
						projectRows = append(projectRows, row)
					}
				}
				rows = projectRows
				if len(rows) != 1 || bytes.Contains(rows[0].Envelope.Ciphertext, []byte(spritesSecretSentinel)) {
					t.Fatal("stored plaintext")
				}
			}
		})
	}
	rows, err := hostedPostgresAdmin(t, f.service).QueryContext(t.Context(), "SELECT actor,project_id,kind,key_version,event FROM project_secret_audit WHERE organization_id=$1 ORDER BY id", f.service.config.Hosted.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var actor, project, kind, event string
		var version int
		if err := rows.Scan(&actor, &project, &kind, &version, &event); err != nil {
			t.Fatal(err)
		}
		if actor == "" || project != f.project || kind != flySpritesToken || version != 1 {
			t.Fatal("incomplete audit identity")
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "use,set,use,use,replace,use,remove" {
		t.Fatalf("events=%v", events)
	}
	if !strings.Contains(logs.String(), `"msg":"request"`) {
		t.Fatal("request logger was not exercised")
	}
	if strings.Contains(logs.String(), "SPRITES-VALUE-SENTINEL") {
		t.Fatal("request/error log exposed token")
	}
	// Invalid decoder fields can carry the value too; never echo decode errors.
	raw := httptest.NewRequest(http.MethodPut, f.server.URL+base, strings.NewReader(`{"SPRITES-VALUE-SENTINEL":true}`))
	raw.Header.Set("Content-Type", "application/json")
	raw.Header.Set("Origin", f.server.URL)
	owner := f.cookies["owner"]
	if owner == nil {
		t.Fatal("missing owner session cookie")
	}
	raw.Header.Set("X-CSRF-Token", hostedCSRF(owner.Value))
	raw.AddCookie(owner)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, raw)
	if response.Code != 422 || strings.Contains(response.Body.String()+logs.String(), "SPRITES-VALUE-SENTINEL") {
		t.Fatal("decode error leaked value")
	}
}

func TestProjectSecretStartupKeys(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, openTestService(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), Hosted: &hostedplans.HostedConfig{OrganizationID: "org_secret_startup", WorkOSOrganizationID: "provider_secret_startup", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}}), "", "Secret startup")
	now := f.service.config.now()
	envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD(f.project.OrganizationID, f.project.ID, flySpritesToken))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Kind: flySpritesToken, OrganizationSlug: "detent-test", Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Kind: flySpritesToken, Actor: "fixture", Event: "set", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	admin := hostedPostgresAdmin(t, f.service)
	cfg := f.service.config
	cfg.TenantRepository, cfg.AccessRepository, cfg.BillingRepository = nil, nil, nil
	cfg.NativeRunEvents, cfg.ProjectParticipants, cfg.ConversationDispatch, cfg.ChangeVersionAuthority = nil, nil, nil, nil
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		keys *hubsecrets.Keyring
		want bool
	}{
		{name: "missing keys"},
		{name: "missing old version", keys: secretTestKeys(t, "2", "2")},
		{name: "correct version", keys: secretTestKeys(t, "1", "1"), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg.SecretKeys = test.keys
			service, err := Open(t.Context(), cfg)
			if (err == nil) != test.want {
				t.Fatalf("startup error=%v", err)
			}
			if service != nil {
				if err := service.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if _, err := admin.ExecContext(t.Context(), "UPDATE project_secrets SET wrapped_data_key=$4 WHERE organization_id=$1 AND project_id=$2 AND kind=$3", f.project.OrganizationID, f.project.ID, flySpritesToken, []byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if service, err := Open(t.Context(), cfg); err == nil {
		service.Close()
		t.Fatal("startup accepted a tampered data key")
	}
}

func updateSecretTestMemberRole(ctx context.Context, service *Service, user, role string) error {
	organization := tracker.OrganizationID(service.config.Hosted.OrganizationID)
	return service.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(ctx, user)
		if err != nil {
			return err
		}
		member.Role, member.UpdatedAt = role, service.config.now()
		return w.UpdateAccessMember(ctx, member)
	})
}
