package hubdeploy

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestVerifyDatabaseIntegrityRestoredFixtures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, schema string
		tenant       bool
		corrupt      bool
		tables       int
		wantError    bool
	}{
		{"tenant scoped tokenizer", "", true, false, 6, false},
		{"registry", "CREATE TABLE tenants (id TEXT PRIMARY KEY); INSERT INTO tenants VALUES ('example-one'), ('example-two')", false, false, 1, false},
		{"auth", "CREATE TABLE sessions (id TEXT PRIMARY KEY); INSERT INTO sessions VALUES ('example-one'), ('example-two')", false, false, 1, false},
		{"damaged tenant page", "", true, true, 0, true},
		{"orphaned index page", "CREATE TABLE records (id TEXT PRIMARY KEY); INSERT INTO records VALUES ('example-one'), ('example-two'); PRAGMA writable_schema=ON; DELETE FROM sqlite_schema WHERE type='index'", false, false, 0, true},
		{"empty", "", false, false, 0, true},
		{"missing", "", false, false, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var contents []byte
			if test.tenant {
				var err error
				contents, err = os.ReadFile("testdata/tenant.db")
				if err != nil {
					t.Fatal(err)
				}
			} else if test.schema != "" {
				fixture := filepath.Join(t.TempDir(), "fixture.db")
				db, err := sql.Open("sqlite", fixture)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				if _, err := db.ExecContext(t.Context(), test.schema); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				contents, err = os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.corrupt {
				contents[len(contents)/2] = 0
			}
			restored := filepath.Join(t.TempDir(), "restored.db")
			if test.name != "missing" {
				if err := os.WriteFile(restored, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tables, err := hubserver.VerifyDatabaseIntegrity(t.Context(), restored)
			if (err != nil) != test.wantError || tables != test.tables {
				t.Fatalf("integrity check = (%d, %v), want tables=%d error=%v", tables, err, test.tables, test.wantError)
			}
			after, err := os.ReadFile(restored)
			if test.name == "missing" {
				if !os.IsNotExist(err) {
					t.Fatalf("missing database was created: %v", err)
				}
			} else if err != nil || !bytes.Equal(contents, after) {
				t.Fatalf("verification changed restored database: %v", err)
			}
		})
	}
}

func TestRestoreScriptReportsVerificationOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, response, result string
		restoreFail, checkFail bool
		tables                 string
	}{
		{"verified", `{"tables":6}`, "ok", false, false, "6"},
		{"corruption", "", "corrupt", false, true, "0"},
		{"empty schema", `{"tables":0}`, "corrupt", false, false, "0"},
		{"invalid output", "invalid", "corrupt", false, false, "0"},
		{"restore failure", "", "corrupt", true, false, "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			cmd := exec.CommandContext(t.Context(), "bash", "-c", `
source ./restore-test.sh
/usr/local/bin/litestream-detent() {
  [ "$1" = restore ] && [ "$2" = -o ] && [ "$4" = 's3://example/production/tenants/example/hub.db?endpoint=example' ] || exit 2
  [ "$RESTORE_FAIL" = false ] || return 1
  printf fixture > "$3"
}

/opt/detent-hub/detent() {
  [ "$1 $2 $3 $4" = 'hub verify --integrity-only --database' ] && [ "$5" = "$WORK/tenants_example_hub.db" ] || exit 2
  [ "$CHECK_FAIL" = false ] || return 1
  printf '%s\n' "$RESPONSE"
}
stat() { [ -f "$3" ] && printf '7\n'; }
restore_target tenants/example/hub.db "$WORK" s3://example/production endpoint=example
`)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + work, "WORK=" + work, "RESPONSE=" + test.response,
				"RESTORE_FAIL=" + strconv.FormatBool(test.restoreFail), "CHECK_FAIL=" + strconv.FormatBool(test.checkFail)}
			output, err := cmd.CombinedOutput()
			if (err == nil) != (test.result == "ok") {
				t.Fatalf("script error = %v, output = %s", err, output)
			}
			journal := regexp.MustCompile(`(?m)^restore_test target=tenants/example/hub.db result=` + test.result + ` bytes=[0-9]+ tables=` + test.tables + ` seconds=[0-9]+$`)
			if !journal.Match(output) {
				t.Fatalf("missing journal outcome: %s", output)
			}
			entries, err := os.ReadDir(work)
			if err != nil || len(entries) != 0 {
				t.Fatalf("restored data not cleaned up: %v, %v", entries, err)
			}
			if strings.Contains(string(output), "fixture") {
				t.Fatal("journal exposed restored content")
			}
		})
	}
}

func TestReleaseDeploysToLegacyReceiver(t *testing.T) {
	t.Parallel()
	owner, err := os.ReadFile("deploy-from-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, environment string
		invalidIdentity   bool
	}{
		{"production binary", "production", false},
		{"staging binary", "staging", false},
		{"rejected identity", "production", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			for _, dir := range []string{"bin", "opt/releases", "state", "lock", "release-hub"} {
				if err := os.MkdirAll(filepath.Join(work, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, contents string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(work, path), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			const commit = "0123456789012345678901234567890123456789"
			reported := commit
			if test.invalidIdentity {
				reported = strings.Repeat("f", 40)
			}
			write("release-hub/detent", "#!/bin/sh\nprintf '%s\\n' '{\"version\":\"1.2.3\",\"commit\":\""+reported+"\"}'\n"+strings.Repeat("\n", 1000000))
			write("restore-installed", "previous script")
			rewritten := strings.NewReplacer(
				"OPT=/opt/$NAME", "OPT=\"$WORK/opt\"",
				"STATE=/var/lib/$NAME", "STATE=\"$WORK/state\"",
				"/run/lock/$NAME-deploy.lock", "\"$WORK/lock/deploy.lock\"",
				"/tmp/$NAME-", "$WORK/temporary-",
				"/usr/local/sbin/detent-restore-test", "\"$WORK/restore-installed\"",
			).Replace(string(owner))
			write("owner.sh", rewritten)
			write("bin/ssh", `#!/bin/bash
for argument in "$@"; do command=$argument; done
[[ "$command" = "deploy 0123456789012345678901234567890123456789" ]] || exit 2
export SSH_ORIGINAL_COMMAND=$command
exec bash "$WORK/owner.sh"
`)
			write("bin/jq", "#!/bin/sh\ncat >/dev/null\n")
			write("bin/flock", "#!/bin/sh\nexit 0\n")
			write("bin/pgrep", "#!/bin/sh\nexit 1\n")
			write("bin/sleep", "#!/bin/sh\nexit 1\n")
			write("bin/curl", "#!/bin/sh\nprintf '200'\n")
			write("bin/systemctl", "#!/bin/sh\nif [ \"$1\" = show ]; then printf '42\\n'; fi\n")
			write("bin/stat", "#!/bin/sh\nwc -c < \"$3\"\n")
			write("bin/sudo", `#!/bin/bash
shift
if [ "$1" = -u ]; then shift 2; fi
if [ "$1" = install ]; then
  shift
  mode=$2
  shift 6
  exec install -m "$mode" "$@"
fi
if [ "$1" = readlink ]; then exec readlink "$WORK/opt/detent"; fi
exec "$@"
`)
			releaseScript, err := filepath.Abs("../../scripts/deploy-release.sh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", releaseScript, test.environment, commit, "v1.2.3")
			cmd.Dir = "../.."
			cmd.Env = []string{"PATH=" + filepath.Join(work, "bin") + ":" + os.Getenv("PATH"),
				"TMPDIR=" + work, "WORK=" + work, "SSH_KEY=example-key", "KNOWN_HOSTS=example-host"}
			output, err := cmd.CombinedOutput()
			if (err != nil) != test.invalidIdentity {
				t.Fatalf("deploy error = %v, output = %s", err, output)
			}
			installed, err := os.ReadFile(filepath.Join(work, "restore-installed"))
			if err != nil {
				t.Fatal(err)
			}
			want := []byte("previous script")
			if !bytes.Equal(installed, want) {
				t.Fatal("deployment owner installed the wrong restore script")
			}
		})
	}
}

func TestRestoreScriptBoundsAndResumesTenantRotation(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("restore-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name              string
		tenants           int
		runs              int
		fail              bool
		mixedProvisioning bool
	}{
		{"wrap rotation", 23, 3, false, false},
		{"ten thousand tenants", 10000, 1, false, false},
		{"small fleet", 3, 2, false, false},
		{"empty fleet", 0, 1, false, false},
		{"corrupt tenant does not starve rotation", 23, 2, true, false},
		{"mixed provisioning states", 23, 3, false, true},
		{"no provisioned tenants", 0, 1, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			fixture := filepath.Join(work, "registry.db")
			db, err := sql.Open("sqlite", fixture)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if _, err := db.ExecContext(t.Context(), "CREATE TABLE organizations (id TEXT PRIMARY KEY, state TEXT NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			if test.tenants > 0 {
				if _, err := db.ExecContext(t.Context(), `WITH RECURSIVE ids(n) AS (SELECT 0 UNION ALL SELECT n+1 FROM ids WHERE n+1 < ?) INSERT INTO organizations SELECT printf('org_%05d', n), CASE WHEN n % 2 = 0 THEN 'ready' ELSE 'disabled' END FROM ids`, test.tenants); err != nil {
					t.Fatal(err)
				}
			}
			if test.mixedProvisioning {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations VALUES ('org_00000_requested','requested'), ('org_00000_allocating','allocating'), ('org_00000_failed','failed'), ('org_00000_deleting','deleting'), ('org_00000_deleted','deleted')`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for tenant := range test.tenants {
				if err := os.MkdirAll(filepath.Join(work, "tenants", fmt.Sprintf("org_%05d", tenant)), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			source := strings.NewReplacer(
				". /etc/litestream-detent/environment", ":",
				"local work=/var/tmp/detent-restore-test", `local work="$WORK/restore"`,
				"/var/lib/detent-hub/tenants/", "$WORK/tenants/",
			).Replace(string(script))
			writeDeployFixture(t, filepath.Join(work, "restore.sh"), source)
			batchSize := min(test.tenants, 10)
			for run := range test.runs {
				cmd := exec.CommandContext(t.Context(), "bash", "-c", `
source "$WORK/restore.sh"
restore_target() {
  printf '%s\n' "$1"
  if [ "$1" = cloud/registry.db ]; then cp "$WORK/registry.db" "$2/cloud_registry.db"; fi
  case "$1" in tenants/*) [ -d "$WORK/${1%/hub.db}" ] || return 1;; esac
  if [ "$FAIL" = true ] && [ "$1" = tenants/org_00000/hub.db ]; then return 1; fi
}
main
`)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + work, "WORK=" + work, "FAIL=" + strconv.FormatBool(test.fail)}
				output, err := cmd.CombinedOutput()
				wantFailure := test.fail && run == 0
				if (err != nil) != wantFailure {
					t.Fatalf("run %d: error = %v, output = %s", run, err, output)
				}
				want := []string{"cloud/registry.db", "cloud/auth.db"}
				for offset := range batchSize {
					tenant := fmt.Sprintf("org_%05d", (run*batchSize+offset)%test.tenants)
					want = append(want, "tenants/"+tenant+"/hub.db")
				}
				if got := len(strings.Split(strings.TrimSpace(string(output)), "\n")); got != len(want) {
					t.Fatalf("run %d restored %d targets, want %d", run, got, len(want))
				}
				if string(output) != strings.Join(want, "\n")+"\n" {
					t.Fatalf("run %d restored %s, want %v", run, output, want)
				}
				if batchSize > 0 {
					cursor, err := os.ReadFile(filepath.Join(work, "restore/tenant.cursor"))
					wantCursor := fmt.Sprintf("org_%05d\n", ((run+1)*batchSize-1)%test.tenants)
					if err != nil || string(cursor) != wantCursor {
						t.Fatalf("cursor = %q, %v; want %q", cursor, err, wantCursor)
					}
				}
			}
		})
	}
}

func TestRestoreInstallerChecksAndRollsBack(t *testing.T) {
	t.Parallel()
	installer, err := os.ReadFile("install-restore-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	restore, err := os.ReadFile("restore-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		check    bool
		fail     bool
		previous bool
	}{
		{"dry run", true, false, true},
		{"idempotent install", false, false, true},
		{"rollback", false, true, true},
		{"failed first install", false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			if err := os.Mkdir(filepath.Join(work, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeDeployFixture(t, filepath.Join(work, "restore-test.sh"), string(restore))
			writeDeployFixture(t, filepath.Join(work, "installer.sh"), strings.NewReplacer(
				"destination=/usr/local/sbin/detent-restore-test", `destination="$WORK/installed"`,
				`[ "$EUID" -eq 0 ]`, "true",
			).Replace(string(installer)))
			writeDeployFixture(t, filepath.Join(work, "bin/systemctl"), `#!/bin/bash
printf '%s\n' "$*" >> "$WORK/services"
if [ "$1" = restart ] && [ "$FAIL" = true ] && [ ! -e "$WORK/failed" ]; then
  touch "$WORK/failed"
  exit 1
fi
`)
			installedPath := filepath.Join(work, "installed")
			if test.previous {
				writeDeployFixture(t, installedPath, "previous verifier\n")
			}
			for run := range 2 {
				args := []string{filepath.Join(work, "installer.sh")}
				if test.check {
					args = append(args, "--check")
				}
				cmd := exec.CommandContext(t.Context(), "bash", args...)
				cmd.Env = []string{"PATH=" + filepath.Join(work, "bin") + ":" + os.Getenv("PATH"), "TMPDIR=" + work, "WORK=" + work, "FAIL=" + strconv.FormatBool(test.fail)}
				output, err := cmd.CombinedOutput()
				if (err != nil) != test.fail {
					t.Fatalf("install error = %v, output = %s", err, output)
				}
				if test.check || test.fail {
					break
				}
				if run == 0 {
					backup, err := os.ReadFile(installedPath + ".previous")
					if err != nil || string(backup) != "previous verifier\n" {
						t.Fatalf("backup = %q, %v", backup, err)
					}
				}
			}
			installed, err := os.ReadFile(installedPath)
			if test.fail && !test.previous {
				if !os.IsNotExist(err) {
					t.Fatalf("failed installation remained: %q, %v", installed, err)
				}
			} else {
				want := string(restore)
				if test.check || test.fail {
					want = "previous verifier\n"
				}
				if err != nil || string(installed) != want {
					t.Fatalf("installed = %q, %v; want %q", installed, err, want)
				}
			}
			services, err := os.ReadFile(filepath.Join(work, "services"))
			if err != nil {
				t.Fatal(err)
			}
			wantRestarts := 1
			if test.check {
				wantRestarts = 0
			} else if test.fail && test.previous {
				wantRestarts = 2
			}
			if got := strings.Count(string(services), "restart detent-restore-test.service\n"); got != wantRestarts {
				t.Fatalf("restarts = %d, want %d: %s", got, wantRestarts, services)
			}
			if test.check {
				if _, err := os.Stat(installedPath + ".previous"); !os.IsNotExist(err) {
					t.Fatalf("dry run created backup: %v", err)
				}
			}
		})
	}
}

func writeDeployFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
