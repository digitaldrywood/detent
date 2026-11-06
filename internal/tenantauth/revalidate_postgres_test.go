package tenantauth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestRevalidationRejectsChangedAuthorityAndHoldsLocks(t *testing.T) {
	for _, test := range []struct{ name, change string }{
		{"revoked", `UPDATE api_tokens SET revoked_at='2026-11-06' WHERE organization_id='first'`},
		{"rotated", `UPDATE api_tokens SET token_hash='rotated' WHERE organization_id='first'`},
		{"expired", `UPDATE api_tokens SET expires_at='2026-11-06' WHERE organization_id='first'`},
		{"generation", `UPDATE hosted_tenant SET allocation_generation=2 WHERE organization_id='first'`},
		{"permission", `UPDATE api_tokens SET operator_key_scope='read' WHERE organization_id='first'`},
		{"unchanged", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, pools := fixture(t)
			authority := tenantauth.New(pools, func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) })
			scope, err := authority.Resolve(t.Context(), tenantauth.RequestIdentity{Organization: "first", CredentialHash: "same-proof"})
			if err != nil {
				t.Fatal(err)
			}
			if test.change != "" {
				if _, err := db.ExecContext(t.Context(), test.change); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := pools.Begin(t.Context(), pgtransport.Scope{Organization: "first"}, pgtransport.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tx.Rollback(t.Context()); err != nil {
					t.Error(err)
				}
			})
			current, err := authority.Revalidate(t.Context(), tx, scope)
			if test.change != "" {
				if !errors.Is(err, tenantauth.ErrDenied) {
					t.Fatalf("changed authority accepted: %v", err)
				}
				return
			}
			if err != nil || current.Organization() != "first" {
				t.Fatalf("revalidation: %v", err)
			}
			for _, table := range []string{"organizations", "api_tokens", "hosted_tenant"} {
				probe, err := db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				_, lockErr := probe.ExecContext(t.Context(), `SELECT organization_id FROM `+table+` WHERE organization_id='first' FOR UPDATE NOWAIT`)
				rollbackErr := probe.Rollback()
				if pgtransport.SQLState(lockErr) != pgtransport.LockTimeout || rollbackErr != nil {
					t.Fatalf("%s lock not retained: %v rollback=%v", table, lockErr, rollbackErr)
				}
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `SELECT id FROM api_tokens WHERE organization_id='first' FOR UPDATE NOWAIT`); err != nil {
				t.Fatalf("commit retained lock: %v", err)
			}
		})
	}
}
