package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSecretRewrapStepIsBoundedAndTenantScoped(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	prior := secretTestKeys(t, "1", "1")
	service := &Service{config: Config{AccessRepository: repository, SecretKeys: secretTestKeys(t, "2", "1", "2"), now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			for _, kind := range []string{"first", "second"} {
				envelope, err := prior.Seal([]byte("example"), accessstore.SecretAAD(org, "", kind))
				if err != nil {
					return err
				}
				if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: kind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: kind, Actor: "example", Event: "set", KeyVersion: 1, RecordedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		next, processed, err := service.MaintainSecret(t.Context(), org, SecretCursor{})
		if err != nil || !processed || next.Kind != "first" {
			t.Fatalf("one-item rewrap: next=%+v processed=%t err=%v", next, processed, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			secrets, err := r.AccessSecrets(t.Context(), "", "", 3)
			if err != nil {
				return err
			}
			if len(secrets) != 2 || secrets[0].Envelope.Version != 2 || secrets[1].Envelope.Version != 1 {
				return fmt.Errorf("rewrap exceeded one candidate: %+v", next)
			}
			for _, secret := range secrets {
				value, err := service.config.SecretKeys.Open(secret.Envelope, accessstore.SecretAAD(org, "", secret.Kind))
				if err != nil || string(value) != "example" {
					return fmt.Errorf("rewrap changed secret value: %w", err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func secretRewrapFixture(t *testing.T) (nativeFixture, []accessstore.Secret) {
	t.Helper()
	f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	f.service.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	<-f.service.secretRewrapDone
	second := newNativeFixture(t, f.service, "org_first_second", "Second secret project")
	var original []accessstore.Secret
	for _, fixture := range []nativeFixture{f, second} {
		for _, project := range []tracker.ProjectID{"", fixture.project.ID} {
			row := accessstore.Secret{OrganizationID: fixture.project.OrganizationID, ProjectID: project, Kind: flySpritesToken, UpdatedAt: f.service.config.now().UTC().Truncate(time.Microsecond)}
			envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD(row.OrganizationID, project, row.Kind))
			if err != nil {
				t.Fatal(err)
			}
			row.Envelope = envelope
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.StoreAccessSecret(t.Context(), row, accessstore.SecretAudit{OrganizationID: row.OrganizationID, ProjectID: project, Kind: row.Kind, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: row.UpdatedAt})
			}); err != nil {
				t.Fatal(err)
			}
			original = append(original, row)
		}
	}
	return f, original
}

func readRewrapSecrets(t *testing.T, s *Service, original []accessstore.Secret) []accessstore.Secret {
	t.Helper()
	var rows []accessstore.Secret
	for _, before := range original {
		if err := s.config.AccessRepository.ReadTenant(t.Context(), before.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.AccessSecret(t.Context(), before.ProjectID, before.Kind)
			if err == nil {
				rows = append(rows, row)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

func assertSecretRewrap(t *testing.T, f nativeFixture, original []accessstore.Secret, version, audits int) {
	t.Helper()
	s := f.service
	rows := readRewrapSecrets(t, s, original)
	if len(rows) != len(original) {
		t.Fatalf("rewrapped rows=%d", len(rows))
	}
	for i, row := range rows {
		before := original[i]
		if row.Envelope.Version != version || !bytes.Equal(row.Envelope.Ciphertext, before.Envelope.Ciphertext) || !bytes.Equal(row.Envelope.Nonce, before.Envelope.Nonce) || !row.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("rewrap changed value or metadata for %s/%s/%s: version=%d want=%d", row.OrganizationID, row.ProjectID, row.Kind, row.Envelope.Version, version)
		}
		value, err := s.config.SecretKeys.Open(row.Envelope, accessstore.SecretAAD(row.OrganizationID, row.ProjectID, row.Kind))
		if err != nil || string(value) != spritesSecretSentinel {
			t.Fatalf("secret plaintext changed: %v", err)
		}
		clear(value)
		table, predicate, args := "organization_secret_audit", "organization_id=$1 AND kind=$2", []any{row.OrganizationID, row.Kind}
		if row.ProjectID != "" {
			table, predicate, args = "project_secret_audit", "organization_id=$1 AND project_id=$2 AND kind=$3", []any{row.OrganizationID, row.ProjectID, row.Kind}
		}
		var count int
		admin := nativeAttemptTestDatabase(t, f)
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+predicate+" AND event='rotate' AND actor='system:rewrap'", args...).Scan(&count); err != nil || count != audits {
			t.Fatalf("rotation audits=%d want=%d error=%v", count, audits, err)
		}
		var auditVersion int
		if audits > 0 {
			if err := admin.QueryRowContext(t.Context(), "SELECT key_version FROM "+table+" WHERE "+predicate+" AND event='rotate' ORDER BY id DESC LIMIT 1", args...).Scan(&auditVersion); err != nil || auditVersion != version {
				t.Fatalf("audit version=%d want=%d error=%v", auditVersion, version, err)
			}
		}
	}
}

func rewrapOriginalSecret(t *testing.T, s *Service, original accessstore.Secret) error {
	t.Helper()
	after := SecretCursor{}
	if original.ProjectID != "" {
		after.Kind = original.Kind
	}
	_, _, err := s.MaintainSecret(t.Context(), original.OrganizationID, after)
	return err
}

func TestSecretRewrapRestart(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			f, original := secretRewrapFixture(t)
			f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			if partial {
				if err := rewrapOriginalSecret(t, f.service, original[0]); err != nil {
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
					writer, err = nativeAttemptTestDatabase(t, f).Conn(t.Context())
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
				f.service = s
				assertSecretRewrap(t, f, original, version, audits)
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
			f := newSecretPostgresFixture(t)
			f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			for _, original := range f.originals {
				f.service.config.AccessRepository = &secretMutationAfterRead{Repository: f.repository, mutate: func() error {
					return f.repository.WriteTenant(t.Context(), original.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						audit := accessstore.SecretAudit{OrganizationID: original.OrganizationID, ProjectID: original.ProjectID, Kind: original.Kind, Actor: "replacement", Event: "set", KeyVersion: 2, RecordedAt: f.service.config.now()}
						if mutation == "delete" {
							audit.Event = "remove"
							return w.DeleteAccessSecret(t.Context(), original.ProjectID, original.Kind, audit)
						}
						keys := f.service.config.SecretKeys
						if mutation == "replace same version" {
							keys = secretTestKeys(t, "1", "1", "2")
							audit.KeyVersion = 1
						}
						replacement := original
						envelope, err := keys.Seal([]byte(spritesSecretSentinel+"-replacement"), accessstore.SecretAAD(original.OrganizationID, original.ProjectID, original.Kind))
						if err != nil {
							return err
						}
						replacement.Envelope = envelope
						return w.StoreAccessSecret(t.Context(), replacement, audit)
					})
				}}
				after := SecretCursor{}
				if original.ProjectID != "" {
					after.Kind = original.Kind
				}
				next, processed, err := f.service.MaintainSecret(t.Context(), original.OrganizationID, after)
				if err != nil || !processed || next.ProjectID != original.ProjectID || next.Kind != original.Kind {
					t.Fatalf("stale rewrap next=%+v processed=%v err=%v", next, processed, err)
				}
			}
			var rows []accessstore.Secret
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := f.repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.AccessSecrets(t.Context(), "", "", 10)
					rows = append(rows, page...)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if mutation == "delete" && len(rows) != 0 || strings.HasPrefix(mutation, "replace") && len(rows) != 4 {
				t.Fatalf("rows=%d", len(rows))
			}
			for _, row := range rows {
				wantVersion := 2
				if mutation == "replace same version" {
					wantVersion = 1
				}
				value, err := f.service.config.SecretKeys.Open(row.Envelope, accessstore.SecretAAD(row.OrganizationID, row.ProjectID, row.Kind))
				if err != nil || string(value) != spritesSecretSentinel+"-replacement" || row.Envelope.Version != wantVersion {
					t.Fatalf("concurrent replacement lost: %v", err)
				}
				clear(value)
			}
			for _, table := range []string{"project_secret_audit", "organization_secret_audit"} {
				var count int
				if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE event='rotate'").Scan(&count); err != nil || count != 0 {
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
	connection, err := nativeAttemptTestDatabase(t, f).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	if _, err := connection.ExecContext(t.Context(), "LOCK TABLE organization_secrets, project_secrets IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
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
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), `CREATE FUNCTION fail_rewrap() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event='rotate' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_rewrap BEFORE INSERT ON organization_secret_audit FOR EACH ROW EXECUTE FUNCTION fail_rewrap()`); err != nil {
		t.Fatal(err)
	}
	f.service.rewrapSecrets(t.Context())
	rows := readRewrapSecrets(t, f.service, original)
	for i, row := range rows {
		want := 2
		if row.ProjectID == "" {
			want = 1
		}
		if row.Envelope.Version != want || !bytes.Equal(row.Envelope.Ciphertext, original[i].Envelope.Ciphertext) {
			t.Fatal("row error stopped pass or committed unaudited rewrap")
		}
	}
	if strings.Count(logs.String(), `"msg":"rewrap provider secret"`) != 2 || strings.Contains(logs.String(), spritesSecretSentinel) {
		t.Fatalf("rewrap logs missing or exposed secret: %s", logs.String())
	}
}

func TestProjectSecretRewrapDeletion(t *testing.T) {
	f := newOperatorSecurityFixture(t)
	var original []accessstore.Secret
	for _, name := range []string{"First secret project", "Second secret project"} {
		project := tracker.ProjectID(newNativeID("prj"))
		now := f.service.config.now().UTC().Truncate(time.Microsecond)
		envelope, err := f.service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD("org_security", project, flySpritesToken))
		if err != nil {
			t.Fatal(err)
		}
		row := accessstore.Secret{OrganizationID: "org_security", ProjectID: project, Kind: flySpritesToken, Envelope: envelope, UpdatedAt: now}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: project, Name: name, Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), row, accessstore.SecretAudit{OrganizationID: "org_security", ProjectID: project, Kind: row.Kind, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		original = append(original, row)
	}
	owner := f.user(t, "deletion", "owner", "deletion@example.test", "write", "")
	f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
	for i, row := range original {
		name := []string{"First secret project", "Second secret project"}[i]
		base := "/api/v2/organizations/org_security/projects/" + string(row.ProjectID)
		requireNativeStatus(t, f.request(t, owner, http.MethodDelete, base, map[string]any{"idempotency_key": "delete-" + string(row.ProjectID), "confirm_name": name}), http.StatusOK)
		if err := rewrapOriginalSecret(t, f.service, row); err != nil {
			t.Fatal(err)
		}
	}
	var remaining, rotations, removals int
	if err := f.admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM project_secrets), (SELECT count(*) FROM project_secret_audit WHERE event='rotate'), (SELECT count(*) FROM project_secret_audit WHERE event='remove')").Scan(&remaining, &rotations, &removals); err != nil || remaining != 0 || rotations != 0 || removals != 2 {
		t.Fatalf("remaining=%d rotations=%d removals=%d error=%v", remaining, rotations, removals, err)
	}
}
