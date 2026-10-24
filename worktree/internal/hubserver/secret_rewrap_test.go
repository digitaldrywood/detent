package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func secretRewrapFixture(t *testing.T) (nativeFixture, []projectSecretRow) {
	t.Helper()
	f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	f.service.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	<-f.service.secretRewrapDone
	secondOrg := tracker.OrganizationID("org_rewrap_second")
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)`, secondOrg, "Second organization", formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	second := newNativeFixture(t, f.service, secondOrg, "second")
	for _, fixture := range []nativeFixture{f, second} {
		storeSpriteWakeSecret(t, fixture, spritesSecretSentinel)
		storeOrganizationSpritesSecret(t, fixture, spritesSecretSentinel)
	}
	rows, err := readSecretRows(t.Context(), f.service.database.reader)
	if err != nil || len(rows) != 4 {
		t.Fatalf("secret fixture rows=%d error=%v", len(rows), err)
	}
	return f, rows
}

func assertSecretRewrap(t *testing.T, s *Service, original []projectSecretRow, version, audits int) {
	t.Helper()
	rows, err := readSecretRows(t.Context(), s.database.reader)
	if err != nil || len(rows) != len(original) {
		t.Fatalf("rewrapped rows=%d error=%v", len(rows), err)
	}
	for i, row := range rows {
		before := original[i]
		if row.envelope.Version != version || !bytes.Equal(row.envelope.Ciphertext, before.envelope.Ciphertext) || !bytes.Equal(row.envelope.Nonce, before.envelope.Nonce) || row.updatedAt != before.updatedAt {
			t.Fatalf("rewrap changed value or metadata for %s/%s/%s", row.organization, row.project, row.kind)
		}
		value, err := s.config.SecretKeys.Open(row.envelope, secretAAD(row.organization, row.project, row.kind))
		if err != nil || string(value) != spritesSecretSentinel {
			t.Fatalf("secret plaintext changed: %v", err)
		}
		clear(value)
		table, predicate, args := "organization_secret_audit", "organization_id=? AND kind=?", []any{row.organization, row.kind}
		if row.project != "" {
			table, predicate, args = "project_secret_audit", "organization_id=? AND project_id=? AND kind=?", []any{row.organization, row.project, row.kind}
		}
		var count int
		if err := s.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+predicate+" AND event='rotate' AND actor='system:rewrap'", args...).Scan(&count); err != nil || count != audits {
			t.Fatalf("rotation audits=%d want=%d error=%v", count, audits, err)
		}
		var auditVersion int
		if audits > 0 {
			if err := s.database.reader.QueryRowContext(t.Context(), "SELECT key_version FROM "+table+" WHERE "+predicate+" AND event='rotate' ORDER BY id DESC LIMIT 1", args...).Scan(&auditVersion); err != nil || auditVersion != version {
				t.Fatalf("audit version=%d want=%d error=%v", auditVersion, version, err)
			}
		}
	}
}

func TestSecretRewrapRestart(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			f, original := secretRewrapFixture(t)
			f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			if partial {
				if err := f.service.rewrapSecret(t.Context(), original[0]); err != nil {
					t.Fatal(err)
				}
			}
			cfg := f.service.config
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			for i, active := range []string{"2", "2", "1"} {
				cfg.SecretKeys = secretTestKeys(t, active, "1", "2")
				s, err := Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				var writer *sql.Conn
				if i == 1 {
					writer, err = s.database.db.Conn(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { writer.Close() })
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				served := make(chan error, 1)
				go func() { served <- s.Serve(listener) }()
				<-s.secretRewrapDone
				version, audits := 2, 1
				if i == 2 {
					version, audits = 1, 2
				}
				assertSecretRewrap(t, s, original, version, audits)
				if writer != nil {
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if err := <-served; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSecretRewrapCAS(t *testing.T) {
	for _, mutation := range []string{"replace", "replace same version", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			f, original := secretRewrapFixture(t)
			f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			f.service.config.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) { return spritesTestResponse(`{"sprites":[]}`, 200), nil })}
			for _, row := range original {
				fixture := f
				fixture.project.OrganizationID, fixture.project.ID = tracker.OrganizationID(row.organization), tracker.ProjectID(row.project)
				if strings.HasPrefix(mutation, "replace") {
					if mutation == "replace same version" {
						f.service.config.SecretKeys = secretTestKeys(t, "1", "1", "2")
					}
					request := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"token":"`+spritesSecretSentinel+`-replacement"}`))
					request.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					c := f.service.echo.NewContext(request, response)
					c.SetParamNames("kind")
					c.SetParamValues(row.kind)
					c.Set("native_scope", nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID, credential: apiCredential{Scope: apiScopeAdmin, ID: "test-admin"}})
					if err := f.service.setProjectSecret(c); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, response, http.StatusOK)
					f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
				} else {
					if err := f.service.secretMutation(t.Context(), nativeScope{organization: fixture.project.OrganizationID, credential: apiCredential{Scope: apiScopeAdmin}}, func(tx *sql.Tx) error {
						return removeProjectSecretInTx(t.Context(), tx, nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}, f.service.config.now())
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.service.rewrapSecret(t.Context(), row); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := readSecretRows(t.Context(), f.service.database.reader)
			if err != nil || (mutation == "delete" && len(rows) != 0) || (strings.HasPrefix(mutation, "replace") && len(rows) != 4) {
				t.Fatalf("rows=%d error=%v", len(rows), err)
			}
			for _, row := range rows {
				wantVersion := 2
				if mutation == "replace same version" {
					wantVersion = 1
				}
				value, err := f.service.config.SecretKeys.Open(row.envelope, secretAAD(row.organization, row.project, row.kind))
				if err != nil || string(value) != spritesSecretSentinel+"-replacement" || row.envelope.Version != wantVersion {
					t.Fatalf("concurrent replacement lost: %v", err)
				}
				clear(value)
			}
			for _, table := range []string{"project_secret_audit", "organization_secret_audit"} {
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE event='rotate'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("stale rewrap audit count=%d error=%v", count, err)
				}
			}
		})
	}
}

func TestSecretRewrapServingAndShutdown(t *testing.T) {
	f, _ := secretRewrapFixture(t)
	cfg := f.service.config
	cfg.SecretKeys = secretTestKeys(t, "2", "1", "2")
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	connection, err := s.database.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodGet, "/health", testHubAdminToken, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health while rewrap pending: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-s.secretRewrapDone:
		t.Fatal("rewrap completed while writer unavailable")
	default:
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.secretRewrapDone:
	default:
		t.Fatal("shutdown did not join rewrap")
	}
}

func TestSecretRewrapContinuesAfterError(t *testing.T) {
	f, original := secretRewrapFixture(t)
	var logs bytes.Buffer
	f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
	if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_rewrap BEFORE INSERT ON organization_secret_audit WHEN NEW.event='rotate' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	f.service.rewrapSecrets(t.Context())
	rows, err := readSecretRows(t.Context(), f.service.database.reader)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		want := 2
		if row.project == "" {
			want = 1
		}
		if row.envelope.Version != want || !bytes.Equal(row.envelope.Ciphertext, original[i].envelope.Ciphertext) {
			t.Fatal("row error stopped pass or committed unaudited rewrap")
		}
	}
	if strings.Count(logs.String(), `"msg":"rewrap provider secret"`) != 2 || strings.Contains(logs.String(), spritesSecretSentinel) {
		t.Fatalf("rewrap logs missing or exposed secret: %s", logs.String())
	}
}

func TestProjectSecretRewrapDeletion(t *testing.T) {
	f := newBrowserHostedOrganizationFixture(t, true, "org_rewrap_deletion", func(cfg *Config) {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) { return spritesTestResponse(`{"sprites":[]}`, 200), nil })}
	})
	for _, name := range []string{"First secret project", "Second secret project"} {
		project := f.createProject(t, name)
		base := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/projects/" + project
		f.api(t, "owner", http.MethodPut, base+"/secrets/"+flySpritesToken, map[string]any{"token": spritesSecretSentinel}, http.StatusOK)
	}
	<-f.service.secretRewrapDone
	original, err := readSecretRows(t.Context(), f.service.database.reader)
	if err != nil || len(original) != 2 {
		t.Fatalf("rows=%d error=%v", len(original), err)
	}
	f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
	for _, row := range original {
		var name string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT name FROM projects WHERE id=?", row.project).Scan(&name); err != nil {
			t.Fatal(err)
		}
		base := "/api/v2/organizations/" + row.organization + "/projects/" + row.project
		f.api(t, "owner", http.MethodDelete, base, map[string]any{"idempotency_key": "delete-" + row.project, "confirm_name": name}, http.StatusOK)
		if err := f.service.rewrapSecret(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	var remaining, rotations, removals int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM project_secrets), (SELECT count(*) FROM project_secret_audit WHERE event='rotate'), (SELECT count(*) FROM project_secret_audit WHERE event='remove')").Scan(&remaining, &rotations, &removals); err != nil || remaining != 0 || rotations != 0 || removals != 2 {
		t.Fatalf("remaining=%d rotations=%d removals=%d error=%v", remaining, rotations, removals, err)
	}
}
