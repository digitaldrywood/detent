package pgstoretest

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func SeedTenantAccess(t *testing.T, r tenantstore.Repository, org tracker.OrganizationID) {
	t.Helper()
	now := HistoryAt()
	err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.AccessOrganization(t.Context()); errors.Is(err, corestore.ErrNotFound) {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), AllocationGeneration: 1}); err != nil {
			return err
		}
		for _, id := range []string{"first", "second"} {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: id, Hash: "hash_" + id, Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: id, Email: id + "@example.com", MembershipID: "membership_" + id, Role: "admin", Active: true, PrincipalID: id, NotifyIssues: true, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func AccessTestKeys(t *testing.T, retain bool, activeVersion ...string) *hubsecrets.Keyring {
	t.Helper()
	raw := fmt.Sprintf(`{"2":%q}`, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	if retain {
		raw = fmt.Sprintf(`{"1":%q,"2":%q}`, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return raw
		}
		if name == "DETENT_HUB_SECRET_KEY_VERSION" {
			if len(activeVersion) > 0 {
				return activeVersion[0]
			}
			if retain {
				return "1"
			}
			return "2"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func SeedAccessTarget(t *testing.T, db *sql.DB, repository *postgres.Repository, organization tracker.OrganizationID) {
	t.Helper()
	createdAt := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,'Example organization','example',false,$2)`, organization, createdAt); err != nil {
		t.Fatal(err)
	}
	SeedRepository(t, repository, organization)
}
