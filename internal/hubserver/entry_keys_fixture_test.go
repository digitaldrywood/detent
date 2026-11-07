package hubserver

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryKeyFixture struct {
	hostedSharedFixture
	admin *sql.DB
}

func newEntryKeyFixture(t *testing.T, configure ...func(*Config)) entryKeyFixture {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted.OrganizationID, cfg.Hosted.WorkOSOrganizationID = "org_security", "provider_org_security"
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	plans := pilotHostedPlans()
	cfg.Hosted.Plans = &plans
	key := hostedSharedKey(7)
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	for _, apply := range configure {
		apply(&cfg)
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	project := tracker.ProjectID("prj_security")
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	for _, organization := range []tracker.OrganizationID{"org_security", "org_other"} {
		seedStartupOrganization(t, store.postgres, organization)
		now := time.Now().UTC()
		envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{7}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: states}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.db != nil || service.database.reader != nil {
		t.Fatal("entry key fixture must use PostgreSQL only")
	}
	for _, organization := range []string{"org_security", "org_other"} {
		seedNativeFixturePolicy(t, service, organization+"/"+string(project))
	}
	return entryKeyFixture{hostedSharedFixture: hostedSharedFixture{hostedSecurityFixture: hostedSecurityFixture{service: service, project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}, key: key}, admin: admin}
}

func (f entryKeyFixture) user(t *testing.T, role string) hostedSecurityUser {
	t.Helper()
	identity := auth.Identity{Subject: "user_key_owner", Email: "key-owner@example.test"}
	for _, organization := range []tracker.OrganizationID{"org_security", "org_other"} {
		now := f.service.config.now()
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			id := "credential_" + identity.Subject
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: "Example owner", Hash: apikey.HashToken(string(organization) + id), Scope: "operator", NativeOnly: true, UserID: identity.Subject, MembershipID: identity.Subject, ProjectAccess: "all", KeyScope: "write", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: identity.Subject, Email: identity.Email, MembershipID: identity.Subject, PrincipalID: id, Role: role, Active: true, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: f.project}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: identity.Subject, ProjectID: f.project, CanWrite: true})
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.assertOtherTenant(t)
	return hostedSecurityUser{identity: identity}
}

func (f entryKeyFixture) assertOtherTenant(t *testing.T) {
	t.Helper()
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_other", corestore.Serving, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(t.Context(), "user_key_owner")
		if err != nil {
			return err
		}
		project, err := r.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		grant, err := r.AccessProjectGrant(t.Context(), member.UserID, f.project)
		if err != nil {
			return err
		}
		if !member.Active || project.Core.DeletedAt != nil || !grant.CanWrite {
			return errors.New("other tenant access was changed")
		}
		_, err = r.AccessCredential(t.Context(), member.PrincipalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f entryKeyFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f entryKeyFixture) rejectAudit(t *testing.T, event, condition string) {
	t.Helper()
	f.exec(t, `CREATE OR REPLACE FUNCTION reject_key_audit_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
	f.exec(t, "CREATE TRIGGER reject_key_audit BEFORE "+event+" ON hosted_audit FOR EACH ROW WHEN (NEW.organization_id='org_security' AND NEW.event='key_call'"+condition+") EXECUTE FUNCTION reject_key_audit_write()")
}
