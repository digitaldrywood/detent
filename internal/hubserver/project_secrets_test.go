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

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/operatortool"
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

func TestSpritesTokenValidation(t *testing.T) {
	for _, test := range []struct {
		name, token, body string
		status            int
		transportError    bool
		valid             bool
	}{
		{name: "current response", body: `{"sprites":[{"organization":"detent-test"}]}`, status: 200, valid: true},
		{name: "legacy response", body: `{"sprites":[{"org_slug":"detent-test"}]}`, status: 200, valid: true},
		{name: "empty organization", body: `{"sprites":[]}`, status: 200, valid: true},
		{name: "invalid token", body: spritesSecretSentinel, status: 401},
		{name: "organization billing required", body: spritesSecretSentinel, status: 402},
		{name: "provider unavailable", body: spritesSecretSentinel, status: 503},
		{name: "provider error contains value", transportError: true},
		{name: "bad response contains value", body: spritesSecretSentinel, status: 200},
		{name: "missing sprites", body: `{}`, status: 200},
		{name: "missing organization", body: `{"sprites":[{}]}`, status: 200},
		{name: "different organization", body: `{"sprites":[{"organization":"other"}]}`, status: 200},
		{name: "conflicting fields", body: `{"sprites":[{"org_slug":"other","organization":"detent-test"}]}`, status: 200},
		{name: "trailing json", body: `{"sprites":[]} {}`, status: 200},
		{name: "header injection", token: "detent-test/org/id/value\nInjected: value", status: 200},
		{name: "invalid format", token: "SPRITES-VALUE-SENTINEL", status: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			token := test.token
			if token == "" {
				token = spritesSecretSentinel
			}
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.String() != "https://api.sprites.dev/v1/sprites" || request.Header.Get("Authorization") != "Bearer "+token {
					t.Fatal("incorrect validation request")
				}
				if test.transportError {
					return nil, errors.New(spritesSecretSentinel)
				}
				return spritesTestResponse(test.body, test.status), nil
			})}
			slug, err := validateSpritesToken(t.Context(), client, []byte(token))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v", test.valid, err)
			}
			if test.valid && slug != "detent-test" {
				t.Fatal("incorrect organization binding")
			}
			if err != nil && (strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "SPRITES-VALUE-SENTINEL")) {
				t.Fatal("error exposes token")
			}
		})
	}
	t.Run("redirect refused", func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			r := spritesTestResponse("", 302)
			r.Header.Set("Location", "https://api.sprites.dev/leak")
			return r, nil
		})}
		if _, err := validateSpritesToken(t.Context(), client, []byte(spritesSecretSentinel)); err == nil || calls != 1 {
			t.Fatal("validation followed a redirect")
		}
	})
}

func TestProjectSecretPermissions(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential apiCredential
		want       bool
	}{
		{name: "owner", credential: apiCredential{Hosted: &auth.HostedIdentity{}, HostedRole: "owner"}, want: true},
		{name: "admin", credential: apiCredential{Hosted: &auth.HostedIdentity{}, HostedRole: "admin"}, want: true},
		{name: "member", credential: apiCredential{Hosted: &auth.HostedIdentity{}, HostedRole: "member"}},
		{name: "viewer", credential: apiCredential{Hosted: &auth.HostedIdentity{}, HostedRole: "viewer"}},
		{name: "instance administrator", credential: apiCredential{Scope: apiScopeAdmin}, want: true},
		{name: "scoped administrator", credential: apiCredential{Scope: apiScopeAdmin, NativeOnly: true}},
		{name: "operator", credential: apiCredential{Scope: apiScopeOperator}},
		{name: "worker", credential: apiCredential{Scope: apiScopeWorker}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := canManageProjectSecrets(test.credential); got != test.want {
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
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role=? WHERE user_id=?", role, "user_browser_viewer"); err != nil {
				t.Fatal(err)
			}
			status := http.StatusForbidden
			if role == "viewer" {
				status = http.StatusNotFound
			}
			if role == "owner" || role == "admin" {
				status = http.StatusOK
			}
			f.api(t, "viewer", http.MethodGet, base, nil, http.StatusOK)
			f.api(t, "viewer", http.MethodPut, base, map[string]any{"token": spritesSecretSentinel}, status)
			f.api(t, "viewer", http.MethodDelete, base, nil, status)
			poolPath := strings.TrimSuffix(base, "/secrets/"+flySpritesToken) + "/sprite-pool"
			var pool spritePoolView
			decodeHubResponse(t, f.api(t, "viewer", http.MethodGet, poolPath, nil, http.StatusOK), &pool)
			f.api(t, "viewer", http.MethodPut, poolPath, spritePoolSettings{MaxRunners: 2, IdleSeconds: 300, Bootstrap: "true", Revision: pool.Revision}, status)
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
	f.api(t, "owner", http.MethodPut, strings.Replace(base, f.project, "prj_other", 1), map[string]any{"token": spritesSecretSentinel}, http.StatusNotFound)
	f.api(t, "wrong-organization", http.MethodPut, base, map[string]any{"token": spritesSecretSentinel}, http.StatusForbidden)
	t.Run("authority removed during validation", func(t *testing.T) {
		f.service.config.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			f.provider.mu.Lock()
			member := f.provider.members["membership_user_browser_viewer"]
			member.Role.Slug = "member"
			f.provider.members[member.ID] = member
			f.provider.mu.Unlock()
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role='member' WHERE user_id='user_browser_viewer'"); err != nil {
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

				scope := nativeScope{organization: tracker.OrganizationID("org_secret_lifecycle"), project: tracker.ProjectID(f.project), credential: apiCredential{ID: "internal-provisioner"}}
				if slug, err := f.service.checkProjectSprites(t.Context(), scope); err != nil || slug != "detent-test" {
					t.Fatalf("internal use=%s %v", slug, err)
				}
				rows, err := readSecretRows(t.Context(), f.service.database.db)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || bytes.Contains(rows[0].envelope.Ciphertext, []byte(spritesSecretSentinel)) {
					t.Fatal("stored plaintext")
				}
			}
		})
	}
	rows, err := f.service.database.db.QueryContext(t.Context(), "SELECT actor,project_id,kind,key_version,event FROM project_secret_audit ORDER BY id")
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

func TestProjectSecretStartupAndRotation(t *testing.T) {
	f := newBrowserHostedOrganizationFixture(t, true, "org_secret_rotation", func(cfg *Config) {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) { return spritesTestResponse(`{"sprites":[]}`, 200), nil })}
	})
	f.project = f.createProject(t, "Secrets test project")
	base := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/projects/" + f.project + "/secrets/" + flySpritesToken
	f.api(t, "owner", http.MethodPut, base, map[string]any{"token": spritesSecretSentinel}, http.StatusOK)
	f.api(t, "owner", http.MethodPut, strings.Replace(base, f.project, f.privateProject, 1), map[string]any{"token": spritesSecretSentinel}, http.StatusOK)
	original, err := readSecretRows(t.Context(), f.service.database.db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.service.config
	if _, err := RotateProjectSecrets(t.Context(), cfg, "operator:test"); err == nil {
		t.Fatal("rotation ignored running Hub ownership")
	}
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
	cfg.SecretKeys = secretTestKeys(t, "2", "1", "2")
	// A bad later row must roll back the earlier row's wrapping and audit.
	db, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	bad := original[1]
	if _, err := db.db.ExecContext(t.Context(), "UPDATE project_secrets SET wrapped_data_key=? WHERE project_id=?", []byte("tampered"), bad.project); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateProjectSecrets(t.Context(), cfg, "operator:test"); err == nil {
		t.Fatal("rotation accepted bad data key")
	}
	db, err = openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := readSecretRows(t.Context(), db.db)
	if err != nil {
		t.Fatal(err)
	}
	if failed[0].envelope.Version != 1 || !bytes.Equal(failed[0].envelope.WrappedKey, original[0].envelope.WrappedKey) {
		t.Fatal("failed rotation partially committed")
	}
	var auditCount int
	if err := db.db.QueryRowContext(t.Context(), "SELECT count(*) FROM project_secret_audit WHERE event='rotate'").Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 {
		t.Fatal("failed rotation committed audit")
	}
	if _, err := db.db.ExecContext(t.Context(), "UPDATE project_secrets SET wrapped_data_key=? WHERE project_id=?", bad.envelope.WrappedKey, bad.project); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	count, err := RotateProjectSecrets(t.Context(), cfg, "operator:test")
	if err != nil || count != 2 {
		t.Fatalf("rotate count=%d error=%v", count, err)
	}
	count, err = RotateProjectSecrets(t.Context(), cfg, "operator:test")
	if err != nil || count != 0 {
		t.Fatalf("repeat rotation=%d %v", count, err)
	}
	cfg.SecretKeys = secretTestKeys(t, "2", "2")
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	rotated, err := readSecretRows(t.Context(), service.database.db)
	if err != nil {
		t.Fatal(err)
	}
	if rotated[0].envelope.Version != 2 || !bytes.Equal(rotated[0].envelope.Ciphertext, original[0].envelope.Ciphertext) || !bytes.Equal(rotated[0].envelope.Nonce, original[0].envelope.Nonce) {
		t.Fatal("rotation reencrypted value")
	}
	value, err := cfg.SecretKeys.Open(rotated[0].envelope, secretAAD(rotated[0].organization, rotated[0].project, flySpritesToken))
	if err != nil || string(value) != spritesSecretSentinel {
		t.Fatalf("rotated secret unavailable: %v", err)
	}
	clear(value)
	var actor, event string
	var version int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT actor,event,key_version FROM project_secret_audit ORDER BY id DESC LIMIT 1").Scan(&actor, &event, &version); err != nil {
		t.Fatal(err)
	}
	if actor != "operator:test" || event != "rotate" || version != 2 {
		t.Fatal("rotation audit incorrect")
	}
}
