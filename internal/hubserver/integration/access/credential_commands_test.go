package access_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOrganizationCreationWithoutLegacyDatabase(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	var previous []byte
	for _, name := range []string{"First example", "Second example"} {
		response := h.Request(t, http.MethodPost, "/api/v2/organizations", h.AdminToken(), map[string]string{"name": name})
		hubfixture.RequireStatus(t, response, http.StatusCreated)
		var organization struct {
			ID   tracker.OrganizationID `json:"organization_id"`
			Slug string                 `json:"slug"`
		}
		hubfixture.Decode(t, response, &organization)
		if organization.ID == "" || organization.Slug == "" {
			t.Fatalf("incomplete organization: %+v", organization)
		}
		err := h.Repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.AccessOrganization(t.Context())
			if err != nil {
				return err
			}
			if stored.Name != name || stored.Slug != organization.Slug {
				t.Fatalf("organization = %+v, want name %q and slug %q", stored, name, organization.Slug)
			}
			secret, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
			if err != nil {
				return err
			}
			key, err := accessstore.OpenSecret(h.Config.SecretKeys, secret)
			if err != nil {
				return err
			}
			if len(key) != 32 || bytes.Equal(previous, key) {
				t.Fatal("organization cursor key is invalid or reused")
			}
			previous = key
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresLocalOrganizationCursorKey(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	connection := hubtest.PostgresConfig(t, admin)
	cfg := hubfixture.Config{
		DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection,
		SecretKeys: hubfixture.SecretKeys(t), InitialAdminToken: []byte(hubfixture.AdminToken),
		Logger: hubfixture.DiscardLogger(),
	}
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	repository := hubtest.RepositoryOn(t, admin)
	org, err := repository.DesignatedLocalOrganization(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		secret, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
		if err != nil {
			return err
		}
		key, err := accessstore.OpenSecret(cfg.SecretKeys, secret)
		if err == nil && len(key) != 32 {
			t.Fatalf("bootstrap cursor key length = %d, want 32", len(key))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresCredentialMembershipAndKeyAudit(t *testing.T) {
	t.Parallel()
	f := openEntryCatalog(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range f.organizations {
		organization := tracker.OrganizationID(org)
		err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []string{"instance", "member"} {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: org + id, Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "example", Email: "example@example.test", MembershipID: "membership", Role: "admin", Active: false, PrincipalID: "member", CreatedAt: now, UpdatedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
		err = f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, test := range []struct {
				id     string
				member bool
			}{{"instance", false}, {"member", true}} {
				got, err := r.AccessCredentialHasMember(t.Context(), test.id)
				if err != nil {
					return err
				}
				if got != test.member {
					t.Fatalf("%s/%s member = %v, want %v", org, test.id, got, test.member)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		response := f.request(t, org, "/internal/v1/keys/audit", `{"key_id":"key_example","owner":"owner_example","event":"key_revoked"}`, func(claims *cloudassert.Claims) {
			claims.Kind = cloudassert.KindService
		})
		hubfixture.RequireStatus(t, response, http.StatusNoContent)
		var owner, event, key string
		err = f.admin.QueryRowContext(t.Context(), `SELECT key_owner,event,key_id FROM hosted_audit WHERE organization_id=$1 AND key_id=$2`, org, "key_example").Scan(&owner, &event, &key)
		if err != nil {
			t.Fatal(err)
		}
		if owner != "owner_example" || event != "key_revoked" || key != "key_example" {
			t.Fatalf("organization %s key audit = %q %q %q", org, owner, event, key)
		}
	}
}
