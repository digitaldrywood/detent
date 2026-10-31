package platformstore

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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
	tables["access_key_projects"] = true
	tables["access_key_blocked_projects"] = true
	var untyped int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema='platform' AND (data_type IN ('json','jsonb') OR column_name LIKE '%\_json' ESCAPE '\')`).Scan(&untyped); err != nil || untyped != 0 {
		t.Fatalf("untyped columns=%d err=%v", untyped, err)
	}
	for _, group := range []string{"detent_runtime", "detent_platform", "detent_migration"} {
		t.Run(group, func(t *testing.T) {
			pool := rolePool(t, db, group, "")
			for table := range tables {
				t.Run(table, func(t *testing.T) {
					var owner string
					if err := db.QueryRowContext(t.Context(), `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid=$1::regclass`, "platform."+table).Scan(&owner); err != nil {
						t.Fatal(err)
					}
					if owner != "detent_migration" {
						t.Fatalf("table owner=%s", owner)
					}
					var count int
					err := pool.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.`+table).Scan(&count)
					if group == "detent_runtime" {
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
						if group == "detent_runtime" {
							requireState(t, err, "42501")
						}
					}
					if group == "detent_platform" {
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

func TestPlatformConstructorRejectsCombinedAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, group, attributes string
		want                    error
	}{
		{"platform", "detent_platform", "", nil}, {"runtime", "detent_runtime", "", ErrPlatformRole}, {"migration", "detent_migration", "", ErrPlatformRole}, {"no membership", "", "", ErrPlatformRole},
		{"runtime membership", "detent_platform,detent_runtime", "", ErrPlatformRole}, {"migration membership", "detent_platform,detent_migration", "", ErrPlatformRole},
		{"superuser", "detent_platform", "SUPERUSER", ErrPlatformRole}, {"bypass RLS", "detent_platform", "BYPASSRLS", ErrPlatformRole}, {"role administrator", "detent_platform", "CREATEROLE", ErrPlatformRole},
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
