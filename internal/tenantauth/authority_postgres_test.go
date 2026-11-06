package tenantauth_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var template = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile("../hubserver/postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `SET ROLE detent_migration_v2`); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS("../hubserver/postgres/migrations")), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := template.Close(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func fixture(t *testing.T) (*sql.DB, *pgtransport.Pools) {
	t.Helper()
	db := template.Database(t)
	role := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'fixture-only'; GRANT detent_runtime_v2 TO `+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(t.Context()), `DROP ROLE `+role); err != nil {
			t.Error(err)
		}
	})
	config := template.NativeConfig(t, db, role, "fixture-only")
	pools, err := pgtransport.Open(t.Context(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	for _, org := range []string{"first", "second"} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations VALUES ($1,$1,$1,true,'2026-01-01')`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO hosted_tenant VALUES ($1,$1,'','','',1)`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO api_tokens (organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES ($1,'same','same','same-proof','fixture','operator',true,'2026-01-01','2026-01-01')`, org); err != nil {
			t.Fatal(err)
		}
	}
	return db, pools
}

func TestAuthorityIsolatesTenantsAndRejectsRevokedCredentials(t *testing.T) {
	db, pools := fixture(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	authority := tenantauth.New(pools, func() time.Time { return now })
	for _, org := range []string{"first", "second", "first"} {
		input := tenantauth.RequestIdentity{CredentialHash: "same-proof"}
		input.Organization = tracker.OrganizationID(org)
		scope, err := authority.Resolve(t.Context(), input)
		if err != nil || string(scope.Organization()) != org || scope.Principal().CredentialID != "same" {
			t.Fatalf("organization %s: scope=%v err=%v", org, scope, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=$1 WHERE organization_id='first'`, now); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		input  tenantauth.RequestIdentity
		denied bool
	}{
		{"revoked", tenantauth.RequestIdentity{Organization: "first", CredentialHash: "same-proof"}, true},
		{"other tenant remains usable", tenantauth.RequestIdentity{Organization: "second", CredentialHash: "same-proof"}, false},
		{"wrong proof", tenantauth.RequestIdentity{Organization: "second", CredentialHash: "wrong"}, true},
		{"missing tenant", tenantauth.RequestIdentity{CredentialHash: "same-proof"}, true},
		{"unknown tenant", tenantauth.RequestIdentity{Organization: "unknown", CredentialHash: "same-proof"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := authority.Resolve(t.Context(), test.input)
			if errors.Is(err, tenantauth.ErrDenied) != test.denied {
				t.Fatalf("Resolve()=%v, denied=%v", err, test.denied)
			}
		})
	}
	pools.Close()
	if _, err := authority.Resolve(t.Context(), tenantauth.RequestIdentity{Organization: "second", CredentialHash: "same-proof"}); !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, tenantauth.ErrDenied) {
		t.Fatalf("outage classification: %v", err)
	}
}
