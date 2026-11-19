package accessrecords_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAccessTenantSchemaEnforcesIsolation(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if organization, err := repository.DesignatedLocalOrganization(t.Context()); organization != "" || !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("empty database designation = %q, %v", organization, err)
	}
	for i, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		serverID := fmt.Sprintf("hub_%032x", i+1)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: serverID, CursorKey: bytes.Repeat([]byte{byte(i + 1)}, 32)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("credential metadata pages exclude runner bindings", func(t *testing.T) {
		now := pgstoretest.HistoryAt()
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, id := range []string{"00_runner", "page_m_runner", "zz_runner"} {
				pgstoretest.SeedRunnerIdentity(t, repository, org, id, "machine_"+id, now, true)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for _, id := range []string{"page_a", "page_z"} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: string(org) + " " + id, Hash: "hash_" + string(org) + "_" + id, Fingerprint: "fingerprint_" + string(org), Scope: "admin", CreatedAt: now, UpdatedAt: now, RotatedAt: &now, RevokedAt: &now, ExpiresAt: &now, ProjectAccess: "selected", Kind: "service"}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name          string
				offset, limit int
				want          []string
				more, invalid bool
			}{
				{name: "first filtered page", limit: 2, want: []string{"first", "page_a"}, more: true},
				{name: "exact final page", offset: 2, limit: 2, want: []string{"page_z", "second"}},
				{name: "offset after final page", offset: 4, limit: 2},
				{name: "inside filtered page", offset: 1, limit: 1, want: []string{"page_a"}, more: true},
				{name: "maximum page", limit: 200, want: []string{"first", "page_a", "page_z", "second"}},
				{name: "negative offset", offset: -1, limit: 2, invalid: true},
				{name: "oversized offset", offset: 100001, limit: 2, invalid: true},
				{name: "zero page", invalid: true},
				{name: "oversized page", limit: 201, invalid: true},
			} {
				t.Run(string(org)+"/"+test.name, func(t *testing.T) {
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(accessstore.CredentialMetadataPageReader)
						if !ok {
							t.Fatal("credential metadata page owner unavailable")
						}
						rows, more, err := reader.AccessCredentialMetadataPage(t.Context(), test.offset, test.limit)
						if test.invalid {
							if !errors.Is(err, corestore.ErrInvalidPage) {
								t.Fatalf("invalid page error=%v", err)
							}
							return nil
						}
						if err != nil {
							return err
						}
						if len(rows) != len(test.want) || more != test.more {
							t.Fatalf("credential page rows=%d more=%t want=%v more=%t", len(rows), more, test.want, test.more)
						}
						for index, row := range rows {
							if row.OrganizationID != org || row.ID != test.want[index] {
								t.Fatalf("credential page escaped tenant or ordering: %+v", row)
							}
							if strings.HasPrefix(row.ID, "page_") && (row.Name != string(org)+" "+row.ID || row.Fingerprint != "fingerprint_"+string(org) || row.Scope != "admin" || row.Kind != "service" || row.RotatedAt == nil || !row.RotatedAt.Equal(now) || row.RevokedAt == nil || !row.RevokedAt.Equal(now) || row.ExpiresAt == nil || !row.ExpiresAt.Equal(now)) {
								t.Fatalf("credential metadata fields lost: %+v", row)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	})
	t.Run("instance organization pages require current admin", func(t *testing.T) {
		created := pgstoretest.HistoryAt()
		for index, org := range []tracker.OrganizationID{"000_org_a", "000_org_b", "000_org_c"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: fmt.Sprintf("Example organization %d", index), CreatedAt: created})
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for _, test := range []struct {
					id, scope, keyScope, user string
					native                    bool
					expires, revoked          *time.Time
				}{
					{id: "directory_admin", scope: "admin"},
					{id: "directory_operator", scope: "operator"},
					{id: "directory_hosted", scope: "admin", user: "first"},
					{id: "directory_scoped_admin", scope: "admin", keyScope: "admin"},
					{id: "directory_invalid_scope", scope: "admin", keyScope: "invalid"},
					{id: "directory_native", scope: "admin", native: true},
					{id: "directory_expired", scope: "admin", expires: &created},
					{id: "directory_revoked", scope: "admin", revoked: &created},
					{id: "directory_runner", scope: "admin"},
				} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: test.id, Name: "Example directory " + test.id, Hash: test.id + "_" + string(org), Scope: test.scope, KeyScope: test.keyScope, UserID: test.user, NativeOnly: test.native, ExpiresAt: test.expires, RevokedAt: test.revoked, CreatedAt: created, UpdatedAt: created, ProjectAccess: "all", Kind: "service"}); err != nil {
						return err
					}
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "directory_machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				expires := time.Now().UTC().Add(time.Hour)
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "directory_enrollment", RunnerID: "directory_runner", MachineID: "directory_machine", TokenHash: apikey.HashToken("directory_enrollment_" + string(org)), Scope: "organization", Operations: []string{"read"}, CreatedAt: created, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), "directory_enrollment", "directory_runner", "directory_machine", created); err != nil {
					return err
				}
				return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: "directory_runner", MachineID: "directory_machine", TokenID: "directory_runner", EnrollmentID: "directory_enrollment", Scope: "organization", Operations: []string{"read"}, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: created, ExpiresAt: expires, LastHeartbeatAt: created})
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			other := tracker.OrganizationID("org_first")
			if org == "org_first" {
				other = "org_second"
			}
			for _, test := range []struct {
				name, credential, hash string
				offset, limit          int
				ids                    []tracker.OrganizationID
				more, denied           bool
			}{
				{name: "first page", credential: "directory_admin", limit: 2, ids: []tracker.OrganizationID{"000_org_a", "000_org_b"}, more: true},
				{name: "continuation", credential: "directory_admin", offset: 2, limit: 1, ids: []tracker.OrganizationID{"000_org_c"}, more: true},
				{name: "exact final page", credential: "directory_admin", offset: 3, limit: 2, ids: []tracker.OrganizationID{"org_first", "org_second"}},
				{name: "past end", credential: "directory_admin", offset: 100000, limit: 200},
				{name: "ordinary operator", credential: "directory_operator", limit: 1, denied: true},
				{name: "hosted user admin", credential: "directory_hosted", limit: 1, denied: true},
				{name: "instance admin with valid key scope", credential: "directory_scoped_admin", limit: 1, ids: []tracker.OrganizationID{"000_org_a"}, more: true},
				{name: "invalid key scope", credential: "directory_invalid_scope", limit: 1, denied: true},
				{name: "native only admin", credential: "directory_native", limit: 1, denied: true},
				{name: "expired admin", credential: "directory_expired", limit: 1, denied: true},
				{name: "revoked admin", credential: "directory_revoked", limit: 1, denied: true},
				{name: "runner admin", credential: "directory_runner", limit: 1, denied: true},
				{name: "wrong credential hash", credential: "directory_admin", hash: "wrong", limit: 1, denied: true},
				{name: "other tenant real credential hash", credential: "directory_admin", hash: "directory_admin_" + string(other), limit: 1, denied: true},
				{name: "missing credential", credential: "directory_missing", limit: 1, denied: true},
			} {
				t.Run(string(org)+"/"+test.name, func(t *testing.T) {
					err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(accessstore.InstanceOrganizationPageReader)
						if !ok {
							t.Fatal("instance organization page owner unavailable")
						}
						hash := test.hash
						if hash == "" {
							hash = test.credential + "_" + string(org)
						}
						rows, more, err := reader.AccessInstanceOrganizationPage(t.Context(), test.credential, hash, test.offset, test.limit)
						if test.denied {
							if len(rows) != 0 || more {
								t.Fatalf("unauthorized directory response rows=%d more=%t error=%v", len(rows), more, err)
							}
							return err
						}
						if err != nil {
							return err
						}
						if len(rows) != len(test.ids) || more != test.more {
							t.Fatalf("organization page rows=%d more=%t want=%v more=%t", len(rows), more, test.ids, test.more)
						}
						for index, row := range rows {
							if row.OrganizationID != test.ids[index] || row.Name == "" || row.Local {
								t.Fatalf("organization metadata/order lost: %+v", row)
							}
						}
						return nil
					})
					if test.denied {
						if !errors.Is(err, accessstore.ErrDenied) {
							t.Fatalf("directory authorization error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
		var id string
		err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT organization_id FROM public.operator_organization_metadata_page('directory_admin','directory_admin_org_first',0,1)`).Scan(&id)
		var state interface{ SQLState() string }
		if !errors.As(err, &state) || state.SQLState() != "28000" {
			t.Fatalf("unbound organization page error=%v", err)
		}
		var allowed bool
		if err := admin.QueryRowContext(t.Context(), `SELECT p.proowner=r.oid AND p.prosecdef AND p.provolatile='s'
 AND p.proconfig=ARRAY['search_path=pg_catalog, pg_temp']
 AND NOT (r.rolcanlogin OR r.rolinherit OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members m WHERE r.oid IN (m.roleid,m.member))
 AND has_function_privilege('detent_runtime_v2'::name,p.oid,'EXECUTE')
 AND NOT EXISTS(SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0 AND a.privilege_type='EXECUTE')
 FROM pg_roles r CROSS JOIN pg_proc p
 WHERE r.rolname='detent_local_bootstrap_reader_v2' AND p.oid='public.operator_organization_metadata_page(text,text,integer,integer)'::regprocedure`).Scan(&allowed); err != nil || !allowed {
			t.Fatalf("isolated organization directory owner/function = %t, %v", allowed, err)
		}
		for _, role := range []string{"detent_platform_v2", "detent_migration_v2", "detent_search_reader_v2"} {
			if err := admin.QueryRowContext(t.Context(), `SELECT has_function_privilege($1::name,'public.operator_organization_metadata_page(text,text,integer,integer)','EXECUTE')`, role).Scan(&allowed); err != nil || allowed {
				t.Fatalf("%s organization directory execute = %t, %v", role, allowed, err)
			}
		}
	})
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
	platform := pgstoretest.RolePool(t, admin, "detent_platform_v2", "")
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
	t.Run("native credential grant transaction", func(t *testing.T) {
		now := pgstoretest.HistoryAt()
		grantedAt := now.Add(time.Hour)
		organizations := []tracker.OrganizationID{"org_first", "org_second"}
		for _, organization := range organizations {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_native", Name: "Native grant project", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
					return err
				}
				for _, id := range []string{"native_target", "native_other"} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: id, Hash: "hash_" + id, Fingerprint: "fingerprint_" + id, Scope: "operator", CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, test := range []struct {
			name                      string
			organization              tracker.OrganizationID
			credential                string
			grantOrganization         tracker.OrganizationID
			project                   tracker.ProjectID
			wantError                 error
			wantSQLState              string
			firstNative, secondNative bool
		}{
			{name: "first tenant", organization: "org_first", credential: "native_target", grantOrganization: "org_first", project: "prj_native", firstNative: true},
			{name: "replayed grant", organization: "org_first", credential: "native_target", grantOrganization: "org_first", project: "prj_native", firstNative: true},
			{name: "missing project rolls back", organization: "org_first", credential: "native_other", grantOrganization: "org_first", project: "prj_missing", wantSQLState: "23503", firstNative: true},
			{name: "cross organization rolls back", organization: "org_first", credential: "native_other", grantOrganization: "org_second", project: "prj_native", wantError: corestore.ErrTenantMismatch, firstNative: true},
			{name: "second tenant", organization: "org_second", credential: "native_target", grantOrganization: "org_second", project: "prj_native", firstNative: true, secondNative: true},
			{name: "missing credential", organization: "org_first", credential: "native_missing", grantOrganization: "org_first", project: "prj_native", wantError: corestore.ErrNotFound, firstNative: true, secondNative: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				err := repository.WriteTenant(t.Context(), test.organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.ScopeAccessCredentialToNative(t.Context(), test.credential, grantedAt); err != nil {
						return err
					}
					return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: test.grantOrganization, TokenID: test.credential, ProjectID: test.project})
				})
				if test.wantSQLState != "" {
					var state interface{ SQLState() string }
					if !errors.As(err, &state) || state.SQLState() != test.wantSQLState {
						t.Fatalf("grant transaction error=%v, want SQLSTATE %s", err, test.wantSQLState)
					}
				} else if !errors.Is(err, test.wantError) {
					t.Fatalf("grant transaction error=%v, want %v", err, test.wantError)
				}
				for index, organization := range organizations {
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						for _, id := range []string{"native_target", "native_other"} {
							row, err := r.AccessCredential(t.Context(), id)
							if err != nil {
								return err
							}
							wantNative := id == "native_target" && (index == 0 && test.firstNative || index == 1 && test.secondNative)
							wantUpdated := now
							if wantNative {
								wantUpdated = grantedAt
							}
							if row.OrganizationID != organization || row.ID != id || row.NativeOnly != wantNative || !row.UpdatedAt.Equal(wantUpdated) || !row.CreatedAt.Equal(now) || row.Name != id || row.Hash != "hash_"+id || row.Fingerprint != "fingerprint_"+id || row.Scope != "operator" || row.ProjectAccess != "selected" || row.Kind != "service" || row.RevokedAt != nil || row.ExpiresAt != nil {
								t.Fatalf("credential scope or sibling changed: organization=%s id=%s native=%v updated=%s", organization, id, row.NativeOnly, row.UpdatedAt)
							}
							grants, err := r.AccessTokenGrants(t.Context(), id, "", 2)
							if err != nil {
								return err
							}
							wantCount := 0
							if wantNative {
								wantCount = 1
							}
							if len(grants) != wantCount || wantCount == 1 && (grants[0].OrganizationID != organization || grants[0].TokenID != id || grants[0].ProjectID != "prj_native") {
								t.Fatalf("grant scope or replay changed: organization=%s id=%s grants=%+v", organization, id, grants)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	})
	t.Run("designated local organization", func(t *testing.T) {
		if organization, err := repository.DesignatedLocalOrganization(t.Context()); organization != "" || !errors.Is(err, postgres.ErrLocalOrganizationRequired) {
			t.Fatalf("nonlocal organizations did not refuse local startup: %q, %v", organization, err)
		}
		createLocal := func(organization tracker.OrganizationID) error {
			return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Local organization", Local: true, CreatedAt: pgstoretest.HistoryAt()})
			})
		}
		if err := createLocal("org_local"); err != nil {
			t.Fatal(err)
		}
		if organization, err := repository.DesignatedLocalOrganization(t.Context()); err != nil || organization != "org_local" {
			t.Fatalf("designated organization = %q, %v", organization, err)
		}
		if err := pool.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM public.organizations`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("runtime can read unbound organization rows: count=%d error=%v", count, err)
		}
		var allowed bool
		if err := platform.QueryRow(t.Context(), `SELECT has_function_privilege(current_user,'public.lookup_designated_local_organization()','EXECUTE')`).Scan(&allowed); err != nil || allowed {
			t.Fatalf("platform discovery execute = %t, %v", allowed, err)
		}
		if err := admin.QueryRowContext(t.Context(), `SELECT p.proowner=r.oid AND p.prosecdef
			AND NOT (r.rolcanlogin OR r.rolinherit OR r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication)
			AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE r.oid IN (m.roleid,m.member))
			FROM pg_roles r CROSS JOIN pg_proc p
			WHERE r.rolname='detent_local_bootstrap_reader_v2' AND p.oid='public.lookup_designated_local_organization()'::regprocedure`).Scan(&allowed); err != nil || !allowed {
			t.Fatalf("isolated discovery owner = %t, %v", allowed, err)
		}
		migration := pgstoretest.RolePool(t, admin, "detent_migration_v2", "")
		defer migration.Close()
		if err := migration.QueryRow(pgtest.UnboundProbe(t.Context()), `SELECT count(*) FROM public.organizations`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("migration can read unbound organization rows: count=%d error=%v", count, err)
		}
		if err := migration.QueryRow(t.Context(), `SELECT has_function_privilege(current_user,'public.lookup_designated_local_organization()','EXECUTE')`).Scan(&allowed); err != nil || allowed {
			t.Fatalf("migration discovery execute = %t, %v", allowed, err)
		}
		t.Run("caller setting retains tenant isolation", func(t *testing.T) {
			ctx := t.Context()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			deadline, ok := t.Deadline()
			if !ok {
				deadline = time.Now().Add(5 * time.Second)
			}
			defer func() {
				cleanup, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
				defer cancel()
				if err := tx.Rollback(cleanup); err != nil {
					t.Error(err)
				}
			}()
			if _, err := tx.Exec(ctx, `SET LOCAL detent.designated_local_lookup = 'on'`); err != nil {
				t.Fatal(err)
			}
			var unbound int
			if err := tx.QueryRow(pgtest.UnboundProbe(ctx), `SELECT count(*) FROM public.organizations`).Scan(&unbound); err != nil || unbound != 0 {
				t.Fatalf("runtime setting exposed unbound organization rows: count=%d error=%v", unbound, err)
			}
			for _, organization := range []string{"org_first", "org_second"} {
				if _, err := tx.Exec(ctx, `SELECT set_config('detent.organization_id', $1, true)`, organization); err != nil {
					t.Fatal(err)
				}
				var designated string
				if err := tx.QueryRow(ctx, `SELECT public.lookup_designated_local_organization()`).Scan(&designated); err != nil || designated != "org_local" {
					t.Fatalf("designated organization with caller setting = %q, %v", designated, err)
				}
				var visible []string
				if err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT organization_id FROM public.organizations)`).Scan(&visible); err != nil || len(visible) != 1 || visible[0] != organization {
					t.Fatalf("runtime setting for %q exposed organizations %v: %v", organization, visible, err)
				}
			}
		})
		if err := createLocal("org_other_local"); !errors.Is(err, corestore.ErrUniqueViolation) {
			t.Fatalf("second local organization = %v, want unique violation", err)
		}
		if _, err := admin.ExecContext(t.Context(), `DROP INDEX public.organizations_single_local`); err != nil {
			t.Fatal(err)
		}
		if err := createLocal("org_other_local"); err != nil {
			t.Fatal(err)
		}
		if organization, err := repository.DesignatedLocalOrganization(t.Context()); organization != "" || err == nil || errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("ambiguous designation = %q, %v", organization, err)
		}
	})
}

func TestAccessCurrentAuthority(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	pgstoretest.SeedTenantAccess(t, repository, "org_first")
	now := pgstoretest.HistoryAt()
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedLeaseFamily(t, repository, org)
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "first", pgstoretest.HistoryAt())
	}); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.AuthorizeAccess(t.Context(), accessstore.Authority{CredentialID: "first", Hash: "hash_first"}, pgstoretest.HistoryAt()); err != nil {
				return err
			}
			_, err := w.Claim(t.Context(), pgstoretest.LeaseClaim(1))
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

func TestAccessImportedEnvelopesAndAtomicAudit(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	keys := pgstoretest.AccessTestKeys(t, true)
	secrets := map[tracker.OrganizationID]accessstore.Secret{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedRepository(t, repository, org)
		envelope, err := keys.Seal([]byte("example credential"), accessstore.SecretAAD(org, "prj_first", "example"))
		if err != nil {
			t.Fatal(err)
		}
		secret := accessstore.Secret{OrganizationID: org, ProjectID: "prj_first", Kind: "example", OrganizationSlug: "example", Envelope: envelope, UpdatedAt: pgstoretest.HistoryAt()}
		secrets[org] = secret
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), secret, accessstore.SecretAudit{OrganizationID: org, ProjectID: secret.ProjectID, Kind: secret.Kind, Actor: "first", Event: "import", KeyVersion: envelope.Version, RecordedAt: pgstoretest.HistoryAt()})
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
			if err := accessstore.VerifySecret(pgstoretest.AccessTestKeys(t, false), got); !errors.Is(err, hubsecrets.ErrUnavailable) {
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
		if err := w.StoreAccessSecret(t.Context(), secret, accessstore.SecretAudit{OrganizationID: secret.OrganizationID, ProjectID: secret.ProjectID, Kind: secret.Kind, Actor: "first", Event: "set", KeyVersion: secret.Envelope.Version, RecordedAt: pgstoretest.HistoryAt()}); err != nil {
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedRepository(t, repository, org)
		now := pgstoretest.HistoryAt()
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.RevokeAccessSession(t.Context(), "binding", now); err != nil {
				return err
			}
			inserted, err := w.EnsureAccessSession(t.Context(), accessstore.Session{OrganizationID: org, Hash: "binding", Subject: "first", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
			if err != nil {
				return err
			}
			if inserted {
				t.Fatal("replayed assertion reported a new binding")
			}
			row, err := w.AccessSession(t.Context(), "binding")
			if err != nil {
				return err
			}
			if row.RevokedAt == nil {
				t.Fatal("replayed assertion resurrected revoked binding")
			}
			active := accessstore.Session{OrganizationID: org, Hash: "active", Subject: "first", SupportActor: "support@example.test", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			if inserted, err := w.EnsureAccessSession(t.Context(), active); err != nil || !inserted {
				t.Fatalf("new binding inserted=%t error=%v", inserted, err)
			}
			for i, want := range []bool{true, false} {
				ended, ok, err := w.EndAccessSession(t.Context(), "active", now)
				if err != nil {
					return err
				}
				if ok != want || ok && (ended.SupportActor != active.SupportActor || ended.Subject != active.Subject) {
					t.Fatalf("end binding attempt %d = %+v %t, want ended=%t", i, ended, ok, want)
				}
			}
			if _, ok, err := w.EndAccessSession(t.Context(), "missing", now); err != nil || ok {
				t.Fatalf("end missing binding ok=%t error=%v", ok, err)
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
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessLoginTransaction(t.Context(), accessstore.LoginTransaction{OrganizationID: org, Hash: "support", State: "support-state", SupportActor: "support@example.com", SupportSession: "session_support", ExpiresAt: now.Add(time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
		for index, consume := range []struct {
			hash, state string
			want        bool
		}{
			{"login", "wrong", false},
			{"login", "", false},
			{"login", "state", true},
			{"login", "state", false},
			{"support", "", true},
			{"support", "", false},
		} {
			var consumed accessstore.LoginTransaction
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				consumed, err = w.ConsumeAccessTransaction(t.Context(), consume.hash, consume.state, now)
				return err
			})
			if (err == nil) != consume.want {
				t.Fatalf("consume index=%d error=%v", index, err)
			}
			if consume.want && consume.hash == "support" && (consumed.SupportActor != "support@example.com" || consumed.SupportSession != "session_support") {
				t.Fatalf("support transaction = %+v", consumed)
			}
		}
	}
}

func TestAccessAuditReferencesAndRewrap(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	objects := pgstoretest.NewHistoryObjects()
	keys := pgstoretest.AccessTestKeys(t, true)
	active := pgstoretest.AccessTestKeys(t, true, "2")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		ref, err := objects.StoreDetail(t.Context(), string(org), []byte(`{"actor":"example"}`))
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := keys.Seal([]byte("example"), accessstore.SecretAAD(org, "", "example"))
		if err != nil {
			t.Fatal(err)
		}
		secret := accessstore.Secret{OrganizationID: org, Kind: "example", Envelope: envelope, UpdatedAt: pgstoretest.HistoryAt()}
		audit := accessstore.SecretAudit{OrganizationID: org, Kind: "example", Actor: "first", Event: "set", KeyVersion: envelope.Version, RecordedAt: pgstoretest.HistoryAt()}
		var ids []int64
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.StoreAccessSecret(t.Context(), secret, audit); err != nil {
				return err
			}
			for _, key := range []string{"first", "second"} {
				id, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, SessionID: "session", ActualActor: "first", EffectiveUser: "first", Event: "key_call", StartedAt: pgstoretest.HistoryAt(), ExpiresAt: pgstoretest.HistoryAt().Add(time.Hour), RecordedAt: pgstoretest.HistoryAt(), RecordedAtNS: pgstoretest.HistoryAt().UnixNano(), Mutation: &ref, KeyID: key, KeyKind: "personal", KeyOwner: "first"})
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
	runAccessAuditRetentionCases(t, repository, pool, admin, objects)
}

func TestAccessLookupFailureIsUnavailable(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	authority := accessstore.Authority{CredentialID: "first", Hash: "hash_first", UserID: "first", MembershipID: "membership_first", SessionHash: "session", ProjectID: "prj_first", Write: true}
	for _, org := range organizations {
		pgstoretest.SeedTenantAccess(t, repository, org)
		pgstoretest.SeedRepository(t, repository, org)
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
				steps := map[string]string{"api_tokens": "credential", "hosted_members": "member", "hosted_sessions": "session", "hosted_project_grants": "project_grant", "token_grants": "token_grant"}
				var lookup *accessstore.LookupError
				if !errors.As(err, &lookup) || lookup.Step != steps[table] {
					t.Errorf("tenant %s missing lookup step %s: %v", org, steps[table], err)
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

type accessAuditQueryTrace struct {
	mu        sync.Mutex
	statement string
	args      []any
	armed     atomic.Bool
	started   chan uint32
}

func (p *accessAuditQueryTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT public.prune_") && strings.Contains(data.SQL, "access_audits(") {
		p.mu.Lock()
		p.statement, p.args = data.SQL, append([]any(nil), data.Args...)
		p.mu.Unlock()
		if p.armed.CompareAndSwap(true, false) {
			p.started <- conn.PgConn().PID()
		}
	}
	return ctx
}
func (*accessAuditQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func runAccessAuditRetentionCases(t *testing.T, repository *postgres.Repository, pool *pgxpool.Pool, admin *sql.DB, objects *pgstoretest.HistoryObjects) {
	t.Helper()
	admin.SetMaxOpenConns(2)
	var database string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 8"); err != nil {
		t.Fatal(err)
	}
	trace := &accessAuditQueryTrace{started: make(chan uint32, 1)}
	pruner, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, trace), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pruner.Close(); err != nil {
			t.Error(err)
		}
	})
	now := pgstoretest.HistoryAt()
	cutoff := now.Add(-30 * 24 * time.Hour)
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for index, org := range organizations {
		t.Run(string(org)+"/paired retention rollback and reference release", func(t *testing.T) {
			ids := make(map[tracker.OrganizationID]int64)
			keys := make(map[tracker.OrganizationID]string)
			for _, tenant := range organizations {
				ref, err := objects.StoreDetail(t.Context(), string(tenant), []byte("retained audit mutation "+string(tenant)))
				if err != nil {
					t.Fatal(err)
				}
				keys[tenant] = ref.Key
				if err := repository.WriteTenant(t.Context(), tenant, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					ids[tenant], err = w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: tenant, ID: int64(90001 + index), SessionID: "retention", ActualActor: "actor", EffectiveUser: "actor", Event: "mutation", Status: 200, StartedAt: now, ExpiresAt: now, RecordedAt: cutoff.Add(-time.Nanosecond), RecordedAtNS: cutoff.UnixNano() - 1, Mutation: &ref})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if index == 0 {
				runAccessAuditPruneIsolation(t, pool, admin, organizations, ids)
			}
			function := pgx.Identifier{"audit_prune_failure_" + string(org)}.Sanitize()
			trigger := pgx.Identifier{"audit_prune_failure_" + string(org)}.Sanitize()
			if _, err := admin.ExecContext(t.Context(), "CREATE FUNCTION "+function+"() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION 'audit prune failed'; END; $f$; CREATE TRIGGER "+trigger+" BEFORE DELETE ON hosted_audit FOR EACH ROW WHEN (OLD.organization_id='"+strings.ReplaceAll(string(org), "'", "''")+"' AND OLD.id="+fmt.Sprint(ids[org])+") EXECUTE FUNCTION "+function+"()"); err != nil {
				t.Fatal(err)
			}
			drop := func(ctx context.Context) error {
				_, err := admin.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger+" ON hosted_audit; DROP FUNCTION IF EXISTS "+function+"()")
				return err
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if err := drop(ctx); err != nil {
					t.Error(err)
				}
			})
			err := pruner.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				_, err := w.PruneAccessAuditsBefore(t.Context(), cutoff, 2)
				return err
			})
			if err == nil {
				t.Fatal("parent delete failure did not abort audit retention")
			}
			pgstoretest.RequireSQLState(t, err, "P0001")
			for _, tenant := range organizations {
				if err := repository.ReadTenant(t.Context(), tenant, corestore.Serving, func(r tenantstore.Reader) error { _, err := r.AccessAudit(t.Context(), ids[tenant]); return err }); err != nil {
					t.Fatalf("rollback lost %s audit: %v", tenant, err)
				}
				var paired int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times WHERE organization_id=$1 AND audit_id=$2`, tenant, ids[tenant]).Scan(&paired); err != nil || paired != 1 {
					t.Fatalf("rollback lost %s times row: %d %v", tenant, paired, err)
				}
			}
			if err := drop(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := pruner.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				count, err := w.PruneAccessAuditsBefore(t.Context(), cutoff, 2)
				if err == nil && count != 2 {
					return fmt.Errorf("pruned=%d want2", count)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, tenant := range organizations {
				if err := repository.ReadTenant(t.Context(), tenant, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.AccessAudit(t.Context(), ids[tenant])
					if !errors.Is(err, corestore.ErrNotFound) {
						return fmt.Errorf("pruned audit lookup=%v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var references, paired int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND mutation_object_key=$2`, tenant, keys[tenant]).Scan(&references); err != nil || references != 0 {
					t.Fatalf("pruned %s audit still references payload: %d %v", tenant, references, err)
				}
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times WHERE organization_id=$1 AND audit_id=$2`, tenant, ids[tenant]).Scan(&paired); err != nil || paired != 0 {
					t.Fatalf("pruned %s audit left times row: %d %v", tenant, paired, err)
				}
			}
		})
		t.Run(string(org)+"/legacy read predicate rechecked after lock", func(t *testing.T) {
			var id int64
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				id, err = w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, SessionID: "recheck", ActualActor: "actor", EffectiveUser: "actor", Event: "action", Route: "GET /items", Status: 200, StartedAt: now, ExpiresAt: now, RecordedAt: cutoff.Add(time.Nanosecond), RecordedAtNS: cutoff.UnixNano() + 1})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			blocker, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if err := blocker.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Error(err)
				}
			}()
			if _, err := blocker.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
				t.Fatal(err)
			}
			var blockerPID int
			if err := blocker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.Exec(t.Context(), `UPDATE hosted_audit SET status=403 WHERE organization_id=$1 AND id=$2`, org, id); err != nil {
				t.Fatal(err)
			}
			trace.armed.Store(true)
			task := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return pruner.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
					count, err := w.PruneAccessAuditReads(ctx, 2)
					if err == nil && count != 0 {
						return fmt.Errorf("changed read was pruned: %d", count)
					}
					return err
				})
			})
			var pid uint32
			waiting := testenv.WaitContext(t, t.Context())
			select {
			case pid = <-trace.started:
			case <-waiting.Done():
				t.Fatal("audit prune did not start")
			}
			testenv.WaitUntil(t, t.Context(), func() error {
				var blocked bool
				if err := admin.QueryRowContext(t.Context(), `SELECT $2=ANY(pg_blocking_pids($1))`, pid, blockerPID).Scan(&blocked); err != nil {
					return err
				}
				if !blocked {
					return errors.New("audit prune has not reached the real row lock")
				}
				return nil
			})
			if err := blocker.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := task.Wait(t); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.AccessAudit(t.Context(), id)
				if err == nil && row.Status != 403 {
					return fmt.Errorf("refusal audit changed: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var paired int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times WHERE organization_id=$1 AND audit_id=$2`, org, id).Scan(&paired); err != nil || paired != 1 {
				t.Fatalf("changed security row lost paired time: %d %v", paired, err)
			}
		})
		t.Run(string(org)+"/successful GET mutation survives cleanup", func(t *testing.T) {
			ref, err := objects.StoreDetail(t.Context(), string(org), []byte(`{"action":"update","outcome":"action"}`))
			if err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				id, err = w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, SessionID: "mutation_get", ActualActor: "actor", EffectiveUser: "actor", Event: "action", Route: "GET /items", Status: 200, StartedAt: now, ExpiresAt: now, RecordedAt: now, RecordedAtNS: now.UnixNano(), Mutation: &ref})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := pruner.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				count, err := w.PruneAccessAuditReads(t.Context(), 2)
				if err == nil && count != 0 {
					return fmt.Errorf("successful GET mutation was cleaned: %d", count)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.AccessAudit(t.Context(), id)
				if err != nil {
					return err
				}
				if row.OrganizationID != org || row.Status != http.StatusOK || row.Route != "GET /items" || !reflect.DeepEqual(row.Mutation, &ref) {
					return fmt.Errorf("successful GET mutation evidence changed: %+v", row)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var paired int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times WHERE organization_id=$1 AND audit_id=$2`, org, id).Scan(&paired); err != nil || paired != 1 {
				t.Fatalf("successful GET mutation lost paired time: %d %v", paired, err)
			}
		})
		t.Run(string(org)+"/prune limits", func(t *testing.T) {
			for _, limit := range []int{0, accessstore.MaxAuditPruneBatch + 1} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					if _, err := w.PruneAccessAuditReads(t.Context(), limit); !errors.Is(err, corestore.ErrInvalidPage) {
						return fmt.Errorf("legacy limit%d=%v", limit, err)
					}
					if _, err := w.PruneAccessAuditsBefore(t.Context(), cutoff, limit); !errors.Is(err, corestore.ErrInvalidPage) {
						return fmt.Errorf("age limit%d=%v", limit, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
		t.Run(string(org)+"/cutoff validation and clock safety", func(t *testing.T) {
			for _, cutoff := range []any{nil, int64(0)} {
				var count int
				err := pool.QueryRow(t.Context(), `SELECT public.prune_expired_access_audits($1,$2)`, cutoff, 2).Scan(&count)
				pgstoretest.RequireSQLState(t, err, "22023")
			}
			for _, statement := range []string{`SELECT public.prune_legacy_access_audits(NULL)`, `SELECT public.prune_expired_access_audits(1,NULL)`} {
				var count int
				pgstoretest.RequireSQLState(t, pool.QueryRow(t.Context(), statement).Scan(&count), "22023")
			}
			for _, zone := range []string{"UTC", "America/New_York"} {
				ref, err := objects.StoreDetail(t.Context(), string(org), []byte("clock safety mutation "+zone))
				if err != nil {
					t.Fatal(err)
				}
				if err := func() (result error) {
					tx, err := pool.Begin(t.Context())
					if err != nil {
						return err
					}
					defer func() {
						ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
						defer cancel()
						if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
							result = errors.Join(result, err)
						}
					}()
					if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
						return err
					}
					if _, err := tx.Exec(t.Context(), `SELECT set_config('TimeZone', $1, true)`, zone); err != nil {
						return err
					}
					var floor time.Time
					if err := tx.QueryRow(t.Context(), `SELECT transaction_timestamp()-interval '720 hours'`).Scan(&floor); err != nil {
						return err
					}
					if _, err := tx.Exec(t.Context(), `WITH inserted AS (
 INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,recorded_at_ns,account_kind,account_retry,mutation_object_key,mutation_sha256,mutation_bytes,mutation_content_bytes,mutation_format_version)
 SELECT $1,'clock_safety','actor','actor','',label,'GET /items','',403,$2,$2,
 $2::timestamptz+delta*interval '1 minute',$3::bigint+delta::bigint*60000000000,'','',
 CASE WHEN label='clock_mutation' THEN $4::text END,CASE WHEN label='clock_mutation' THEN $5::text END,
 CASE WHEN label='clock_mutation' THEN $6::bigint END,CASE WHEN label='clock_mutation' THEN $7::bigint END,
 CASE WHEN label='clock_mutation' THEN $8::bigint END
 FROM (VALUES('clock_expired',-30),('clock_security',30),('clock_mutation',30)) input(label,delta)
 RETURNING organization_id,id,recorded_at_ns
) INSERT INTO hosted_audit_times(organization_id,audit_id,recorded_at_ns) SELECT organization_id,id,recorded_at_ns FROM inserted`, org, floor, floor.UnixNano(), ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.SchemaVersion); err != nil {
						return err
					}
					var count int
					if err := tx.QueryRow(t.Context(), `SELECT public.prune_expired_access_audits($1,$2)`, floor.Add(48*time.Hour).UnixNano(), accessstore.MaxAuditPruneBatch).Scan(&count); err != nil {
						return err
					}
					var events string
					var paired int
					if err := tx.QueryRow(t.Context(), `SELECT COALESCE(string_agg(event,',' ORDER BY event),''),
 (SELECT count(*) FROM hosted_audit_times paired JOIN hosted_audit audit ON audit.organization_id=paired.organization_id AND audit.id=paired.audit_id WHERE audit.organization_id=$1 AND audit.session_id='clock_safety')
 FROM hosted_audit WHERE organization_id=$1 AND session_id='clock_safety'`, org).Scan(&events, &paired); err != nil {
						return err
					}
					if count < 1 || count > accessstore.MaxAuditPruneBatch || events != "clock_mutation,clock_security" || paired != 2 {
						return fmt.Errorf("timezone %q unsafe cutoff count=%d survivors=%q paired=%d", zone, count, events, paired)
					}
					return nil
				}(); err != nil {
					t.Fatalf("database720-hour safety floor changed in %s: %v", zone, err)
				}
			}
		})
	}
	checkAccessAuditForeignUpdateDenied(t, admin, organizations, cutoff)
}

func runAccessAuditPruneIsolation(t *testing.T, pool *pgxpool.Pool, admin *sql.DB, organizations []tracker.OrganizationID, ids map[tracker.OrganizationID]int64) {
	t.Helper()
	migration := pgstoretest.RolePool(t, admin, postgres.MigrationRoleName, "")
	defer migration.Close()
	for _, reader := range []*pgxpool.Pool{pool, migration} {
		var role string
		if err := reader.QueryRow(t.Context(), `SELECT current_user`).Scan(&role); err != nil || role == postgres.MigrationRoleName {
			t.Fatalf("direct reader unexpectedly uses maintenance owner %q: %v", role, err)
		}
		for index, org := range organizations {
			foreign := organizations[1-index]
			if err := func() (result error) {
				tx, err := reader.Begin(t.Context())
				if err != nil {
					return err
				}
				defer func() {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
						result = errors.Join(result, err)
					}
				}()
				if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
					return err
				}
				for _, statement := range []string{
					`SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND id=$2`,
					`SELECT count(*) FROM hosted_audit_times WHERE organization_id=$1 AND audit_id=$2`,
				} {
					var count int
					if err := tx.QueryRow(t.Context(), statement, foreign, ids[foreign]).Scan(&count); err != nil {
						return err
					}
					if count != 0 {
						return fmt.Errorf("direct role read exposed %s eligible audit: %d", foreign, count)
					}
				}
				for _, statement := range []string{
					`UPDATE hosted_audit SET status=403 WHERE organization_id=$1 AND id=$2`,
					`DELETE FROM hosted_audit_times WHERE organization_id=$1 AND audit_id=$2`,
					`DELETE FROM hosted_audit WHERE organization_id=$1 AND id=$2`,
				} {
					result, err := tx.Exec(t.Context(), statement, foreign, ids[foreign])
					if err != nil {
						return err
					}
					if result.RowsAffected() != 0 {
						return fmt.Errorf("direct role mutated %s eligible audit: %d", foreign, result.RowsAffected())
					}
				}
				return nil
			}(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func checkAccessAuditForeignUpdateDenied(t *testing.T, admin *sql.DB, organizations []tracker.OrganizationID, cutoff time.Time) {
	t.Helper()
	migration := pgstoretest.RolePool(t, admin, postgres.MigrationRoleName, "")
	defer migration.Close()
	t.Run("maintenance row locks do not permit foreign updates", func(t *testing.T) {
		tx, err := migration.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
			defer cancel()
			if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Error(err)
			}
		}()
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE "+pgx.Identifier{postgres.MigrationRoleName}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(organizations[0])); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), `UPDATE hosted_audit SET status=403 WHERE organization_id=$1 AND recorded_at_ns<$2`, organizations[1], cutoff.UnixNano())
		pgstoretest.RequireSQLState(t, err, "42501")
	})
}

func TestAuthorizeAccessRunnerProjectScope(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	now := pgstoretest.HistoryAt()
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	organizationRunners := []string{"runner_org", "runner_expiring", "runner_removed", "runner_reduced", "runner_revoked", "runner_session"}
	hash := func(organization tracker.OrganizationID, credential string) string {
		return apikey.HashToken(string(organization) + "/" + credential)
	}
	own := func(organization tracker.OrganizationID) tracker.ProjectID {
		return tracker.ProjectID("project_only_" + string(organization))
	}
	for _, organization := range organizations {
		if _, err := admin.ExecContext(ctx, `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, organization, now); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, project := range []tracker.ProjectID{"project_one", "project_two", own(organization)} {
				if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
					return err
				}
			}
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_shared", Hostname: "example", DisplayName: "Example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			for _, credential := range append([]string{"runner_selected", "service_plain"}, organizationRunners...) {
				row := accessstore.Credential{OrganizationID: organization, ID: credential, Name: credential, Hash: hash(organization, credential), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: pgstoretest.TimePointer(now.Add(24 * time.Hour)), ProjectAccess: "selected", Kind: "service"}
				if err := w.CreateAccessCredential(ctx, row); err != nil {
					return err
				}
				if credential == "service_plain" {
					continue
				}
				enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + credential, RunnerID: credential, MachineID: "machine_shared", TokenHash: apikey.HashToken("enrollment/" + string(organization) + "/" + credential), Scope: "organization", SharedMachine: true, Operations: []string{"read", "claim"}, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), CreatedBy: "admin"}
				expires := now.Add(24 * time.Hour)
				if credential == "runner_expiring" {
					expires = now.Add(time.Hour)
				}
				if credential == "runner_selected" {
					enrollment.Scope, enrollment.ProjectIDs = "projects", []tracker.ProjectID{"project_one"}
					if err := w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: credential, ProjectID: "project_one"}); err != nil {
						return err
					}
				}
				if err := w.CreateRunnerEnrollment(ctx, enrollment); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(ctx, enrollment.ID, credential, "machine_shared", now); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(ctx, runnerstore.Identity{OrganizationID: organization, RunnerID: credential, MachineID: "machine_shared", TokenID: credential, EnrollmentID: enrollment.ID, Scope: enrollment.Scope, Operations: enrollment.Operations, DisplayName: credential, CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "amd64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	authority := func(organization tracker.OrganizationID, credential string, project tracker.ProjectID) accessstore.Authority {
		return accessstore.Authority{CredentialID: credential, Hash: hash(organization, credential), ProjectID: project, Write: true}
	}
	authorize := func(organization tracker.OrganizationID, credential string, project tracker.ProjectID, at time.Time) error {
		return repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.AuthorizeAccess(ctx, authority(organization, credential, project), at)
			return err
		})
	}
	requireDecision := func(t *testing.T, err error, allowed bool) {
		t.Helper()
		if allowed && err != nil {
			t.Fatalf("authorization error=%v, want allowed", err)
		}
		if !allowed && (!errors.Is(err, accessstore.ErrDenied) || errors.Is(err, tenantauth.ErrUnavailable)) {
			t.Fatalf("authorization error=%v, want denied", err)
		}
	}
	for _, test := range []struct {
		name       string
		credential string
		project    func(organization, other tracker.OrganizationID) tracker.ProjectID
		after      time.Duration
		allowed    bool
	}{
		{name: "organization runner first project", credential: "runner_org", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }, allowed: true},
		{name: "organization runner second project", credential: "runner_org", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_two" }, allowed: true},
		{name: "organization runner own project", credential: "runner_org", project: func(organization, _ tracker.OrganizationID) tracker.ProjectID { return own(organization) }, allowed: true},
		{name: "organization runner foreign project", credential: "runner_org", project: func(_, other tracker.OrganizationID) tracker.ProjectID { return own(other) }},
		{name: "organization runner missing project", credential: "runner_org", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_absent" }},
		{name: "selected runner granted project", credential: "runner_selected", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }, allowed: true},
		{name: "selected runner other project", credential: "runner_selected", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_two" }},
		{name: "selected runner foreign project", credential: "runner_selected", project: func(_, other tracker.OrganizationID) tracker.ProjectID { return own(other) }},
		{name: "service credential without runner identity", credential: "service_plain", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }},
		{name: "unknown credential", credential: "runner_absent", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }},
		{name: "live identity before expiry", credential: "runner_expiring", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }, after: 30 * time.Minute, allowed: true},
		{name: "expired identity", credential: "runner_expiring", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }, after: 2 * time.Hour},
		{name: "expired credential", credential: "runner_org", project: func(_, _ tracker.OrganizationID) tracker.ProjectID { return "project_one" }, after: 25 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			for index, organization := range organizations {
				requireDecision(t, authorize(organization, test.credential, test.project(organization, organizations[1-index]), now.Add(test.after)), test.allowed)
			}
		})
	}
	for _, test := range []struct {
		name   string
		runner string
		change func(*testing.T)
	}{
		{name: "removed identity", runner: "runner_removed", change: func(t *testing.T) {
			if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RemoveRunnerIdentity(ctx, "runner_removed", now) }); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "scope reduced", runner: "runner_reduced", change: func(t *testing.T) {
			if _, err := admin.ExecContext(ctx, `UPDATE runner_identities SET scope='projects' WHERE organization_id='org_first' AND runner_id='runner_reduced'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "revoked credential", runner: "runner_revoked", change: func(t *testing.T) {
			if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(ctx, "runner_revoked", now) }); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, organization := range organizations {
				requireDecision(t, authorize(organization, test.runner, "project_two", now), true)
			}
			test.change(t)
			requireDecision(t, authorize("org_first", test.runner, "project_two", now), false)
			requireDecision(t, authorize("org_second", test.runner, "project_two", now), true)
		})
	}
	t.Run("removal inside one transaction", func(t *testing.T) {
		var before, after error
		if err := repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			for range 2 {
				if _, before = w.AuthorizeAccess(ctx, authority("org_first", "runner_session", "project_two"), now); before != nil {
					return nil
				}
			}
			if err := w.RemoveRunnerIdentity(ctx, "runner_session", now); err != nil {
				return err
			}
			_, after = w.AuthorizeAccess(ctx, authority("org_first", "runner_session", "project_two"), now)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		requireDecision(t, before, true)
		requireDecision(t, after, false)
		requireDecision(t, authorize("org_first", "runner_session", "project_two", now), false)
		requireDecision(t, authorize("org_second", "runner_session", "project_two", now), true)
	})
	t.Run("identity lookup failure", func(t *testing.T) {
		if _, err := admin.ExecContext(ctx, "REVOKE SELECT ON runner_identities FROM detent_runtime_v2"); err != nil {
			t.Fatal(err)
		}
		for _, organization := range organizations {
			if err := authorize(organization, "runner_org", "project_two", now); !errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, accessstore.ErrDenied) {
				t.Errorf("tenant %s lookup error=%v, want unavailable without denial", organization, err)
			}
		}
		if _, err := admin.ExecContext(ctx, "GRANT SELECT ON runner_identities TO detent_runtime_v2"); err != nil {
			t.Fatal(err)
		}
		for _, organization := range organizations {
			requireDecision(t, authorize(organization, "runner_org", "project_two", now), true)
		}
	})
}
