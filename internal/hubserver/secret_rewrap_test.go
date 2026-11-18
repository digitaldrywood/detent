package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
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

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coldSecretReader struct {
	tenantstore.Reader
	tenantstore.Repository
	secret accessstore.Secret
	legacy bool
}

func (r *coldSecretReader) ReadTenant(_ context.Context, _ tracker.OrganizationID, _ corestore.Workload, read func(tenantstore.Reader) error) error {
	return read(r)
}
func (r *coldSecretReader) AccessSecrets(_ context.Context, project tracker.ProjectID, kind string, limit int) ([]accessstore.Secret, error) {
	if r.legacy && project == "" && kind == "" && limit == 1 {
		return []accessstore.Secret{r.secret}, nil
	}
	return nil, errors.New("cold view enumerated tenant secrets")
}
func (r *coldSecretReader) AccessSecret(_ context.Context, project tracker.ProjectID, kind string) (accessstore.Secret, error) {
	if r.legacy || project != "" || kind != accessstore.NativeCursorKeyKind {
		return accessstore.Secret{}, corestore.ErrNotFound
	}
	return r.secret, nil
}

func TestColdHostedViewValidatesOrganizationKey(t *testing.T) {
	keys := secretTestKeys(t, "1", "1")
	org := tracker.OrganizationID("org_example")
	r := &coldSecretReader{}
	s := &Service{config: Config{runtimeHost: new(HostedRuntime), runtimeTenant: org, Hosted: &hostedplans.HostedConfig{OrganizationID: string(org)}, SecretKeys: keys, AccessRepository: r}}
	for _, kind := range []string{accessstore.NativeCursorKeyKind, "legacy-key"} {
		r.legacy = kind != accessstore.NativeCursorKeyKind
		r.secret = accessstore.Secret{OrganizationID: org, Kind: kind}
		envelope, err := keys.Seal([]byte("example"), accessstore.SecretAAD(org, "", kind))
		if err != nil {
			t.Fatal(err)
		}
		r.secret.Envelope = envelope
		if err := s.checkAccessSecretKeys(t.Context()); err != nil {
			t.Fatal(err)
		}
		r.secret.Envelope.Version++
		if err := s.checkAccessSecretKeys(t.Context()); err == nil {
			t.Fatalf("unreadable %s accepted", kind)
		}
	}
}

func TestSecretRewrapStepIsBoundedAndTenantScoped(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	prior := secretTestKeys(t, "1", "1")
	service := &Service{config: Config{AccessRepository: repository, SecretKeys: secretTestKeys(t, "2", "1", "2"), Clock: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), Deployment: "shared", PublicURL: "https://example.com", AllocationGeneration: 1}); err != nil {
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
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			envelope, err := prior.Seal([]byte("example"), accessstore.SecretAAD(org, "", "different"))
			if err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: "a_bad", Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: "a_bad", Actor: "example", Event: "set", KeyVersion: 1, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		cfg := service.config
		cfg.Hosted = &hostedplans.HostedConfig{OrganizationID: string(org), PublicURL: "https://example.com", WorkOSOrganizationID: "provider_" + string(org), SharedEntry: &hostedplans.HostedSharedEntry{Generation: 1}}
		host := &HostedRuntime{database: &database{postgres: repository}}
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		budget := hubmaintenance.Budget{Items: 1, Bytes: 1024}
		for i, kind := range []string{"a_bad", "first", "second"} {
			outcome, err := host.MaintainSecret(budget.Context(t.Context(), tenant), org, 1, cfg)
			if outcome.Items != 1 {
				t.Fatalf("org %s step %d used %d items", org, i, outcome.Items)
			}
			if (err != nil) != (i == 0) {
				t.Fatalf("org %s item %d error = %v", org, i, err)
			}
			cursor, err := repository.MaintenanceCursor(t.Context(), org, 1, "secret-rewrap")
			if err != nil || cursor.Kind != kind {
				t.Fatalf("org %s cursor = %v, want %q, err %v", org, cursor, kind, err)
			}
		}
	}

}

func secretRewrapFixture(t *testing.T) (nativeFixture, []accessstore.Secret, []*Service) {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	organizations := []tracker.OrganizationID{"org_startup", "org_other"}
	limit := (1+2*len(organizations))*cfg.Postgres.Pool.MaxConnections + 1
	if _, err := admin.ExecContext(t.Context(), fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT %d", pgx.Identifier{cfg.Postgres.Connection.Database}.Sanitize(), limit)); err != nil {
		t.Fatal(err)
	}
	key := hostedSharedKey(49)
	cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	var first nativeFixture
	var tenants []*Service
	var original []accessstore.Secret
	for _, org := range organizations {
		binding := *cfg.Hosted
		binding.OrganizationID, binding.WorkOSOrganizationID = string(org), "provider_"+string(org)
		seedStartupOrganization(t, store.postgres, org, &binding)
		now := time.Now().UTC().Truncate(time.Microsecond)
		cursor := bytes.Repeat([]byte{0x5a}, 32)
		envelope, err := cfg.SecretKeys.Seal(cursor, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example secret project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: hubapi.NewID("hub"), CursorKey: cursor}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: 1, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		tenantCfg := cfg
		hosted := *cfg.Hosted
		hosted.OrganizationID, hosted.WorkOSOrganizationID = string(org), "provider_"+string(org)
		entry := *cfg.Hosted.SharedEntry
		hosted.SharedEntry = &entry
		tenantCfg.Hosted = &hosted
		service, err := Open(t.Context(), tenantCfg)
		if err != nil {
			t.Fatal(err)
		}
		repository := service.config.TenantRepository
		projectRecordDatabases.Store(repository, admin)
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
			projectRecordDatabases.Delete(repository)
		})
		service.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
		<-service.secretRewrapDone
		if err := service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, service)
		if first.service == nil {
			first = nativeFixture{service: service, project: tracker.NativeProject{OrganizationID: org, ID: "project", Name: "Example secret project", Profile: "native"}}
		}
		for _, project := range []tracker.ProjectID{"", "project"} {
			row := accessstore.Secret{OrganizationID: org, ProjectID: project, Kind: flySpritesToken, UpdatedAt: now}
			row.Envelope, err = service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD(org, project, row.Kind))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.StoreAccessSecret(t.Context(), row, accessstore.SecretAudit{OrganizationID: org, ProjectID: project, Kind: row.Kind, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			original = append(original, row)
		}
	}
	rows, err := readRewrapSecrets(t.Context(), first.service, admin, original)
	if err != nil || len(rows) != 4 {
		t.Fatalf("secret fixture rows=%d error=%v", len(rows), err)
	}
	return first, original, tenants
}

var errSecretRewrapPopulation = errors.New("secret population differs")

func readRewrapSecrets(ctx context.Context, s *Service, admin *sql.DB, original []accessstore.Secret) ([]accessstore.Secret, error) {
	var total, cursors, projectCursors int
	if err := admin.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE kind=$1), count(*) FILTER (WHERE kind=$1 AND project_id<>'') FROM (
 SELECT '' AS project_id,kind FROM organization_secrets UNION ALL SELECT project_id,kind FROM project_secrets
) secrets`, accessstore.NativeCursorKeyKind).Scan(&total, &cursors, &projectCursors); err != nil {
		return nil, err
	}
	if total != len(original)+2 || cursors != 2 || projectCursors != 0 {
		return nil, fmt.Errorf("%w: total=%d want=%d cursor=%d want=2 project_cursor=%d want=0", errSecretRewrapPopulation, total, len(original)+2, cursors, projectCursors)
	}
	var rows []accessstore.Secret
	for _, before := range original {
		if err := s.config.AccessRepository.ReadTenant(ctx, before.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.AccessSecret(ctx, before.ProjectID, before.Kind)
			if err == nil {
				rows = append(rows, row)
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func assertSecretRewrap(t *testing.T, f nativeFixture, original []accessstore.Secret, version, audits int) {
	t.Helper()
	s := f.service
	rows, err := readRewrapSecrets(t.Context(), s, nativeAttemptTestDatabase(t, f), original)
	if err != nil || len(rows) != len(original) {
		t.Fatalf("rewrapped rows=%d error=%v", len(rows), err)
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
	envelope, err := s.config.SecretKeys.Rewrap(original.Envelope, accessstore.SecretAAD(original.OrganizationID, original.ProjectID, original.Kind))
	if err != nil {
		return err
	}
	return s.config.AccessRepository.WriteTenant(t.Context(), original.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
		return w.RewrapAccessSecret(t.Context(), original, envelope, accessstore.SecretAudit{OrganizationID: original.OrganizationID, ProjectID: original.ProjectID, Kind: original.Kind, Actor: "system:rewrap", Event: "rotate", KeyVersion: envelope.Version, RecordedAt: s.config.Clock()})
	})
}

func TestSecretRewrapRestart(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			f, original, tenants := secretRewrapFixture(t)
			admin := nativeAttemptTestDatabase(t, f)
			f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			if partial {
				if err := rewrapOriginalSecret(t, f.service, original[0]); err != nil {
					t.Fatal(err)
				}
			}
			configs := make([]Config, len(tenants))
			for i, tenant := range tenants {
				configs[i] = tenant.config
				if err := tenant.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for i, active := range []string{"2", "2", "1"} {
				var writer *sql.Conn
				if i == 1 {
					var err error
					writer, err = admin.Conn(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := writer.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
							t.Error(err)
						}
					})
				}
				var opened []*Service
				var results []chan error
				var joined []*bool
				for _, cfg := range configs {
					cfg.SecretKeys = secretTestKeys(t, active, "1", "2")
					service, err := Open(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := service.Close(); err != nil {
							t.Error(err)
						}
					})
					repository := service.config.TenantRepository
					projectRecordDatabases.Store(repository, admin)
					t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					result := make(chan error, 1)
					complete := new(bool)
					t.Cleanup(func() {
						if err := service.Close(); err != nil {
							t.Error(err)
						}
						if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
							t.Error(err)
						}
						if !*complete {
							if err := <-result; err != nil {
								t.Error(err)
							}
						}
					})
					go func() { result <- service.Serve(listener) }()
					<-service.secretRewrapDone
					opened = append(opened, service)
					results = append(results, result)
					joined = append(joined, complete)
				}
				version, audits := 2, 1
				if i == 2 {
					version, audits = 1, 2
				}
				f.service = opened[0]
				assertSecretRewrap(t, f, original, version, audits)
				if writer != nil {
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
				}
				for j, service := range opened {
					if err := service.Close(); err != nil {
						t.Fatal(err)
					}
					serveErr := <-results[j]
					*joined[j] = true
					if serveErr != nil {
						t.Fatal(serveErr)
					}
				}
			}
		})
	}
}

func TestSecretRewrapCAS(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"replace", "replace same version", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSecretPostgresFixture(t)
			f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
			for _, original := range f.originals {
				f.service.config.AccessRepository = &secretMutationAfterRead{Repository: f.repository, mutate: func() error {
					return f.repository.WriteTenant(t.Context(), original.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						audit := accessstore.SecretAudit{OrganizationID: original.OrganizationID, ProjectID: original.ProjectID, Kind: original.Kind, Actor: "replacement", Event: "set", KeyVersion: 2, RecordedAt: f.service.config.Clock()}
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
	t.Parallel()
	f, _, tenants := secretRewrapFixture(t)
	admin := nativeAttemptTestDatabase(t, f)
	var opened []*Service
	for _, tenant := range tenants {
		cfg := tenant.config
		cfg.SecretKeys = secretTestKeys(t, "2", "1", "2")
		if err := tenant.Close(); err != nil {
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
		opened = append(opened, service)
	}
	connection, err := admin.BeginTx(t.Context(), nil)
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
	for _, service := range opened {
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		assertion, err := cloudassert.Sign(hostedSharedKey(49), cloudassert.Claims{Issuer: "entry", Audience: service.config.Hosted.OrganizationID, Generation: 1, Kind: cloudassert.KindService, Method: http.MethodPost, Path: "/internal/v1/health", BodyDigest: cloudassert.BodyDigest([]byte("{}")), IssuedAt: now, ExpiresAt: now.Add(30 * time.Second), ID: id})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/health", strings.NewReader("{}"))
		request.Header.Set(cloudassert.Header, assertion)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("health while rewrap pending: %d %s", response.Code, response.Body.String())
		}
		select {
		case <-service.secretRewrapDone:
			t.Fatal("rewrap completed while writer unavailable")
		default:
		}
	}
	for _, service := range opened {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-service.secretRewrapDone:
		default:
			t.Fatal("shutdown did not join rewrap")
		}
	}
}

func TestSecretRewrapContinuesAfterError(t *testing.T) {
	t.Parallel()
	f, original, tenants := secretRewrapFixture(t)
	var logs bytes.Buffer
	for _, tenant := range tenants {
		tenant.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
		tenant.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), `CREATE FUNCTION fail_rewrap() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event='rotate' AND NEW.kind='`+flySpritesToken+`' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_rewrap BEFORE INSERT ON organization_secret_audit FOR EACH ROW EXECUTE FUNCTION fail_rewrap()`); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		tenant.rewrapSecrets(t.Context())
	}
	rows, err := readRewrapSecrets(t.Context(), f.service, nativeAttemptTestDatabase(t, f), original)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		want := 2
		if row.ProjectID == "" {
			want = 1
		}
		if row.Envelope.Version != want || !bytes.Equal(row.Envelope.Ciphertext, original[i].Envelope.Ciphertext) {
			t.Fatal("row error stopped pass or committed unaudited rewrap")
		}
		table, predicate, args := "organization_secret_audit", "organization_id=$1 AND kind=$2", []any{row.OrganizationID, row.Kind}
		rotations := 0
		if row.ProjectID != "" {
			table, predicate, args = "project_secret_audit", "organization_id=$1 AND project_id=$2 AND kind=$3", []any{row.OrganizationID, row.ProjectID, row.Kind}
			rotations = 1
		}
		var count int
		if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+predicate+" AND event='rotate' AND actor='system:rewrap'", args...).Scan(&count); err != nil || count != rotations {
			t.Fatalf("provider audit rollback/continuation: count=%d want=%d err=%v", count, rotations, err)
		}
	}
	for _, tenant := range tenants {
		org := tenant.accessOrganization()
		if err := tenant.config.AccessRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			cursor, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
			if err != nil {
				return err
			}
			if cursor.Envelope.Version != 2 {
				return fmt.Errorf("cursor secret rotation stopped in %s: version=%d", org, cursor.Envelope.Version)
			}
			value, err := tenant.config.SecretKeys.Open(cursor.Envelope, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			defer clear(value)
			if !bytes.Equal(value, bytes.Repeat([]byte{0x5a}, 32)) {
				return errors.New("cursor secret value changed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var count, version int
		if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*),COALESCE(max(key_version),0) FROM organization_secret_audit WHERE organization_id=$1 AND kind=$2 AND event='rotate' AND actor='system:rewrap'", org, accessstore.NativeCursorKeyKind).Scan(&count, &version); err != nil || count != 1 || version != 2 {
			t.Fatalf("cursor rotation audit: count=%d version=%d err=%v", count, version, err)
		}
	}
	if strings.Count(logs.String(), `"msg":"rewrap provider secret"`) != 2 || strings.Contains(logs.String(), spritesSecretSentinel) {
		t.Fatalf("rewrap logs missing or exposed secret: %s", logs.String())
	}
}

func TestProjectSecretRewrapDeletion(t *testing.T) {
	t.Parallel()
	f := newOperatorSecurityFixture(t)
	var original []accessstore.Secret
	for _, name := range []string{"First secret project", "Second secret project"} {
		project := tracker.ProjectID(hubapi.NewID("prj"))
		now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
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
	f.service.signalSecretRewrap()
	<-f.service.secretRewrapDone
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
	for i, row := range original {
		name := []string{"First secret project", "Second secret project"}[i]
		var snapshot accessstore.Secret
		if err := f.service.config.AccessRepository.ReadTenant(t.Context(), row.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			snapshot, err = r.AccessSecret(t.Context(), row.ProjectID, row.Kind)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if snapshot.OrganizationID != row.OrganizationID || snapshot.ProjectID != row.ProjectID || snapshot.Kind != row.Kind || snapshot.Envelope.Version != 1 {
			t.Fatalf("retirement snapshot identity=%s/%s/%s version=%d", snapshot.OrganizationID, snapshot.ProjectID, snapshot.Kind, snapshot.Envelope.Version)
		}
		base := "/api/v2/organizations/org_security/projects/" + string(row.ProjectID)
		requireNativeStatus(t, f.request(t, owner, http.MethodDelete, base, map[string]any{"idempotency_key": "delete-" + string(row.ProjectID), "confirm_name": name}), http.StatusOK)
		if err := rewrapOriginalSecret(t, f.service, snapshot); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Errorf("stale retirement CAS for %s/%s/%s: err=%v want revision conflict", snapshot.OrganizationID, snapshot.ProjectID, snapshot.Kind, err)
		}
	}
	var remaining, rotations, removals int
	if err := f.admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM project_secrets), (SELECT count(*) FROM project_secret_audit WHERE event='rotate'), (SELECT count(*) FROM project_secret_audit WHERE event='remove')").Scan(&remaining, &rotations, &removals); err != nil || remaining != 0 || rotations != 0 || removals != 2 {
		t.Fatalf("remaining=%d rotations=%d removals=%d error=%v", remaining, rotations, removals, err)
	}
}

func TestSecretRewrapTargetsSnapshot(t *testing.T) {
	t.Parallel()
	f := newSecretPostgresFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		envelope, err := f.service.config.SecretKeys.Seal(bytes.Repeat([]byte{0x5a}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: f.service.config.Clock()}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: f.service.config.Clock()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
	var target accessstore.Secret
	if err := f.repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		target, err = r.AccessSecret(t.Context(), "project", flySpritesToken)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if target.OrganizationID != "org_first" || target.ProjectID != "project" || target.Kind != flySpritesToken {
		t.Fatalf("snapshot identity=%s/%s/%s", target.OrganizationID, target.ProjectID, target.Kind)
	}
	if err := rewrapOriginalSecret(t, f.service, target); err != nil {
		t.Fatal(err)
	}
	for _, before := range f.originals {
		if err := f.repository.ReadTenant(t.Context(), before.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.AccessSecret(t.Context(), before.ProjectID, before.Kind)
			if err != nil {
				return err
			}
			want := 1
			if before.OrganizationID == target.OrganizationID && before.ProjectID == target.ProjectID && before.Kind == target.Kind {
				want = 2
			}
			if row.Envelope.Version != want {
				return fmt.Errorf("snapshot target %s/%s/%s: version=%d want=%d", row.OrganizationID, row.ProjectID, row.Kind, row.Envelope.Version, want)
			}
			cursor, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
			if err == nil && cursor.Envelope.Version != 1 {
				return fmt.Errorf("snapshot target rotated unrelated cursor secret in %s", before.OrganizationID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSecretRewrapPopulation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{flySpritesToken, accessstore.NativeCursorKeyKind} {
		t.Run(kind, func(t *testing.T) {
			f, original, _ := secretRewrapFixture(t)
			org := tracker.OrganizationID("org_unexpected")
			plaintext := []byte("unexpected")
			if kind == accessstore.NativeCursorKeyKind {
				plaintext = bytes.Repeat([]byte{0x5a}, 32)
			}
			envelope, err := f.service.config.SecretKeys.Seal(plaintext, accessstore.SecretAAD(org, "", kind))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				now := f.service.config.Clock()
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Unexpected", Slug: "unexpected", CreatedAt: now}); err != nil {
					return err
				}
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: kind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: kind, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			if rows, err := readRewrapSecrets(t.Context(), f.service, nativeAttemptTestDatabase(t, f), original); !errors.Is(err, errSecretRewrapPopulation) || len(rows) != 0 {
				t.Fatalf("unexpected secret population accepted: rows=%d error=%v", len(rows), err)
			}
		})
	}
}

func runSharedMaintenanceTestStep(t *testing.T, s *Service, name string) error {
	t.Helper()
	tenant := s.MaintenanceTenant()
	for _, feature := range s.SharedMaintenanceFeatures() {
		if feature.Name != name {
			continue
		}
		budget := hubmaintenance.Budget{Items: hubmaintenance.MinItems, Bytes: feature.Bytes}
		outcome, err := feature.Step(budget.Context(t.Context(), tenant), tenant, budget)
		if outcome.Items < 0 || outcome.Items > budget.Items {
			t.Fatalf("feature %s charged %d of %d items", name, outcome.Items, budget.Items)
		}
		return err
	}
	t.Fatalf("missing shared maintenance feature %s", name)
	return nil
}
