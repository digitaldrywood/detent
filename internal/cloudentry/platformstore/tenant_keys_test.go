package platformstore

import (
	"context"
	"database/sql"

	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type countedTracer struct{ calls *atomic.Int64 }

func TestTenantKeyCorruptionIsDenied(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		id := "key_" + string(org)
		if err := r.CreateKey(t.Context(), Key{ID: id, Hash: "hash_" + id, Name: "Example", Kind: "service", OwnerSubject: "owner", Permission: "read", AccessContext: "selected", ServiceOrganizationID: &org, Organizations: []KeyOrganization{{OrganizationID: org, ProjectAccess: "all"}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE platform.access_keys SET owner_subject='' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := r.TenantKey(t.Context(), id, org); !errors.Is(err, tenantauth.ErrDenied) || errors.Is(err, tenantauth.ErrUnavailable) {
			t.Fatalf("corrupt tenant key must be a definite denial: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.TenantKey(ctx, "key_org_first", "org_first"); !errors.Is(err, context.Canceled) || !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, tenantauth.ErrDenied) {
		t.Fatalf("cancelled authority query must remain an outage: %v", err)
	}
}

func (c countedTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.calls.Add(1)
	return ctx
}
func (c countedTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func TestTenantKeyConcurrentReads(t *testing.T) {
	db := template.Database(t)
	var database string
	if err := db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 12"); err != nil {
		t.Fatal(err)
	}
	config := testConfig(t)
	config.Pool = PoolConfig{MaxConnections: 10, ServingConnections: 8, BackgroundConnections: 1}
	var calls atomic.Int64
	pool := rolePool(t, db, postgres.PlatformRoleName, "", countedTracer{calls: &calls})
	r, err := New(t.Context(), pool, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	seedOrganizations(t, r)
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := r.CreateKey(t.Context(), Key{ID: "key_" + string(org), Hash: "hash_" + string(org), Name: "Example", Kind: "service", OwnerSubject: "owner", Permission: "write", AccessContext: "project", ServiceOrganizationID: &org, Organizations: []KeyOrganization{{OrganizationID: org, ProjectAccess: "project", Projects: []tracker.ProjectID{"prj_example"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	locked, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := locked.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
		group.Wait()
	}()
	if _, err := locked.ExecContext(t.Context(), `LOCK TABLE platform.access_keys IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	before := calls.Load()
	for i := range 32 {
		group.Go(func() {
			org := tracker.OrganizationID("org_first")
			if i%2 != 0 {
				org = "org_second"
			}
			result, err := r.TenantKey(t.Context(), "key_"+string(org), org)
			if err != nil || result.Organization.ID != org || !result.Authority.Allows("prj_example") {
				t.Errorf("authority %s: %v", org, err)
			}
		})
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting int
		if err := locked.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND relation='platform.access_keys'::regclass AND NOT granted`).Scan(&waiting); err != nil {
			return err
		}
		if waiting != config.Pool.ServingConnections {
			return fmt.Errorf("concurrent authority queries=%d, want %d", waiting, config.Pool.ServingConnections)
		}
		return nil
	})
	if err := locked.Rollback(); err != nil {
		t.Fatal(err)
	}
	group.Wait()
	if count := calls.Load() - before; count != 32 {
		t.Fatalf("32 calls issued %d database operations", count)
	}
	t.Logf("32 concurrent calls across two tenants: %s, max concurrent SQL=8, exactly 1 query/call", time.Since(started))
	before = calls.Load()
	started = time.Now()
	for range 8 {
		group.Go(func() {
			for i := range 125 {
				org := tracker.OrganizationID("org_first")
				if i%2 != 0 {
					org = "org_second"
				}
				if _, err := r.TenantKey(t.Context(), "key_"+string(org), org); err != nil {
					t.Error(err)
				}
			}
		})
	}
	group.Wait()
	elapsed := time.Since(started)
	if count := calls.Load() - before; count != 1000 {
		t.Fatalf("1000 calls issued %d operations", count)
	}
	t.Logf("1000 calls / 8 workers / 2 tenants: %s, %.0f calls/s", elapsed, 1000/elapsed.Seconds())
	if _, err := r.TenantKey(t.Context(), "key_org_first", "org_second"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cross tenant accepted: %v", err)
	}
	if err := r.RevokeKey(t.Context(), "owner", "key_org_first"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TenantKey(t.Context(), "key_org_first", "org_first"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if _, err := r.TenantKey(t.Context(), "key_org_second", "org_second"); err != nil {
		t.Fatalf("revocation leaked tenants: %v", err)
	}
}

func TestTenantKeyPolicyAndOAuthSnapshot(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			id := "key_" + string(org)
			if err := r.CreateKey(t.Context(), Key{ID: id, Hash: "hash_" + id, Name: "Example", Kind: "personal", OwnerSubject: "owner", Permission: "admin", AccessContext: "global"}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.TenantKey(t.Context(), id, org); err != nil {
				t.Fatal(err)
			}
			if err := r.SetPersonalKeyPolicy(t.Context(), org, "approval"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.TenantKey(t.Context(), id, org); !errors.Is(err, ErrNoSession) {
				t.Fatalf("unapproved key accepted: %v", err)
			}
			if err := r.ApproveKey(t.Context(), org, id); err != nil {
				t.Fatal(err)
			}
			if err := r.BlockKey(t.Context(), org, id, "prj_excluded"); err != nil {
				t.Fatal(err)
			}
			snapshot, err := r.TenantKey(t.Context(), id, org)
			if err != nil || snapshot.Authority.Allows("prj_excluded") || !snapshot.Authority.Allows("prj_other") {
				t.Fatalf("exclusions lost: %v", err)
			}
			if err := r.SetPersonalKeyPolicy(t.Context(), org, "blocked"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.TenantKey(t.Context(), id, org); !errors.Is(err, ErrNoSession) {
				t.Fatalf("policy block ignored: %v", err)
			}
			if err := r.SetPersonalKeyPolicy(t.Context(), org, "allowed"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO platform.access_key_blocked_projects(organization_id,key_id,project_id) SELECT $1,$2,'prj_'||n FROM generate_series(1,201) AS n`, org, id); err != nil {
				t.Fatal(err)
			}
			if _, err := r.TenantKey(t.Context(), id, org); !errors.Is(err, tenantauth.ErrDenied) {
				t.Fatalf("overflowing exclusions accepted: %v", err)
			}
			grant, request := createOAuthGrant(t, r, org, "oauth_grant_"+string(org))
			tokens := oauthTokens(r, string(org))
			exchange(t, r, grant, request, tokens)
			snapshot, err = r.TenantKey(t.Context(), grant.ID, org)
			if err != nil || snapshot.Key.Hash != tokens.AccessHash || snapshot.Resource != grant.Resource || snapshot.Key.OwnerSubject != grant.OwnerSubject {
				t.Fatalf("OAuth snapshot lost authority: %v", err)
			}
			if err := r.RevokeOAuthGrant(t.Context(), org, grant.OwnerSubject, grant.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := r.TenantKey(t.Context(), grant.ID, org); !errors.Is(err, ErrNoSession) {
				t.Fatalf("revoked OAuth grant accepted: %v", err)
			}
		})
	}
}

func TestTenantKeyRejectsAmbiguousOAuthIdentity(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.organizations SET managed=true`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		grant, request := createOAuthGrant(t, r, org, "oauth_grant_duplicate")
		exchange(t, r, grant, request, oauthTokens(r, string(org)))
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := r.TenantKey(t.Context(), "oauth_grant_duplicate", org); !errors.Is(err, ErrNoSession) {
			t.Fatalf("ambiguous OAuth ID accepted in %s: %v", org, err)
		}
	}
}
