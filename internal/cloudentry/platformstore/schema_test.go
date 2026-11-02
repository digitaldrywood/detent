package platformstore

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/schemafingerprint"
)

func TestPlatformInventoryAndRoleSeparation(t *testing.T) {
	db := template.Database(t)
	tables := map[string]bool{}
	for _, family := range []string{"registry", "auth"} {
		content, err := os.ReadFile("../testdata/" + family + "_classification.json")
		if err != nil {
			t.Fatal(err)
		}
		var classes map[string]json.RawMessage
		if err := json.Unmarshal(content, &classes); err != nil {
			t.Fatal(err)
		}
		for name := range classes {
			if name == "entry_schema_version" || strings.HasPrefix(name, "sqlite_") {
				continue
			}
			if family == "auth" && name == "platform_notifications" {
				name = "auth_notifications"
			}
			tables[name] = true
		}
	}
	tables["administration_receipts"] = true
	tables["organization_slug_counters"] = true
	tables["access_key_projects"] = true
	tables["access_key_blocked_projects"] = true
	var untyped int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema='platform' AND (data_type IN ('json','jsonb') OR column_name LIKE '%\_json' ESCAPE '\')`).Scan(&untyped); err != nil || untyped != 0 {
		t.Fatalf("untyped columns=%d err=%v", untyped, err)
	}
	for _, group := range []string{postgres.RuntimeRoleName, postgres.PlatformRoleName, postgres.MigrationRoleName} {
		t.Run(group, func(t *testing.T) {
			pool := rolePool(t, db, group, "")
			for table := range tables {
				t.Run(table, func(t *testing.T) {
					var owner string
					if err := db.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid=$1::regclass`, "platform."+table).Scan(&owner); err != nil {
						t.Fatal(err)
					}
					if owner != postgres.MigrationRoleName {
						t.Fatalf("table owner=%s", owner)
					}
					var count int
					err := pool.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.`+table).Scan(&count)
					if group == postgres.RuntimeRoleName {
						requireState(t, err, "42501")
					} else if err != nil {
						t.Fatal(err)
					}
					for _, statement := range []string{`INSERT INTO platform.` + table + ` DEFAULT VALUES`, `DELETE FROM platform.` + table} {
						if strings.HasSuffix(statement, "SET ") {
							continue
						}
						tx, err := pool.BeginTx(t.Context(), nil)
						if err != nil {
							t.Fatal(err)
						}
						_, err = tx.ExecContext(t.Context(), statement)
						tx.Rollback()
						if group == postgres.RuntimeRoleName {
							requireState(t, err, "42501")
						}
					}
					if group == postgres.PlatformRoleName {
						tx, err := pool.BeginTx(t.Context(), nil)
						if err != nil {
							t.Fatal(err)
						}
						_, err = tx.ExecContext(t.Context(), `ALTER TABLE platform.`+table+` ADD COLUMN example text`)
						tx.Rollback()
						requireState(t, err, "42501")
					}
				})
			}
		})
	}
}

func TestSchemaFingerprintUsesLatestMigrationState(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	rows, err := db.QueryContext(t.Context(), `SELECT version_id FROM (
		SELECT version_id, is_applied, ROW_NUMBER() OVER (PARTITION BY version_id ORDER BY id DESC) AS state_rank
		FROM public.goose_db_version WHERE version_id > 0
	) AS migration_state WHERE state_rank = 1 AND is_applied ORDER BY version_id`)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(versions) < 2 {
		t.Fatalf("applied migration versions = %v", versions)
	}
	wantBefore := schemafingerprint.Versions(versions)
	gotBefore, err := r.SchemaFingerprint(t.Context())
	if err != nil || gotBefore != wantBefore {
		t.Fatalf("initial fingerprint = %q, want %q: %v", gotBefore, wantBefore, err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO public.goose_db_version(version_id,is_applied) VALUES($1,false)`, versions[0]); err != nil {
		t.Fatal(err)
	}
	expected := slices.DeleteFunc(slices.Clone(versions), func(version int64) bool { return version == versions[0] })
	wantAfter := schemafingerprint.Versions(expected)
	gotAfter, err := r.SchemaFingerprint(t.Context())
	if err != nil || gotAfter != wantAfter || gotAfter == gotBefore {
		t.Fatalf("fingerprint after down record = %q, want %q (before %q): %v", gotAfter, wantAfter, gotBefore, err)
	}
}

func TestPlatformConstructorRejectsCombinedAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, group, attributes string
		want                    error
	}{
		{"platform", postgres.PlatformRoleName, "", nil}, {"runtime", postgres.RuntimeRoleName, "", ErrPlatformRole}, {"migration", postgres.MigrationRoleName, "", ErrPlatformRole}, {"no membership", "", "", ErrPlatformRole},
		{"runtime membership", postgres.PlatformRoleName + "," + postgres.RuntimeRoleName, "", ErrPlatformRole}, {"migration membership", postgres.PlatformRoleName + "," + postgres.MigrationRoleName, "", ErrPlatformRole},
		{"superuser", postgres.PlatformRoleName, "SUPERUSER", ErrPlatformRole}, {"bypass RLS", postgres.PlatformRoleName, "BYPASSRLS", ErrPlatformRole}, {"role administrator", postgres.PlatformRoleName, "CREATEROLE", ErrPlatformRole},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := template.Database(t)
			pool := rolePool(t, db, tc.group, tc.attributes)
			r, err := New(t.Context(), pool, testConfig(t))
			if r != nil {
				defer r.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("constructor: %v", err)
			}
		})
	}
}

func requireState(t *testing.T, err error, want string) {
	t.Helper()
	var state interface{ SQLState() string }
	if !errors.As(err, &state) || state.SQLState() != want {
		t.Fatalf("expected SQLSTATE %s, got %v", want, err)
	}
}

func TestPlatformErrorsRedactCustomerData(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	s := createSession(t, r, "private_customer_value")
	err := r.CreateSession(t.Context(), s)
	if !errors.Is(err, corestore.ErrUniqueViolation) || strings.Contains(err.Error(), s.Hash) {
		t.Fatalf("unsafe uniqueness error: %v", err)
	}
	requireState(t, err, "23505")
}

func TestPlatformTenantForeignKeys(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	for _, org := range []string{"org_first", "org_second"} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.oauth_clients(id,name,auth_method,secret_hash,created_at) VALUES($1,'Example','none','',$2)`, "client_"+org, r.now()); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"shared", "other"} {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.oauth_grants(organization_id,id,client_id,owner_subject,owner_email,permission,resource,created_at) VALUES($1,$2,$3,'owner','owner@example.com','write','https://example.com/mcp',$4)`, org, id, "client_"+org, r.now()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.oauth_refresh_tokens(organization_id,token_hash,grant_id,expires_at) VALUES($1,$2,$3,$4)`, org, org+"_"+id, id, r.now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.oauth_grants(organization_id,id,client_id,owner_subject,owner_email,permission,resource,created_at) VALUES('org_first','first_only','client_org_first','owner','owner@example.com','write','https://example.com/mcp',$1)`, r.now()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, query string }{
		{"refresh token", `INSERT INTO platform.oauth_refresh_tokens(organization_id,token_hash,grant_id,expires_at) VALUES('org_second','new','first_only',$1)`},
		{"spent token", `INSERT INTO platform.oauth_spent_refresh_tokens(organization_id,token_hash,grant_id,expires_at) VALUES('org_second','new','first_only',$1)`},
		{"OAuth request", `INSERT INTO platform.oauth_requests(organization_id,id,client_id,redirect_uri,challenge,resource,code_hash,code_expires_at,grant_id) VALUES('org_second','new','client_org_second','https://example.com','challenge','https://example.com/mcp','code',$1,'first_only')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(t.Context(), tc.query, r.now())
			requireState(t, err, "23503")
		})
	}
	for _, org := range []string{"org_first", "org_second"} {
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.oauth_refresh_tokens WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
			t.Fatalf("%s tokens=%d err=%v", org, count, err)
		}
	}
}
