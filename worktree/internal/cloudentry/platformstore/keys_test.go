package platformstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestStoredKeyCorruptionIsDenied(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		key := Key{ID: "key_" + string(organization), Hash: "hash_" + string(organization), Name: "Example", Kind: "service", OwnerSubject: "owner", Permission: "read", AccessContext: "selected", ServiceOrganizationID: &organization, Organizations: []KeyOrganization{{OrganizationID: organization, ProjectAccess: "all"}}}
		if err := r.CreateKey(t.Context(), key); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct{ name, query string }{
			{"owner", `UPDATE platform.access_keys SET owner_subject='' WHERE id=$1`},
			{"project context", `UPDATE platform.access_key_organizations SET project_access='selected' WHERE key_id=$1`},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				if _, err := db.ExecContext(t.Context(), test.query, key.ID); err != nil {
					t.Fatal(err)
				}
				stored, err := r.Key(t.Context(), key.Hash)
				if !errors.Is(err, tenantauth.ErrDenied) || errors.Is(err, tenantauth.ErrUnavailable) || stored.ID != "" {
					t.Fatalf("corrupt stored key must be a definite denial without partial credentials: id=%q err=%v", stored.ID, err)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE platform.access_keys SET owner_subject='owner' WHERE id=$1`, key.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE platform.access_key_organizations SET project_access='all' WHERE key_id=$1`, key.ID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Key(ctx, "hash_org_first"); !errors.Is(err, context.Canceled) || !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, tenantauth.ErrDenied) {
		t.Fatalf("cancelled key query must remain an outage: %v", err)
	}
}

func TestKeyPermissionsAndTenantScopes(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	for _, permission := range []string{"read", "write", "admin"} {
		for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
			t.Run(permission+"/"+string(organization), func(t *testing.T) {
				for _, suffix := range []string{"one", "two"} {
					id := permission + "_" + string(organization) + "_" + suffix
					k := Key{ID: id, Hash: "hash_" + id, Name: "Example", Kind: "service", OwnerSubject: "owner", OwnerEmail: "owner@example.com", Permission: permission, AccessContext: "project", ServiceOrganizationID: &organization, Organizations: []KeyOrganization{{OrganizationID: organization, ProjectAccess: "project", Projects: []tracker.ProjectID{"prj_shared"}}}}
					if err := r.CreateKey(t.Context(), k); err != nil {
						t.Fatal(err)
					}
					authority, err := r.KeyAuthority(t.Context(), k.Hash, organization, "prj_shared")
					if err != nil || string(authority.Permission) != permission || authority.ID != id {
						t.Fatalf("authority: %+v %v", authority, err)
					}
					digest := sha256.Sum256([]byte("detent-entry-coordinator-credential-v1\x00" + k.Hash))
					if authority.CredentialBinding != hex.EncodeToString(digest[:]) {
						t.Fatal("credential binding changed")
					}
					other := tracker.OrganizationID("org_first")
					if organization == other {
						other = "org_second"
					}
					if _, err := r.KeyAuthority(t.Context(), k.Hash, other, "prj_shared"); !errors.Is(err, ErrNoSession) {
						t.Fatalf("cross tenant key: %v", err)
					}
					if _, err := r.KeyAuthority(t.Context(), k.Hash, organization, "prj_other"); !errors.Is(err, ErrNoSession) {
						t.Fatalf("wrong project key: %v", err)
					}
					if err := r.BlockKey(t.Context(), other, k.ID, ""); err != nil {
						t.Fatal(err)
					}
					if _, err := r.KeyAuthority(t.Context(), k.Hash, organization, "prj_shared"); err != nil {
						t.Fatalf("cross tenant block affected key: %v", err)
					}
					if err := r.BlockKey(t.Context(), organization, k.ID, "prj_shared"); err != nil {
						t.Fatal(err)
					}
					if _, err := r.KeyAuthority(t.Context(), k.Hash, organization, "prj_shared"); !errors.Is(err, ErrNoSession) {
						t.Fatalf("blocked project authorized: %v", err)
					}
					var notices int
					if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM platform.access_key_notifications WHERE key_id=$1 AND organization_id=$2 AND project_id=$3", id, organization, "prj_shared").Scan(&notices); err != nil || notices != 1 {
						t.Fatalf("blocked key notification: count=%d error=%v", notices, err)
					}
					stored, err := r.Key(t.Context(), k.Hash)
					if err != nil || stored.OwnerSubject != k.OwnerSubject || len(stored.Organizations) != 1 || len(stored.Organizations[0].Projects) != 1 || stored.Organizations[0].Projects[0] != "prj_shared" {
						t.Fatalf("block changed selected scope: %+v %v", stored, err)
					}

				}
			})
		}
	}
}

func TestPersonalKeyPolicyAndRevocation(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	for _, id := range []string{"first", "second"} {
		k := Key{ID: id, Hash: "hash_" + id, Name: "Example", Kind: "personal", OwnerSubject: "owner_" + id, OwnerEmail: "owner@example.com", Permission: "write", AccessContext: "global"}
		if err := r.CreateKey(t.Context(), k); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, policy   string
		approve, block bool
		want           error
	}{
		{"allowed", "allowed", false, false, nil}, {"approval required", "approval", false, false, ErrNoSession}, {"approved", "approval", true, false, nil}, {"blocked organization", "blocked", false, false, ErrNoSession}, {"project exclusion", "allowed", false, true, ErrNoSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.SetPersonalKeyPolicy(t.Context(), "org_first", tc.policy); err != nil {
				t.Fatal(err)
			}
			if tc.approve {
				if err := r.ApproveKey(t.Context(), "org_first", "first"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.block {
				if err := r.BlockKey(t.Context(), "org_first", "first", "prj_shared"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.KeyAuthority(t.Context(), "hash_first", "org_first", "prj_shared"); !errors.Is(err, tc.want) {
				t.Fatalf("authority: %v", err)
			}
			if _, err := r.KeyAuthority(t.Context(), "hash_first", "org_second", "prj_shared"); err != nil {
				t.Fatalf("unrelated tenant: %v", err)
			}
		})
	}
	if err := r.RevokeKey(t.Context(), "wrong_owner", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.KeyAuthority(t.Context(), "hash_second", "org_second", "prj_shared"); err != nil {
		t.Fatalf("wrong owner revoked key: %v", err)
	}
	if err := r.RevokeKey(t.Context(), "owner_first", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.KeyAuthority(t.Context(), "hash_first", "org_second", "prj_shared"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := r.KeyAuthority(t.Context(), "hash_second", "org_second", "prj_shared"); err != nil {
		t.Fatalf("unrelated key: %v", err)
	}
}

func TestGlobalKeyRetainsAnExplicitProjectScope(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	k := Key{ID: "example", Hash: "example_hash", Name: "Example", Kind: "personal", OwnerSubject: "owner", Permission: "read", AccessContext: "global", Organizations: []KeyOrganization{{OrganizationID: "org_first", ProjectAccess: "project", Projects: []tracker.ProjectID{"prj_selected"}}}}
	if err := r.CreateKey(t.Context(), k); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		organization tracker.OrganizationID
		project      tracker.ProjectID
		want         error
	}{
		{"explicit selection", "org_first", "prj_selected", nil},
		{"explicit exclusion", "org_first", "prj_other", ErrNoSession},
		{"global fallback", "org_second", "prj_other", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.KeyAuthority(t.Context(), k.Hash, tc.organization, tc.project); !errors.Is(err, tc.want) {
				t.Fatalf("authority: %v", err)
			}
		})
	}
	if err := r.ApproveKey(t.Context(), "org_second", k.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := r.Key(t.Context(), k.Hash)
	if err != nil || len(stored.Organizations) != 1 || stored.Organizations[0].OrganizationID != "org_first" {
		t.Fatalf("approval changed explicit scope: %+v %v", stored, err)
	}
}
