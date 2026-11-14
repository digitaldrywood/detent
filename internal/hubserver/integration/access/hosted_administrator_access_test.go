package access_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestHostedAdministratorAccessRule(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	for _, user := range []struct{ name, role string }{{"owner", "owner"}, {"admin", "admin"}, {"member", "member"}, {"viewer", "viewer"}, {"inactive", "admin"}, {"revoked", "owner"}} {
		f.User(t, user.name, user.role, user.name+"@example.test", "", "")
	}
	principals := map[string]string{}
	if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		for _, name := range []string{"owner", "admin", "member", "viewer", "inactive", "revoked"} {
			member, err := w.AccessMember(t.Context(), "user_"+name)
			if err != nil {
				return err
			}
			principals[name] = member.PrincipalID
			if name == "inactive" {
				member.Active = false
				if err := w.UpdateAccessMember(t.Context(), member); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at=now() WHERE organization_id=$1 AND id=$2`, string(f.Primary), principals["revoked"]); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, organization, user, principal string
		want                                bool
	}{
		{"owner", string(f.Primary), "user_owner", principals["owner"], true},
		{"admin", string(f.Primary), "user_admin", principals["admin"], true},
		{"admin without principal filter", string(f.Primary), "user_admin", "", true},
		{"member", string(f.Primary), "user_member", principals["member"], false},
		{"viewer", string(f.Primary), "user_viewer", principals["viewer"], false},
		{"inactive admin", string(f.Primary), "user_inactive", principals["inactive"], false},
		{"revoked owner principal", string(f.Primary), "user_revoked", principals["revoked"], false},
		{"admin with another principal", string(f.Primary), "user_admin", principals["owner"], false},
		{"admin in another organization", "org_other", "user_admin", principals["admin"], false},
		{"empty user", string(f.Primary), "", "", false},
	}
	for _, test := range tests {
		var got bool
		if err := f.Admin.QueryRowContext(t.Context(), `SELECT hosted_administrator_access($1,$2,$3)`, test.organization, test.user, test.principal).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("%s: hosted_administrator_access=%t, want %t", test.name, got, test.want)
		}
	}
}
