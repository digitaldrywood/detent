package postgres_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv/pgtest"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedTenantAccess(t *testing.T, r tenantstore.Repository, org tracker.OrganizationID) {
	t.Helper()
	now := historyAt()
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

func TestAccessTenantSchemaEnforcesIsolation(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	for i, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		serverID := fmt.Sprintf("hub_%032x", i+1)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: serverID, CursorKey: bytes.Repeat([]byte{byte(i + 1)}, 32)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM hosted_members`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound access count=%d error=%v", count, err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			identity, err := r.HubIdentity(t.Context())
			if err != nil {
				return err
			}
			if identity.OrganizationID != org || len(identity.CursorKey) != 32 {
				t.Fatalf("Hub identity escaped tenant")
			}
			for _, id := range []string{"first", "second"} {
				member, err := r.AccessMember(t.Context(), id)
				if err != nil {
					return err
				}
				if member.OrganizationID != org || !member.Active {
					t.Fatalf("member escaped tenant")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_second"})
	})
	if !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("cross-tenant write=%v", err)
	}
	platform := rolePool(t, admin, "detent_platform_v2", "")
	defer platform.Close()
	if _, err := platform.Exec(pgtest.UnboundProbe(t.Context()), `SELECT * FROM api_tokens`); err == nil {
		t.Fatal("platform authority read tenant credentials")
	}
	for _, table := range []string{"api_tokens", "hub_server_identities", "organization_slugs", "hosted_work_view_preferences", "hosted_work_views", "hosted_work_view_values", "hosted_work_view_projects", "hosted_work_view_favorites", "native_board_items", "native_board_filter_values", "native_board_counts", "native_board_sequences", "native_board_deltas", "native_board_dirty", "native_board_access_revisions"} {
		var allowed bool
		if err := platform.QueryRow(t.Context(), `SELECT has_table_privilege(current_user,$1,'SELECT,INSERT,UPDATE,DELETE')`, table).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Errorf("platform authority can access tenant table %s", table)
		}
	}
}

func TestAccessCurrentAuthority(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	seedTenantAccess(t, repository, "org_first")
	now := historyAt()
	base := accessstore.Authority{CredentialID: "first", Hash: "hash_first", UserID: "first", MembershipID: "membership_first", Write: true, Admin: true}
	tests := []struct {
		name   string
		change func(*accessstore.Authority)
		denied bool
	}{
		{name: "current"},
		{name: "hash", change: func(a *accessstore.Authority) { a.Hash = "wrong" }, denied: true},
		{name: "membership", change: func(a *accessstore.Authority) { a.MembershipID = "old" }, denied: true},
		{name: "other member", change: func(a *accessstore.Authority) { a.UserID = "second"; a.MembershipID = "membership_second" }, denied: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority := base
			if test.change != nil {
				test.change(&authority)
			}
			err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.AuthorizeAccess(t.Context(), authority, now); return err })
			if (err != nil) != test.denied {
				t.Fatalf("authorization error=%v denied=%v", err, test.denied)
			}
		})
	}
	for _, role := range []string{"owner", "admin", "member", "reporter", "viewer"} {
		t.Run(role, func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				m, err := w.AccessMember(t.Context(), "first")
				if err != nil {
					return err
				}
				m.Role = role
				return w.UpdateAccessMember(t.Context(), m)
			}); err != nil {
				t.Fatal(err)
			}
			err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.AuthorizeAccess(t.Context(), base, now); return err })
			if (err == nil) != (role == "owner" || role == "admin") {
				t.Fatalf("role %s authorization=%v", role, err)
			}
		})
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), "first", now) }); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.AuthorizeAccess(t.Context(), base, now); return err }); !errors.Is(err, accessstore.ErrDenied) || errors.Is(err, tenantauth.ErrUnavailable) {
		t.Fatalf("revoked credential=%v", err)
	}
}

func TestAccessRevokedCredentialCannotClaim(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedLeaseFamily(t, repository, org)
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), "first", historyAt()) }); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.AuthorizeAccess(t.Context(), accessstore.Authority{CredentialID: "first", Hash: "hash_first"}, historyAt()); err != nil {
				return err
			}
			_, err := w.Claim(t.Context(), leaseClaim(1))
			return err
		})
		if org == "org_first" && !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("revoked claim=%v", err)
		}
		if org == "org_second" && err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error { _, err := r.Lease(t.Context(), "lease_1"); return err }); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("revoked claim created lease: %v", err)
	}
}

func accessTestKeys(t *testing.T, retain bool, activeVersion ...string) *hubsecrets.Keyring {
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

func TestAccessImportedEnvelopesAndAtomicAudit(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	keys := accessTestKeys(t, true)
	secrets := map[tracker.OrganizationID]accessstore.Secret{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)
		envelope, err := keys.Seal([]byte("example credential"), accessstore.SecretAAD(org, "prj_first", "example"))
		if err != nil {
			t.Fatal(err)
		}
		secret := accessstore.Secret{OrganizationID: org, ProjectID: "prj_first", Kind: "example", OrganizationSlug: "example", Envelope: envelope, UpdatedAt: historyAt()}
		secrets[org] = secret
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), secret, accessstore.SecretAudit{OrganizationID: org, ProjectID: secret.ProjectID, Kind: secret.Kind, Actor: "first", Event: "import", KeyVersion: envelope.Version, RecordedAt: historyAt()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for org, want := range secrets {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.AccessSecret(t.Context(), want.ProjectID, want.Kind)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got.Envelope, want.Envelope) {
				t.Fatal("import changed envelope bytes")
			}
			value, err := accessstore.OpenSecret(keys, got)
			if err != nil {
				return err
			}
			if string(value) != "example credential" {
				t.Fatal("import changed plaintext")
			}
			if err := accessstore.VerifySecret(accessTestKeys(t, false), got); !errors.Is(err, hubsecrets.ErrUnavailable) {
				t.Fatalf("missing retained key=%v", err)
			}
			got.OrganizationID = "org_other"
			if err := accessstore.VerifySecret(keys, got); !errors.Is(err, hubsecrets.ErrInvalid) {
				t.Fatalf("organization mismatch=%v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rollback := errors.New("rollback")
	err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		secret := secrets["org_first"]
		secret.ProjectID = "prj_second"
		if err := w.StoreAccessSecret(t.Context(), secret, accessstore.SecretAudit{OrganizationID: secret.OrganizationID, ProjectID: secret.ProjectID, Kind: secret.Kind, Actor: "first", Event: "set", KeyVersion: secret.Envelope.Version, RecordedAt: historyAt()}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.AccessSecret(t.Context(), "prj_second", "example")
		return err
	}); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("rollback persisted secret: %v", err)
	}
	var count int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM project_secret_audit WHERE organization_id='org_first'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit rollback count=%d error=%v", count, err)
	}
}

func TestAccessSessionTombstoneAndTransactionReplay(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)
		now := historyAt()
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.RevokeAccessSession(t.Context(), "binding", now); err != nil {
				return err
			}
			if err := w.EnsureAccessSession(t.Context(), accessstore.Session{OrganizationID: org, Hash: "binding", Subject: "first", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
			row, err := w.AccessSession(t.Context(), "binding")
			if err != nil {
				return err
			}
			if row.RevokedAt == nil {
				t.Fatal("replayed assertion resurrected revoked binding")
			}
			return w.CreateAccessLoginTransaction(t.Context(), accessstore.LoginTransaction{OrganizationID: org, Hash: "login", State: "state", ExpiresAt: now.Add(time.Hour)})
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"invitation_first", "invitation_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: org, ID: id, Email: "invited@example.com", Role: "member", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
					return err
				}
				return w.CreateAccessInvitationGrant(t.Context(), accessstore.InvitationGrant{OrganizationID: org, InvitationID: id, ProjectID: "prj_first", CanWrite: true})
			}); err != nil {
				t.Fatal(err)
			}
			for i, email := range []string{"other@example.com", "invited@example.com", "invited@example.com"} {
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.AcceptAccessInvitation(t.Context(), id, email, "first", now)
				})
				if (err == nil) != (i == 1) {
					t.Fatalf("invitation replay index=%d error=%v", i, err)
				}
			}
		}
		for index, state := range []string{"wrong", "state", "state"} {
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.ConsumeAccessTransaction(t.Context(), "login", state, now)
				return err
			})
			if (err == nil) != (index == 1) {
				t.Fatalf("consume index=%d error=%v", index, err)
			}
		}
	}
}

func TestAccessAuditReferencesAndRewrap(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	objects := newHistoryObjects()
	keys := accessTestKeys(t, true)
	active := accessTestKeys(t, true, "2")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		ref, err := objects.StoreDetail(t.Context(), string(org), []byte(`{"actor":"example"}`))
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := keys.Seal([]byte("example"), accessstore.SecretAAD(org, "", "example"))
		if err != nil {
			t.Fatal(err)
		}
		secret := accessstore.Secret{OrganizationID: org, Kind: "example", Envelope: envelope, UpdatedAt: historyAt()}
		audit := accessstore.SecretAudit{OrganizationID: org, Kind: "example", Actor: "first", Event: "set", KeyVersion: envelope.Version, RecordedAt: historyAt()}
		var ids []int64
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.StoreAccessSecret(t.Context(), secret, audit); err != nil {
				return err
			}
			for _, key := range []string{"first", "second"} {
				id, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, SessionID: "session", ActualActor: "first", EffectiveUser: "first", Event: "key_call", StartedAt: historyAt(), ExpiresAt: historyAt().Add(time.Hour), RecordedAt: historyAt(), RecordedAtNS: historyAt().UnixNano(), Mutation: &ref, KeyID: key, KeyKind: "personal", KeyOwner: "first"})
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if ids[0] == ids[1] {
			t.Fatal("audit identities collided")
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			for i, id := range ids {
				row, err := r.AccessAudit(t.Context(), id)
				if err != nil {
					return err
				}
				if row.OrganizationID != org || !reflect.DeepEqual(row.Mutation, &ref) || row.KeyID != []string{"first", "second"}[i] {
					t.Fatalf("audit reference lost: %+v", row)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		wrapped, err := active.Rewrap(secret.Envelope, accessstore.SecretAAD(org, "", secret.Kind))
		if err != nil {
			t.Fatal(err)
		}
		audit.Event, audit.KeyVersion = "rotate", wrapped.Version
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RewrapAccessSecret(t.Context(), secret, wrapped, audit) }); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RewrapAccessSecret(t.Context(), secret, wrapped, audit) }); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale rewrap=%v", err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.AccessSecret(t.Context(), "", secret.Kind)
			if err != nil {
				return err
			}
			if !bytes.Equal(got.Envelope.Ciphertext, secret.Envelope.Ciphertext) || !bytes.Equal(got.Envelope.Nonce, secret.Envelope.Nonce) {
				t.Fatal("rewrap changed ciphertext")
			}
			value, err := accessstore.OpenSecret(active, got)
			if err != nil {
				return err
			}
			if string(value) != "example" {
				t.Fatal("rewrap changed secret")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAccessLookupFailureIsUnavailable(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	authority := accessstore.Authority{CredentialID: "first", Hash: "hash_first", UserID: "first", MembershipID: "membership_first", SessionHash: "session", ProjectID: "prj_first", Write: true}
	for _, org := range organizations {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: org, Hash: authority.SessionHash, Subject: authority.UserID, CreatedAt: now, ExpiresAt: now.Add(time.Hour), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: authority.UserID, ProjectID: authority.ProjectID, CanWrite: true}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: authority.CredentialID, ProjectID: authority.ProjectID})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"api_tokens", "hosted_members", "hosted_sessions", "hosted_project_grants", "token_grants"} {
		t.Run(table, func(t *testing.T) {
			if _, err := admin.ExecContext(t.Context(), "REVOKE SELECT ON "+table+" FROM detent_runtime_v2"); err != nil {
				t.Fatal(err)
			}
			for _, org := range organizations {
				err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.AuthorizeAccess(t.Context(), authority, now)
					return err
				})
				if !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, accessstore.ErrDenied) {
					t.Errorf("tenant %s lookup error=%v, want unavailable without denial", org, err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), "GRANT SELECT ON "+table+" TO detent_runtime_v2"); err != nil {
				t.Fatal(err)
			}
			for _, org := range organizations {
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.AuthorizeAccess(t.Context(), authority, now)
					return err
				}); err != nil {
					t.Fatalf("tenant %s recovery error=%v", org, err)
				}
			}
		})
	}
}
