package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAccessAccountReceiptsAndProviderBindings(t *testing.T) {
	t.Run("local release preferences retain current credential authority", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		now := pgstoretest.HistoryAt()
		for orgIndex, org := range []tracker.OrganizationID{"org_release_first", "org_release_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			for actorIndex, actor := range []string{"first", "second"} {
				hash := apikey.HashToken(string(org) + actor)
				user := "local_" + actor
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: actor, Name: actor, Hash: hash, Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now.Add(-time.Hour), UpdatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := repository.LocalReleasePreference(t.Context(), org, actor, "wrong", now); !errors.Is(err, accessstore.ErrDenied) {
					t.Fatalf("missing preference bypassed authentication: %v", err)
				}
				if _, err := repository.LocalReleasePreference(t.Context(), org, actor, hash, now); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("new local baseline: %v", err)
				}
				preference := accountstore.ReleasePreference{Version: fmt.Sprintf("v%d.%d.0", orgIndex+1, actorIndex+1), BuildTime: now.Add(time.Duration(orgIndex*2+actorIndex) * time.Minute)}
				if err := repository.SetLocalReleasePreference(t.Context(), org, actor, hash, now, preference); !errors.Is(err, accessstore.ErrDenied) {
					t.Fatalf("write fabricated local member: %v", err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.AccessMember(t.Context(), user); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("read fabricated local member: %v", err)
					}
					return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: actor + "@example.test", MembershipID: user, PrincipalID: actor, Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.SetLocalReleasePreference(t.Context(), org, actor, hash, now, accountstore.ReleasePreference{Version: "initial", BuildTime: preference.BuildTime.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
				if err := repository.SetLocalReleasePreference(t.Context(), org, actor, hash, now, preference); err != nil {
					t.Fatal(err)
				}
				if err := repository.SetLocalReleasePreference(t.Context(), org, actor, hash, now, accountstore.ReleasePreference{Version: "older", BuildTime: preference.BuildTime.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
				got, err := repository.LocalReleasePreference(t.Context(), org, actor, hash, now)
				if err != nil || got.Version != preference.Version || !got.BuildTime.Equal(preference.BuildTime) {
					t.Fatalf("local monotonic preference=%+v want=%+v error=%v", got, preference, err)
				}
			}
		}
		for orgIndex, org := range []tracker.OrganizationID{"org_release_first", "org_release_second"} {
			for actorIndex, actor := range []string{"first", "second"} {
				hash := apikey.HashToken(string(org) + actor)
				original, err := repository.LocalReleasePreference(t.Context(), org, actor, hash, now)
				if err != nil {
					t.Fatal(err)
				}
				if original.Version != fmt.Sprintf("v%d.%d.0", orgIndex+1, actorIndex+1) || !original.BuildTime.Equal(now.Add(time.Duration(orgIndex*2+actorIndex)*time.Minute)) {
					t.Fatalf("other local account overwrote preference: %+v", original)
				}
				for _, test := range []struct{ name, query string }{
					{"rotated hash", "UPDATE api_tokens SET token_hash='rotated_'||$2,updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"revoked", "UPDATE api_tokens SET revoked_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"expired", "UPDATE api_tokens SET expires_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"not yet valid", "UPDATE api_tokens SET created_at=$3::timestamptz+interval '1 second' WHERE organization_id=$1 AND id=$2"},
					{"worker scope", "UPDATE api_tokens SET scope='worker',updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"operator scope", "UPDATE api_tokens SET scope='operator',updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"native only", "UPDATE api_tokens SET native_only=true,updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"key scope", "UPDATE api_tokens SET operator_key_scope='read',updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"hosted user", "UPDATE api_tokens SET hosted_user_id='hosted',updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"hosted membership", "UPDATE api_tokens SET hosted_membership_id='hosted',updated_at=$3 WHERE organization_id=$1 AND id=$2"},
					{"inactive member", "UPDATE hosted_members SET active=false,updated_at=$3 WHERE organization_id=$1 AND user_id='local_'||$2"},
					{"member identity", "UPDATE hosted_members SET membership_id='other',updated_at=$3 WHERE organization_id=$1 AND user_id='local_'||$2"},
					{"member principal", "UPDATE hosted_members SET principal_id=CASE WHEN $2='first' THEN 'second' ELSE 'first' END,updated_at=$3 WHERE organization_id=$1 AND user_id='local_'||$2"},
					{"member role", "UPDATE hosted_members SET role='viewer',updated_at=$3 WHERE organization_id=$1 AND user_id='local_'||$2"},
				} {
					t.Run(string(org)+"/"+actor+"/"+test.name, func(t *testing.T) {
						if _, err := admin.ExecContext(t.Context(), test.query, org, actor, now); err != nil {
							t.Fatal(err)
						}
						if _, err := repository.LocalReleasePreference(t.Context(), org, actor, hash, now); !errors.Is(err, accessstore.ErrDenied) {
							t.Fatalf("stale local read authority: %v", err)
						}
						if err := repository.SetLocalReleasePreference(t.Context(), org, actor, hash, now, accountstore.ReleasePreference{Version: "unauthorized", BuildTime: now.Add(time.Hour)}); !errors.Is(err, accessstore.ErrDenied) {
							t.Fatalf("stale local write authority: %v", err)
						}
						if _, err := admin.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=NULL,expires_at=NULL,created_at=$3::timestamptz-interval '1 hour',scope='admin',native_only=false,operator_key_scope='',hosted_user_id='',hosted_membership_id='',token_hash=$4 WHERE organization_id=$1 AND id=$2", org, actor, now, hash); err != nil {
							t.Fatal(err)
						}
						if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET active=true,membership_id='local_'||$2,principal_id=$2,role='admin',updated_at=$3 WHERE organization_id=$1 AND user_id='local_'||$2", org, actor, now); err != nil {
							t.Fatal(err)
						}
						got, err := repository.LocalReleasePreference(t.Context(), org, actor, hash, now)
						if err != nil || got.Version != original.Version || !got.BuildTime.Equal(original.BuildTime) {
							t.Fatalf("denied write changed preference=%+v error=%v", got, err)
						}
					})
				}
			}
			pgstoretest.SeedRunnerIdentity(t, repository, org, "bound_runner", "machine_release", now, true)
			if _, err := admin.ExecContext(t.Context(), "UPDATE api_tokens SET scope='admin',native_only=false WHERE organization_id=$1 AND id='bound_runner'", org); err != nil {
				t.Fatal(err)
			}
			hash := apikey.HashToken("credential_" + string(org) + "_bound_runner")
			if _, err := repository.LocalReleasePreference(t.Context(), org, "bound_runner", hash, now); !errors.Is(err, accessstore.ErrDenied) {
				t.Fatalf("runner acquired local read authority: %v", err)
			}
			if err := repository.SetLocalReleasePreference(t.Context(), org, "bound_runner", hash, now, accountstore.ReleasePreference{Version: "runner", BuildTime: now}); !errors.Is(err, accessstore.ErrDenied) {
				t.Fatalf("runner acquired local write authority: %v", err)
			}
		}
		if _, err := repository.LocalReleasePreference(t.Context(), "org_release_second", "first", apikey.HashToken("org_release_firstfirst"), now); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("cross-tenant credential accepted: %v", err)
		}
	})
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: pgstoretest.HistoryAt()}); err != nil {
				return err
			}
			owner := w
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org}); err != nil {
				return err
			}
			if err := owner.BindAccessTenantProvider(t.Context(), "provider_"+string(org)); err != nil {
				return err
			}
			if err := owner.BindAccessTenantProvider(t.Context(), "provider_"+string(org)); err != nil {
				return err
			}
			if err := owner.BindAccessTenantProvider(t.Context(), "other"); err == nil {
				t.Error("provider binding overwritten")
			}
			for _, event := range []string{"pending", "succeeded"} {
				if _, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, ActualActor: "actor", Event: event, AccountKind: "account_administration", AccountRetry: "retry", RecordedAt: pgstoretest.HistoryAt()}); err != nil {
					return err
				}
			}
			row, err := owner.AccessAccountAudit(t.Context(), "actor", "retry")
			if err != nil {
				return err
			}
			if row.OrganizationID != org || row.Event != "succeeded" {
				t.Errorf("receipt=%+v", row)
			}
			if _, err := owner.AccessAccountAudit(t.Context(), "another-actor", "retry"); !errors.Is(err, corestore.ErrNotFound) {
				t.Errorf("foreign actor error=%v", err)
			}

			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}); err != nil {
				return err
			}
			if missing, err := w.AccessMissingUsageGrant(t.Context(), "actor"); err != nil || !missing {
				t.Fatalf("missing grant=%v error=%v", missing, err)
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "actor", Name: "Actor", Hash: "actor-hash", Scope: "operator", NativeOnly: true, CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt(), ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "actor", Email: "actor@example.test", MembershipID: "membership", Role: "admin", Active: true, PrincipalID: "actor", CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "actor", ProjectID: "prj_example"}); err != nil {
				return err
			}
			if missing, err := w.AccessMissingUsageGrant(t.Context(), "actor"); err != nil || missing {
				t.Fatalf("granted access missing=%v error=%v", missing, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
