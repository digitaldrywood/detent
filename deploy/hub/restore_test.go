package hubdeploy

import (
	"bytes"
	"database/sql"
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

func TestDeploymentOwnerInstallsReleaseRestoreScript(t *testing.T) {
	t.Parallel()
	owner, err := os.ReadFile("deploy-from-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	restoreScript, err := os.ReadFile("restore-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, environment string
		invalidIdentity   bool
		installScript     bool
	}{
		{"production bundle", "production", false, true},
		{"staging binary", "staging", false, false},
		{"rejected identity", "production", true, false},
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
			if test.installScript {
				want = restoreScript
			}
			if !bytes.Equal(installed, want) {
				t.Fatal("deployment owner installed the wrong restore script")
			}
		})
	}
}
