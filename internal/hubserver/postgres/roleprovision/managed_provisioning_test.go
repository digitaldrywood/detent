package roleprovision_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

const (
	previousReleaseSchemaVersion = int64(20261107232145)
	managedAdministrator         = "example_cluster_admin"
	temporaryAdministrator       = "example_temporary_admin"
	laterTemporaryAdministrator  = "example_later_temporary_admin"
	unprivilegedLogin            = "example_unprivileged_login"
	unrelatedAdministrator       = "example_unrelated_admin"
	loginPassword                = "example-password"
	bootstrapSuperuser           = "detent_test"
)

func previousReleaseDatabase(ctx context.Context, db *sql.DB) error {
	previousRoles, err := os.ReadFile("testdata/roles_v0.185.10.sql")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, `
		CREATE ROLE `+managedAdministrator+` LOGIN NOSUPERUSER INHERIT CREATEROLE CREATEDB REPLICATION BYPASSRLS PASSWORD '`+loginPassword+`';
		SET ROLE `+managedAdministrator+`;
		CREATE ROLE detent_migration_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
		CREATE ROLE detent_runtime_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
		CREATE ROLE detent_platform_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
		CREATE ROLE detent_search_reader_v2 NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
		GRANT detent_migration_v2, detent_runtime_v2, detent_platform_v2, detent_search_reader_v2 TO `+managedAdministrator+` WITH INHERIT TRUE, SET TRUE;
		RESET ROLE`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(previousRoles)); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		ALTER FUNCTION public.search_scope_key(text,text,text,text,text,text,text) OWNER TO `+managedAdministrator+`;
		ALTER FUNCTION public.search_scope_keys(text,text,text,text,text,boolean,text) OWNER TO `+managedAdministrator+`;
		GRANT USAGE, CREATE ON SCHEMA public TO `+managedAdministrator+`;
		SET ROLE `+postgres.MigrationRoleName); err != nil {
		return err
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		return err
	}
	_, err = provider.UpTo(ctx, previousReleaseSchemaVersion)
	return err
}

type provisioningCase struct {
	name             string
	provisioner      string
	setRole          string
	reapply          []string
	dropProvisioner  bool
	additionalGrants []string
}

func TestRolesProvisionAsManagedAdministrator(t *testing.T) {
	fixture := pgtest.NewTemplate(previousReleaseDatabase)
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	control := fixture.Database(t)
	cases := []provisioningCase{
		{name: "bootstrap superuser", reapply: []string{""}},
		{
			name:             "managed administrator login",
			provisioner:      managedAdministrator,
			reapply:          []string{managedAdministrator, laterTemporaryAdministrator},
			additionalGrants: []string{"membership detent_local_bootstrap_reader_v2 member=" + managedAdministrator + " grantor=" + bootstrapSuperuser + " admin=t inherit=f set=f"},
		},
		{name: "temporary login inheriting the managed administrator", provisioner: temporaryAdministrator, reapply: []string{temporaryAdministrator}, dropProvisioner: true},
		{
			name:             "temporary login acting as the managed administrator",
			provisioner:      temporaryAdministrator,
			setRole:          managedAdministrator,
			reapply:          []string{laterTemporaryAdministrator},
			dropProvisioner:  true,
			additionalGrants: []string{"membership detent_local_bootstrap_reader_v2 member=" + managedAdministrator + " grantor=" + bootstrapSuperuser + " admin=t inherit=f set=f"},
		},
	}
	var reference []string
	for _, tc := range cases {
		var profile []string
		passed := t.Run(tc.name, func(t *testing.T) { profile = provisionUpgrade(t, fixture, tc) })
		resetClusterRoles(t, control)
		if !passed {
			if reference == nil {
				t.FailNow()
			}
			continue
		}
		if reference == nil {
			reference = profile
			continue
		}
		want := slices.Sorted(slices.Values(append(slices.Clone(reference), tc.additionalGrants...)))
		if diff := profileDifference(want, profile); diff != "" {
			t.Errorf("%s provisioning differs from the superuser reference:\n%s", tc.name, diff)
		}
	}
}

func provisionUpgrade(t *testing.T, fixture *pgtest.Template, tc provisioningCase) []string {
	db := fixture.Database(t)
	var database string
	if err := db.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := db.ExecContext(t.Context(), `ALTER DATABASE `+quoted+` OWNER TO `+managedAdministrator+`; ALTER DATABASE `+quoted+` CONNECTION LIMIT 8`); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{temporaryAdministrator, laterTemporaryAdministrator} {
		if _, err := db.ExecContext(t.Context(), `CREATE ROLE `+login+` LOGIN NOSUPERUSER INHERIT CREATEROLE CREATEDB BYPASSRLS PASSWORD '`+loginPassword+`';
			GRANT `+managedAdministrator+` TO `+login+` WITH INHERIT TRUE, SET TRUE`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `CREATE ROLE `+unprivilegedLogin+` LOGIN NOSUPERUSER NOCREATEROLE PASSWORD '`+loginPassword+`';
		CREATE ROLE `+unrelatedAdministrator+` LOGIN NOSUPERUSER CREATEROLE PASSWORD '`+loginPassword+`'`); err != nil {
		t.Fatal(err)
	}
	if version := schemaVersion(t, db); version != previousReleaseSchemaVersion {
		t.Fatalf("pre-upgrade schema version = %d, want %d", version, previousReleaseSchemaVersion)
	}
	applyRoles(t, fixture, db, tc.provisioner, tc.setRole)
	migrate(t, db)
	latest, err := postgres.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version := schemaVersion(t, db); version != latest {
		t.Fatalf("upgraded schema version = %d, want %d", version, latest)
	}
	before := provisioningProfile(t, db)
	for _, login := range tc.reapply {
		applyRoles(t, fixture, db, login, "")
	}
	if diff := profileDifference(before, provisioningProfile(t, db)); diff != "" {
		t.Errorf("reapplying roles.sql changed the provisioned state:\n%s", diff)
	}
	if tc.dropProvisioner {
		if _, err := db.ExecContext(t.Context(), `DROP ROLE `+tc.provisioner); err != nil {
			t.Fatalf("remove temporary provisioning login: %v", err)
		}
	}
	profile := provisioningProfile(t, db)
	reader := pgstoretest.RolePoolFromTemplate(t, fixture, db, postgres.RuntimeRoleName, "")
	writer := pgstoretest.RolePoolFromTemplate(t, fixture, db, postgres.RuntimeRoleName, "")
	if _, err := postgres.NewRuntime(t.Context(), reader, writer); err != nil {
		t.Fatalf("runtime rejected the provisioned roles: %v", err)
	}
	settled := provisioningProfile(t, db)
	for _, forbidden := range []struct{ grant, revoke string }{
		{
			grant:  `GRANT detent_local_bootstrap_reader_v2 TO detent_runtime_v2 WITH INHERIT FALSE, SET FALSE`,
			revoke: `REVOKE detent_local_bootstrap_reader_v2 FROM detent_runtime_v2`,
		},
		{
			grant:  `GRANT detent_runtime_v2 TO detent_local_bootstrap_reader_v2 WITH INHERIT FALSE, SET FALSE`,
			revoke: `REVOKE detent_runtime_v2 FROM detent_local_bootstrap_reader_v2`,
		},
		{
			grant:  `GRANT detent_local_bootstrap_reader_v2 TO ` + laterTemporaryAdministrator + ` WITH ADMIN FALSE, INHERIT FALSE, SET TRUE`,
			revoke: `REVOKE detent_local_bootstrap_reader_v2 FROM ` + laterTemporaryAdministrator,
		},
		{
			grant:  `GRANT detent_local_bootstrap_reader_v2 TO ` + laterTemporaryAdministrator + ` WITH ADMIN TRUE, INHERIT TRUE, SET FALSE`,
			revoke: `REVOKE detent_local_bootstrap_reader_v2 FROM ` + laterTemporaryAdministrator,
		},
		{
			grant:  `GRANT detent_local_bootstrap_reader_v2 TO ` + unprivilegedLogin + ` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE`,
			revoke: `REVOKE detent_local_bootstrap_reader_v2 FROM ` + unprivilegedLogin,
		},
		{
			grant:  `GRANT detent_local_bootstrap_reader_v2 TO ` + unrelatedAdministrator + ` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE`,
			revoke: `REVOKE detent_local_bootstrap_reader_v2 FROM ` + unrelatedAdministrator,
		},
		{
			grant: `GRANT detent_local_bootstrap_reader_v2 TO ` + laterTemporaryAdministrator + ` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE;
				GRANT detent_local_bootstrap_reader_v2 TO ` + managedAdministrator + ` WITH ADMIN TRUE, INHERIT FALSE, SET FALSE GRANTED BY ` + laterTemporaryAdministrator,
			revoke: `REVOKE detent_local_bootstrap_reader_v2 FROM ` + managedAdministrator + ` GRANTED BY ` + laterTemporaryAdministrator + `;
				REVOKE detent_local_bootstrap_reader_v2 FROM ` + laterTemporaryAdministrator,
		},
	} {
		if _, err := db.ExecContext(t.Context(), forbidden.grant); err != nil {
			t.Fatalf("%s: %v", forbidden.grant, err)
		}
		if _, err := db.ExecContext(t.Context(), postgres.RolesSQL()); err == nil || !strings.Contains(err.Error(), "Detent local bootstrap reader must have no role memberships") {
			t.Errorf("roles.sql after %s: %v, want the reader membership refusal", forbidden.grant, err)
		}
		if _, err := postgres.NewRuntime(t.Context(), reader, writer); !errors.Is(err, postgres.ErrRuntimeRole) {
			t.Errorf("runtime after %s: %v, want %v", forbidden.grant, err, postgres.ErrRuntimeRole)
		}
		if _, err := db.ExecContext(t.Context(), forbidden.revoke); err != nil {
			t.Fatalf("%s: %v", forbidden.revoke, err)
		}
	}
	for _, delegableExecute := range []string{`public.board_search_candidates(text,text[],boolean,text,boolean)`, `public.hosted_write_authorized(bigint)`, `public.hosted_github_webhook_authorized(text,text,text,text,text,boolean)`} {
		if _, err := db.ExecContext(t.Context(), `GRANT EXECUTE ON FUNCTION `+delegableExecute+` TO detent_runtime_v2 WITH GRANT OPTION`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), postgres.RolesSQL()); err == nil || !strings.Contains(err.Error(), "Detent security definer functions must have their provisioned owners and grants") {
			t.Errorf("roles.sql with a delegable runtime grant: %v, want the function grant refusal", err)
		}
		if strings.Contains(delegableExecute, "hosted_") {
			if _, err := postgres.NewRuntime(t.Context(), reader, writer); !errors.Is(err, postgres.ErrRuntimeRole) {
				t.Errorf("runtime accepted delegable %s: %v", delegableExecute, err)
			}
		}
		if _, err := db.ExecContext(t.Context(), `REVOKE GRANT OPTION FOR EXECUTE ON FUNCTION `+delegableExecute+` FROM detent_runtime_v2`); err != nil {
			t.Fatal(err)
		}
	}
	if diff := profileDifference(settled, provisioningProfile(t, db)); diff != "" {
		t.Errorf("forbidden membership checks left state behind:\n%s", diff)
	}
	return profile
}

func applyRoles(t *testing.T, fixture *pgtest.Template, db *sql.DB, login, setRole string) {
	t.Helper()
	target := db
	if login != "" {
		target = fixture.OpenRole(t, db, login, loginPassword)
		defer target.Close()
		var superuser bool
		if err := target.QueryRowContext(t.Context(), `SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&superuser); err != nil || superuser {
			t.Fatalf("provisioning login %s superuser=%t: %v", login, superuser, err)
		}
	}
	script := postgres.RolesSQL()
	if setRole != "" {
		script = "SET ROLE " + setRole + ";\n" + script
	}
	if _, err := target.ExecContext(t.Context(), script); err != nil {
		t.Fatalf("apply roles.sql as %q: %v", login, err)
	}
}

func migrate(t *testing.T, db *sql.DB) {
	t.Helper()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(t.Context(), `SET ROLE `+postgres.MigrationRoleName); err != nil {
		t.Fatal(err)
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatalf("apply migrations after provisioning: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
}

func schemaVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var version int64
	if err := db.QueryRowContext(t.Context(), `SELECT max(version_id) FROM public.goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func resetClusterRoles(t *testing.T, control *sql.DB) {
	t.Helper()
	if _, err := control.ExecContext(t.Context(), `DROP ROLE IF EXISTS detent_local_bootstrap_reader_v2, `+temporaryAdministrator+`, `+laterTemporaryAdministrator+`, `+unprivilegedLogin+`, `+unrelatedAdministrator); err != nil {
		t.Fatalf("reset cluster roles: %v", err)
	}
}

func provisioningProfile(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		SELECT format('role %s super=%s inherit=%s createrole=%s createdb=%s login=%s replication=%s bypassrls=%s',
			rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin, rolreplication, rolbypassrls)
		FROM pg_roles WHERE rolname LIKE 'detent\_%'
		UNION ALL
		SELECT format('membership %s member=%s grantor=%s admin=%s inherit=%s set=%s',
			m.roleid::regrole, m.member::regrole, m.grantor::regrole, m.admin_option, m.inherit_option, m.set_option)
		FROM pg_auth_members m
		WHERE m.roleid::regrole::text LIKE 'detent\_%' OR m.member::regrole::text LIKE 'detent\_%'
		UNION ALL
		SELECT format('database owner=%s acl=%s', pg_get_userbyid(datdba), datacl) FROM pg_database WHERE datname=current_database()
		UNION ALL
		SELECT format('schema %s owner=%s acl=%s', nspname, pg_get_userbyid(nspowner), nspacl)
		FROM pg_namespace WHERE nspname IN ('public','platform')
		UNION ALL
		SELECT format('relation %s.%s kind=%s owner=%s acl=%s rls=%s force=%s',
			n.nspname, c.relname, c.relkind, pg_get_userbyid(c.relowner), c.relacl, c.relrowsecurity, c.relforcerowsecurity)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('public','platform')
		UNION ALL
		SELECT format('column %s.%s acl=%s', a.attrelid::regclass, a.attname, a.attacl)
		FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname IN ('public','platform') AND a.attacl IS NOT NULL
		UNION ALL
		SELECT format('function %s owner=%s acl=%s definer=%s config=%s body=%s',
			p.oid::regprocedure, pg_get_userbyid(p.proowner), p.proacl, p.prosecdef, p.proconfig, md5(p.prosrc))
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('public','platform')
		UNION ALL
		SELECT format('policy %s on %s roles=%s command=%s using=%s check=%s',
			pol.polname, pol.polrelid::regclass, pol.polroles::regrole[], pol.polcmd,
			pg_get_expr(pol.polqual, pol.polrelid), pg_get_expr(pol.polwithcheck, pol.polrelid))
		FROM pg_policy pol
		UNION ALL
		SELECT format('default acl %s %s %s %s', pg_get_userbyid(defaclrole), defaclnamespace::regnamespace, defaclobjtype, defaclacl)
		FROM pg_default_acl
		UNION ALL
		SELECT format('extension %s owner=%s', extname, pg_get_userbyid(extowner)) FROM pg_extension
		UNION ALL
		SELECT format('ledger latest=%s applied=%s', max(version_id), count(*)) FROM public.goose_db_version WHERE is_applied`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var profile []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		profile = append(profile, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(profile)
	return profile
}

func profileDifference(want, got []string) string {
	var diff strings.Builder
	for _, line := range want {
		if !slices.Contains(got, line) {
			diff.WriteString("- " + line + "\n")
		}
	}
	for _, line := range got {
		if !slices.Contains(want, line) {
			diff.WriteString("+ " + line + "\n")
		}
	}
	return diff.String()
}
