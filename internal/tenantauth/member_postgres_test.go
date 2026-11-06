package tenantauth_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestPersonalAuthorityUsesCurrentMembershipAndPrincipal(t *testing.T) {
	db, pools := fixture(t)
	authority := tenantauth.New(pools, nil)
	for _, org := range []string{"first", "second"} {
		for _, statement := range []string{
			`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'principal','principal','principal-proof','fixture','operator',true,'2026-01-01','2026-01-01')`,
			`UPDATE api_tokens SET hosted_user_id='user',hosted_membership_id='membership',key_kind='personal' WHERE organization_id=$1 AND id='same'`,
			`INSERT INTO hosted_members VALUES($1,'user','user@example.test','membership','member',true,'principal',true,'2026-01-01','2026-01-01')`,
		} {
			if _, err := db.ExecContext(t.Context(), statement, org); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, test := range []struct {
		name, change, role string
		denied             bool
	}{
		{name: "current member", role: "member"},
		{name: "inactive member", change: `UPDATE hosted_members SET active=false WHERE organization_id='first'`, denied: true},
		{name: "membership mismatch", change: `UPDATE hosted_members SET membership_id='replacement' WHERE organization_id='first'`, denied: true},
		{name: "revoked principal", change: `UPDATE api_tokens SET revoked_at='2026-11-06' WHERE organization_id='first' AND id='principal'`, denied: true},
		{name: "current downgraded role", change: `UPDATE hosted_members SET role='viewer' WHERE organization_id='first'`, role: "viewer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.ExecContext(t.Context(), `UPDATE hosted_members SET active=true,membership_id='membership',role='member' WHERE organization_id='first'; UPDATE api_tokens SET revoked_at=NULL WHERE organization_id='first'`); err != nil {
				t.Fatal(err)
			}
			if test.change != "" {
				if _, err := db.ExecContext(t.Context(), test.change); err != nil {
					t.Fatal(err)
				}
			}
			scope, err := authority.Resolve(t.Context(), tenantauth.RequestIdentity{Organization: "first", CredentialHash: "same-proof"})
			if errors.Is(err, tenantauth.ErrDenied) != test.denied {
				t.Fatalf("Resolve()=%v, denied=%v", err, test.denied)
			}
			if !test.denied && (scope.Principal().Role != test.role || scope.Principal().PrincipalID != "principal" || scope.Principal().UserID != "user") {
				t.Fatalf("unexpected principal: %+v", scope.Principal())
			}
			second, err := authority.Resolve(t.Context(), tenantauth.RequestIdentity{Organization: "second", CredentialHash: "same-proof"})
			if err != nil || second.Principal().Role != "member" || second.Principal().ProviderID != "second" {
				t.Fatalf("other tenant changed: principal=%+v err=%v", second.Principal(), err)
			}
		})
	}
}
