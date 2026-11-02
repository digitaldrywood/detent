package platformstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type importObjects struct {
	mu        sync.Mutex
	data      map[string][]byte
	beforePut func(context.Context) error
	corrupt   bool
}

func (o *importObjects) Put(ctx context.Context, key string, r io.ReadSeeker, _ int64, _, _ string) error {
	if o.beforePut != nil {
		if err := o.beforePut(ctx); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data[key] = data
	return nil
}

func (o *importObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.data[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	if o.corrupt {
		data = []byte("corrupt")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func importFixture(t *testing.T, db *sql.DB, cfg Config) ImportSource {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	source := ImportSource{DB: db, Catalog: catalog, Registry: storagecatalog.Source{Schema: "example_registry", Store: "registry", CatalogVersion: catalog.Version, Organizations: []string{"org_first", "org_second"}}, Auth: storagecatalog.Source{Schema: "example_auth", Store: "auth", CatalogVersion: catalog.Version, Organizations: []string{"org_first", "org_second"}}, RegistrySchema: storagecatalog.Schema{}, AuthSchema: storagecatalog.Schema{}, SnapshotID: "example-fenced-snapshot", SigningKey: cfg.SigningKey.Public().(ed25519.PublicKey), KeyVersions: cfg.Keys.Versions(), Objects: &importObjects{data: map[string][]byte{}}}
	for _, store := range []string{"registry", "auth"} {
		schema := source.RegistrySchema
		sourceName := source.Registry.Schema
		if store == "auth" {
			schema, sourceName = source.AuthSchema, source.Auth.Schema
		}
		if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+pq.QuoteIdentifier(sourceName)); err != nil {
			t.Fatal(err)
		}
		for name, table := range catalog.Tables {
			tableName, ok := strings.CutPrefix(name, store+"/")
			if !ok {
				continue
			}
			schema[tableName] = map[string]string{}
			var columns []string
			for column, definition := range table.Columns {
				schema[tableName][column] = definition.Source
				columns = append(columns, pq.QuoteIdentifier(column)+` text COLLATE "C"`)
			}
			if table.Disposition != "target" {
				continue
			}
			i := Importer{source: source}
			keys, err := i.sourceKeys(name)
			if err != nil {
				t.Fatal(err)
			}
			for n, key := range keys {
				keys[n] = pq.QuoteIdentifier(key)
			}
			query := "CREATE TABLE " + i.sourceTable(name) + "(" + strings.Join(columns, ",") + ",PRIMARY KEY(" + strings.Join(keys, ",") + "))"
			if _, err := db.ExecContext(t.Context(), query); err != nil {
				t.Fatal(err)
			}
		}
	}
	return source
}

func insertPlatformSource(t *testing.T, source ImportSource, name string, values map[string]any) {
	t.Helper()
	columns := slices.Sorted(maps.Keys(values))
	args, placeholders := make([]any, len(columns)), make([]string, len(columns))
	for n, column := range columns {
		args[n], placeholders[n], columns[n] = values[column], fmt.Sprintf("$%d", n+1), pq.QuoteIdentifier(column)
	}
	i := Importer{source: source}
	if _, err := source.DB.ExecContext(t.Context(), "INSERT INTO "+i.sourceTable(name)+"("+strings.Join(columns, ",")+") VALUES("+strings.Join(placeholders, ",")+")", args...); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

func platformSourceValues(t *testing.T, source ImportSource, cfg Config, name, suffix string, index int) map[string]any {
	t.Helper()
	now := cfg.Now()
	values := map[string]any{}
	for column, d := range source.Catalog.Tables[name].Columns {
		var value any
		switch d.Codec {
		case "text":
			value = column + "_" + suffix
		case "bool01":
			value = "0"
		case "int64":
			value = "1"
		case "timestamp":
			value = now.Format(time.RFC3339Nano)
		}
		if len(d.Allowed) > 0 {
			value = d.Allowed[0]
		}
		if strings.Contains(column, "expires_at") {
			value = now.Add(time.Hour).Format(time.RFC3339Nano)
		}
		if d.Null == "preserve" {
			value = nil
		}
		switch column {
		case "id":
			if d.Codec == "int64" {
				value = strconv.Itoa(index)
			} else {
				value = "id_" + suffix
			}
		case "organization_id":
			value = "org_" + suffix
		case "service_organization_id":
			value = ""
		case "subject", "owner_subject", "creator_subject":
			value = "subject_" + suffix
		case "email", "owner_email", "creator_email":
			value = suffix + "@example.com"
		case "key_id", "grant_id", "client_id":
			value = "id_" + suffix
		case "binding_id":
			value = "id_" + suffix
		case "permission":
			value = "write"
		}
		values[column] = value
	}
	switch name {
	case "registry/organizations":
		values["id"], values["state"], values["slug"], values["endpoint"] = "org_"+suffix, "ready", suffix, "unix:/example/"+suffix+".sock"
	case "registry/organization_slugs":
		values["slug"] = suffix
	case "registry/billing_events":
		values["status"] = "pending"
	case "registry/entitlement_changes":
		values["reason"] = "example grant"
	case "auth/sessions", "auth/authorizations":
		identity := auth.HostedIdentity{Subject: "subject_" + suffix, SessionID: "session_" + suffix, OrganizationID: "provider_id_" + suffix, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		encoded, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		values["identity_json"] = string(encoded)
		values["token_hash"] = "session_hash_" + suffix
		if name == "auth/authorizations" {
			delete(values, "token_hash")
			binding := cloudassert.AuthorizationBinding("session_hash_"+suffix, "org_"+suffix, identity.SessionID)
			values["binding"], values["session_hash"] = binding, "session_hash_"+suffix
			cipher, err := newSecretCipher(cfg.SigningKey, cfg.Keys)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"access_token", "refresh_token"} {
				purpose := "provider-access"
				if field == "refresh_token" {
					purpose = "provider-refresh"
				}
				sealed, err := cipher.seal(purpose, binding, field+"_"+suffix)
				if err != nil {
					t.Fatal(err)
				}
				values[field] = sealed
			}
		}
	case "auth/transactions":
		values["organization_id"], values["invitation_organization"] = "", ""
	case "auth/access_keys":
		values["access_context"], values["kind"] = "selected", "personal"
		values["organizations_json"] = `[{"organization_id":"org_` + suffix + `","project_access":"selected","project_ids":["project_allowed","project_blocked"]}]`
	case "auth/access_key_organizations":
		values["blocked_projects_json"] = `["project_blocked"]`
	case "auth/github_user_connections":
		cipher, err := newSecretCipher(cfg.SigningKey, cfg.Keys)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := cipher.seal("github-user-access", "subject_"+suffix, "github_"+suffix)
		if err != nil {
			t.Fatal(err)
		}
		values["access_token"] = sealed
	case "auth/oauth_grants":
		values["access_hash"] = "access_" + suffix
		values["access_expires_at"] = now.Add(time.Hour).Format(time.RFC3339Nano)
	case "auth/oauth_refresh_tokens":
		values["token_hash"] = "refresh_" + suffix
	case "auth/oauth_spent_refresh_tokens":
		values["token_hash"] = "spent_" + suffix
	case "auth/oauth_requests":
		values["consumed_at"] = now.Format(time.RFC3339Nano)
	case "auth/audit":
		values["event"] = `{"kind":"operator_administration","retry":"retry_` + suffix + `","hash":"input","outcome":"succeeded","output":{"resource_id":"resource_` + suffix + `","data":{"precise":9007199254740993}}}`
	}
	return values
}

func seedPlatformSource(t *testing.T, source ImportSource, cfg Config) {
	t.Helper()
	for _, name := range importTables {
		for n, suffix := range []string{"first", "second"} {
			insertPlatformSource(t, source, name, platformSourceValues(t, source, cfg, name, suffix, n+1))
		}
	}
}

func finishPlatformImport(t *testing.T, i *Importer, limit int) ImportProgress {
	t.Helper()
	for {
		p, err := i.Step(t.Context(), limit)
		if err != nil {
			t.Fatal(err)
		}
		if p.Done {
			return p
		}
	}
}

func TestPlatformImportRetainsAuthenticationAndEveryClassification(t *testing.T) {
	for _, mode := range []string{"legacy", "keyring"} {
		t.Run(mode, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			if mode == "keyring" {
				cfg.Keys = testKeys(t)
			}
			r := repository(t, db, cfg)
			source := importFixture(t, db, cfg)
			seedPlatformSource(t, source, cfg)
			_, err := db.ExecContext(t.Context(), `INSERT INTO platform.sessions SELECT * FROM example_auth.sessions`)
			if err == nil {
				t.Fatal("raw session copy unexpectedly matches typed identity schema")
			}
			requireState(t, err, "42804")
			i, err := r.NewImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			first, err := i.Step(t.Context(), 1)
			if err != nil || first.Processed != 1 {
				t.Fatalf("first checkpoint: %+v %v", first, err)
			}
			reopened := replicaRepository(t, r, cfg)
			i, err = reopened.NewImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			completed := finishPlatformImport(t, i, 1)
			if completed.Processed != int64(2*len(importTables)) {
				t.Fatalf("processed=%d", completed.Processed)
			}
			if replay, err := i.Step(t.Context(), ImportLimit); err != nil || replay != completed {
				t.Fatalf("replay: %+v %v", replay, err)
			}
			for _, name := range importTargetTables() {
				var count int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM platform."+name).Scan(&count); err != nil || count != 2 {
					t.Fatalf("%s: count=%d err=%v", name, count, err)
				}
			}
			for _, suffix := range []string{"first", "second"} {
				organization := tracker.OrganizationID("org_" + suffix)
				if _, err := reopened.Session(t.Context(), "session_hash_"+suffix); err != nil {
					t.Fatal(err)
				}
				binding := cloudassert.AuthorizationBinding("session_hash_"+suffix, string(organization), "session_"+suffix)
				tokens, err := reopened.Tokens(t.Context(), "session_hash_"+suffix, organization, binding)
				if err != nil || tokens.AccessToken != "access_token_"+suffix || tokens.RefreshToken != "refresh_token_"+suffix {
					t.Fatalf("provider tokens: %+v %v", tokens, err)
				}
				if token, err := reopened.GitHubToken(t.Context(), "subject_"+suffix); err != nil || token != "github_"+suffix {
					t.Fatalf("github token: %q %v", token, err)
				}
				if _, err := reopened.KeyAuthority(t.Context(), "token_hash_"+suffix, organization, "project_allowed"); err != nil {
					t.Fatal(err)
				}
				if _, err := reopened.KeyAuthority(t.Context(), "token_hash_"+suffix, organization, "project_blocked"); !errors.Is(err, ErrNoSession) {
					t.Fatalf("blocked project: %v", err)
				}
				if _, err := reopened.OAuthGrant(t.Context(), organization, "access_"+suffix); err != nil {
					t.Fatal(err)
				}
				login, err := reopened.ConsumeLogin(t.Context(), "token_hash_"+suffix, "transaction_id_"+suffix)
				if err != nil || login.OrganizationID != "" {
					t.Fatalf("global login: %+v %v", login, err)
				}
				if _, err := reopened.ConsumeLogin(t.Context(), "token_hash_"+suffix, "transaction_id_"+suffix); !errors.Is(err, ErrNoSession) {
					t.Fatalf("login replay: %v", err)
				}
			}
			if err := reopened.RevokeSession(t.Context(), "session_hash_first"); err != nil {
				t.Fatal(err)
			}
			binding := cloudassert.AuthorizationBinding("session_hash_first", "org_first", "session_first")
			if _, err := reopened.Tokens(t.Context(), "session_hash_first", "org_first", binding); !errors.Is(err, ErrNoSession) {
				t.Fatalf("revoked session tokens: %v", err)
			}
			if err := reopened.RevokeKey(t.Context(), "subject_first", "id_first"); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.KeyAuthority(t.Context(), "token_hash_first", "org_first", "project_allowed"); !errors.Is(err, ErrNoSession) {
				t.Fatalf("revoked key authorizes claim: %v", err)
			}
			if _, err := reopened.KeyAuthority(t.Context(), "token_hash_second", "org_second", "project_allowed"); err != nil {
				t.Fatalf("other tenant key: %v", err)
			}
			tokens := OAuthTokens{AccessHash: "new_access", RefreshHash: "new_refresh", AccessExpiresAt: cfg.Now().Add(time.Hour), RefreshExpiresAt: cfg.Now().Add(time.Hour)}
			if err := reopened.ExchangeOAuthCode(t.Context(), "org_first", "code_hash_first", "id_first", "resource_first", "redirect_uri_first", oauth2.GenerateVerifier(), tokens); !errors.Is(err, ErrNoSession) {
				t.Fatalf("imported code replay: %v", err)
			}
			if err := reopened.RotateOAuthRefresh(t.Context(), "org_first", "spent_first", "id_first", "resource_first", tokens); !errors.Is(err, ErrNoSession) {
				t.Fatalf("imported spent refresh replay: %v", err)
			}
			if _, err := reopened.OAuthGrant(t.Context(), "org_first", "access_first"); !errors.Is(err, ErrNoSession) {
				t.Fatalf("replay did not revoke its grant: %v", err)
			}
			if _, err := reopened.OAuthGrant(t.Context(), "org_second", "access_second"); err != nil {
				t.Fatalf("refresh replay revoked another tenant: %v", err)
			}
			if _, err := i.Step(t.Context(), ImportLimit); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.KeyAuthority(t.Context(), "token_hash_first", "org_first", "project_allowed"); !errors.Is(err, ErrNoSession) {
				t.Fatalf("import replay restored revoked key: %v", err)
			}
			assertImportedAudit(t, db, source)
		})
	}
}

func TestPlatformImportRollsBackAndResumes(t *testing.T) {
	for _, failure := range []string{"constraint", "cancellation", "object verification", "credential uniqueness"} {
		t.Run(failure, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			r := repository(t, db, cfg)
			source := importFixture(t, db, cfg)
			seedPlatformSource(t, source, cfg)
			i, err := r.NewImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			stage := int64(0)
			if failure == "credential uniqueness" {
				stage = int64(slices.Index(importTables[:], "auth/access_keys"))
			}
			if failure == "cancellation" || failure == "object verification" {
				stage = int64(slices.Index(importTables[:], "auth/audit"))
			}
			for stage > 0 {
				var current int64
				err := db.QueryRowContext(t.Context(), `SELECT stage FROM platform.import_checkpoint WHERE singleton`).Scan(&current)
				if err == nil && current == stage {
					break
				}
				if !errors.Is(err, sql.ErrNoRows) && err != nil {
					t.Fatal(err)
				}
				if _, err := i.Step(t.Context(), 2); err != nil {
					t.Fatal(err)
				}
			}
			if stage == 0 {
				if _, err := db.ExecContext(t.Context(), `UPDATE example_registry.organizations SET provider_id='provider_id_first' WHERE id='org_second'`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			objects := source.Objects.(*importObjects)
			switch failure {
			case "credential uniqueness":
				if _, err := db.ExecContext(t.Context(), `UPDATE example_auth.access_keys SET token_hash='token_hash_first' WHERE id='id_second'`); err != nil {
					t.Fatal(err)
				}
			case "cancellation":
				objects.beforePut = func(ctx context.Context) error {
					if err := r.transaction(ctx, corestore.Background, true, func(*sql.Tx) error { return nil }); err != nil {
						return err
					}
					cancel()
					return nil
				}
			case "object verification":
				objects.corrupt = true
			}
			_, err = i.Step(ctx, 2)
			if err == nil {
				t.Fatal("failed batch committed")
			}
			var gotStage, processed int64
			if err := db.QueryRowContext(t.Context(), `SELECT stage,processed FROM platform.import_checkpoint WHERE singleton`).Scan(&gotStage, &processed); err != nil {
				t.Fatal(err)
			}
			if gotStage != stage || processed != stage*2 {
				t.Fatalf("failed checkpoint advanced: stage=%d processed=%d", gotStage, processed)
			}
			var count int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM platform."+importTarget(importTables[stage])).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial batch: count=%d err=%v", count, err)
			}
			objects.beforePut, objects.corrupt = nil, false
			if failure == "constraint" {
				if _, err := db.ExecContext(t.Context(), `UPDATE example_registry.organizations SET provider_id='provider_id_second' WHERE id='org_second'`); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "credential uniqueness" {
				if _, err := db.ExecContext(t.Context(), `UPDATE example_auth.access_keys SET token_hash='token_hash_second' WHERE id='id_second'`); err != nil {
					t.Fatal(err)
				}
			}
			finishPlatformImport(t, i, 2)
		})
	}
}

func TestPlatformImportRejectsChangedAuthorityAndUnsafeRows(t *testing.T) {
	for _, failure := range []string{"signing key", "missing key version", "source column", "source index", "manifest", "nested tenant", "identity subject", "ciphertext binding", "snapshot replay"} {
		t.Run(failure, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			cfg.Keys = testKeys(t)
			r := repository(t, db, cfg)
			source := importFixture(t, db, cfg)
			seedPlatformSource(t, source, cfg)
			switch failure {
			case "signing key":
				source.SigningKey = make(ed25519.PublicKey, ed25519.PublicKeySize)
			case "missing key version":
				source.KeyVersions = []int{2}
			case "source column":
				if _, err := db.ExecContext(t.Context(), `ALTER TABLE example_auth.sessions ADD COLUMN unexpected text`); err != nil {
					t.Fatal(err)
				}
			case "source index":
				if _, err := db.ExecContext(t.Context(), `ALTER TABLE example_auth.sessions DROP CONSTRAINT sessions_pkey`); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				source.Auth.Organizations = []string{"org_first"}
			case "nested tenant":
				if _, err := db.ExecContext(t.Context(), `UPDATE example_auth.access_keys SET organizations_json='[{"organization_id":"org_other","project_access":"all"}]'`); err != nil {
					t.Fatal(err)
				}
			case "identity subject":
				if _, err := db.ExecContext(t.Context(), `UPDATE example_auth.sessions SET subject='different'`); err != nil {
					t.Fatal(err)
				}
			case "ciphertext binding":
				if _, err := db.ExecContext(t.Context(), `UPDATE example_auth.authorizations SET access_token=(SELECT refresh_token FROM example_auth.authorizations WHERE organization_id='org_second') WHERE organization_id='org_first'`); err != nil {
					t.Fatal(err)
				}
			}
			i, err := r.NewImporter(t.Context(), source)
			if slices.Contains([]string{"nested tenant", "identity subject", "ciphertext binding", "snapshot replay"}, failure) {
				if err != nil {
					t.Fatal(err)
				}
				if failure == "snapshot replay" {
					if _, err := i.Step(t.Context(), 1); err != nil {
						t.Fatal(err)
					}
					source.SnapshotID = "different-fence"
					i, err = r.NewImporter(t.Context(), source)
					if err != nil {
						t.Fatal(err)
					}
				}
				for err == nil {
					var p ImportProgress
					p, err = i.Step(t.Context(), ImportLimit)
					if p.Done {
						t.Fatal("unsafe source accepted")
					}
				}
			}
			if err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

func TestPlatformImportRejectsSourceChangedAfterFinalCheck(t *testing.T) {
	db := template.Database(t)
	cfg := testConfig(t)
	r := repository(t, db, cfg)
	source := importFixture(t, db, cfg)
	seedPlatformSource(t, source, cfg)
	i, err := r.NewImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for {
		var stage int64
		if err := db.QueryRowContext(t.Context(), `SELECT stage FROM platform.import_checkpoint WHERE singleton`).Scan(&stage); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		if stage == int64(len(importTables)-1) {
			break
		}
		if _, err := i.Step(t.Context(), ImportLimit); err != nil {
			t.Fatal(err)
		}
	}
	i.source.copyVerifier = changedAfterFinalCheckVerifier(t)
	progress, err := i.Step(t.Context(), ImportLimit)
	if !errors.Is(err, ErrImport) || progress.Done {
		t.Fatalf("final import accepted a changed source: progress=%+v error=%v", progress, err)
	}
	var stage, processed int64
	if err := db.QueryRowContext(t.Context(), `SELECT stage,processed FROM platform.import_checkpoint WHERE singleton`).Scan(&stage, &processed); err != nil {
		t.Fatal(err)
	}
	if stage != int64(len(importTables)) || processed != int64(2*len(importTables)) {
		t.Fatalf("final source check changed committed checkpoint unexpectedly: stage=%d processed=%d", stage, processed)
	}
}

type changedAfterFinalCheck struct {
	verifier sqliteCopyVerifier
	path     string
	changed  bool
}

func (v *changedAfterFinalCheck) Verify(ctx context.Context, complete bool) error {
	if err := v.verifier.Verify(ctx, complete); err != nil {
		return err
	}
	if complete && !v.changed {
		if err := os.Chmod(v.path, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(v.path, []byte("source-v2"), 0o600); err != nil {
			return err
		}
		if err := os.Chmod(v.path, 0o400); err != nil {
			return err
		}
		v.changed = true
	}
	return nil
}

func changedAfterFinalCheckVerifier(t *testing.T) importCopyVerifier {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	data := []byte("source-v1")
	if err := os.WriteFile(path, data, 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	copy := &sealedSQLiteCopy{path: path, file: file, info: info, digest: sha256.Sum256(data)}
	return &changedAfterFinalCheck{verifier: sqliteCopyVerifier{copies: []*sealedSQLiteCopy{copy}}, path: path}
}

func TestPlatformImportConcurrentCheckpoint(t *testing.T) {
	db := template.Database(t)
	cfg := testConfig(t)
	r := repository(t, db, cfg)
	source := importFixture(t, db, cfg)
	seedPlatformSource(t, source, cfg)
	i, err := r.NewImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	objects := source.Objects.(*importObjects)
	var entered atomic.Bool
	objects.beforePut = func(ctx context.Context) error {
		var nestedErr error
		if entered.CompareAndSwap(false, true) {
			_, nestedErr = i.Step(ctx, ImportLimit)
		}
		return nestedErr
	}
	for {
		p, err := i.Step(t.Context(), ImportLimit)
		if errors.Is(err, corestore.ErrRevisionConflict) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Done {
			t.Fatal("stale concurrent checkpoint committed")
		}
	}
	objects.beforePut = nil
	completed := finishPlatformImport(t, i, ImportLimit)
	if !reflect.DeepEqual(completed, ImportProgress{Processed: int64(2 * len(importTables)), Done: true}) {
		t.Fatalf("concurrent progress: %+v", completed)
	}
}

func assertImportedAudit(t *testing.T, db *sql.DB, source ImportSource) {
	t.Helper()
	for _, suffix := range []string{"first", "second"} {
		var key, resource string
		if err := db.QueryRowContext(t.Context(), `SELECT output_object_key,resource_id FROM platform.administration_receipts WHERE organization_id=$1`, "org_"+suffix).Scan(&key, &resource); err != nil {
			t.Fatal(err)
		}
		reader, err := source.Objects.Open(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		compressed, err := gzip.NewReader(reader)
		if err != nil {
			reader.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(compressed)
		if err = errors.Join(err, compressed.Close(), reader.Close()); err != nil {
			t.Fatal(err)
		}
		if resource != "resource_"+suffix || string(data) != `{"precise":9007199254740993}` {
			t.Fatalf("receipt output: %s %q", resource, data)
		}
	}
}
