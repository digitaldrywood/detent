package hub

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLitestreamInstallerPreservesWorkingConfiguration(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("install-litestream-production.sh")
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile("litestream-production.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		argument     string
		failure      string
		current      bool
		inactive     bool
		missing      bool
		wantFailure  bool
		wantRestarts int
	}{
		{name: "install and repeat", wantRestarts: 1},
		{name: "check outdated config", argument: "--check"},
		{name: "check current config", argument: "--check", current: true},
		{name: "current active config", current: true},
		{name: "current inactive config recovers service", current: true, inactive: true, wantRestarts: 1},
		{name: "restart failure restores replication", failure: "restart", wantFailure: true, wantRestarts: 2},
		{name: "inactive service restores replication", failure: "inactive", wantFailure: true, wantRestarts: 2},
		{name: "recovery failure retains backup", failure: "recovery", wantFailure: true, wantRestarts: 2},
		{name: "install failure preserves working config", failure: "install", wantFailure: true},
		{name: "lock failure preserves working config", failure: "lock", wantFailure: true},
		{name: "missing working config refuses install", missing: true, wantFailure: true},
		{name: "unknown option refuses install", argument: "--unknown", wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range []string{"bin", "etc/litestream-detent", "run/lock"} {
				if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			previous := []byte("dbs: []\n")
			if test.current {
				previous = config
			}
			configPath := filepath.Join(dir, "etc/litestream-detent/production.yml")
			files := map[string][]byte{
				"litestream-production.json": config,
				"active":                     []byte("true\n"),
			}
			if test.inactive {
				files["active"] = []byte("false\n")
			}
			if !test.missing {
				files["etc/litestream-detent/production.yml"] = previous
			}
			isolated := strings.NewReplacer(
				"/etc/", filepath.ToSlash(dir)+"/etc/",
				"/run/lock/", filepath.ToSlash(dir)+"/run/lock/",
			).Replace(string(script))
			files["installer.sh"] = []byte(isolated)
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0640); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range map[string]string{
				"flock": "test \"$FIXTURE_FAILURE\" != lock\n",
				"install": `test "$FIXTURE_FAILURE" != install
exec /usr/bin/install "$@"
`,
				"systemctl": `printf '%s\n' "$*" >> "$FIXTURE_ROOT/systemctl.log"
test "${!#}" = litestream-detent@production.service
case "$1" in
  restart)
    count=0
    if [ -f "$FIXTURE_ROOT/restarts" ]; then read -r count < "$FIXTURE_ROOT/restarts"; fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$FIXTURE_ROOT/restarts"
    cp "$FIXTURE_ROOT/etc/litestream-detent/production.yml" "$FIXTURE_ROOT/restart-$count"
    echo false > "$FIXTURE_ROOT/active"
    if [ "$FIXTURE_FAILURE" = recovery ] || { [ "$FIXTURE_FAILURE" = restart ] && [ "$count" = 1 ]; }; then exit 1; fi
    if [ "$FIXTURE_FAILURE" != inactive ] || [ "$count" != 1 ]; then echo true > "$FIXTURE_ROOT/active"; fi
    ;;
  is-active) test "$(cat "$FIXTURE_ROOT/active")" = true ;;
  *) exit 2 ;;
esac
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/bash\nset -eu\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			run := func() []byte {
				t.Helper()
				arguments := []string{filepath.Join(dir, "installer.sh")}
				if test.argument != "" {
					arguments = append(arguments, test.argument)
				}
				command := exec.CommandContext(t.Context(), "bash", arguments...)
				command.Env = []string{
					"PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin",
					"TMPDIR=" + dir,
					"FIXTURE_ROOT=" + dir,
					"FIXTURE_FAILURE=" + test.failure,
				}
				output, err := command.CombinedOutput()
				if (err != nil) != test.wantFailure {
					t.Fatalf("install = %v, want failure %t: %s", err, test.wantFailure, output)
				}
				return output
			}
			output := run()
			if !test.wantFailure && test.argument == "" {
				run()
			}
			installed, err := os.ReadFile(configPath)
			if test.missing {
				if !os.IsNotExist(err) {
					t.Fatalf("missing working config was replaced: %v", err)
				}
			} else {
				want := config
				if test.wantFailure || test.argument == "--check" {
					want = previous
				}
				if err != nil || !bytes.Equal(installed, want) {
					t.Fatalf("installed config = %q, want %q (%v): %s", installed, want, err, output)
				}
				info, err := os.Stat(configPath)
				if err != nil {
					t.Fatal(err)
				}
				wantMode := os.FileMode(0640)
				if test.wantRestarts > 0 && !test.wantFailure {
					wantMode = 0644
				}
				if info.Mode().Perm() != wantMode {
					t.Fatalf("config permissions = %o, want %o", info.Mode().Perm(), wantMode)
				}
			}
			restarts, err := filepath.Glob(filepath.Join(dir, "restart-*"))
			if err != nil || len(restarts) != test.wantRestarts {
				t.Fatalf("restarts = %v, want %d (%v): %s", restarts, test.wantRestarts, err, output)
			}
			for i, restart := range restarts {
				data, err := os.ReadFile(restart)
				want := config
				if i > 0 {
					want = previous
				}
				if err != nil || !bytes.Equal(data, want) {
					t.Fatalf("restart %d used config %q, want %q (%v)", i+1, data, want, err)
				}
			}
			active, err := os.ReadFile(filepath.Join(dir, "active"))
			wantActive := "true\n"
			if test.failure == "recovery" {
				wantActive = "false\n"
				if !strings.Contains(string(output), "Litestream recovery failed") {
					t.Fatalf("recovery failure not reported: %s", output)
				}
			}
			if err != nil || string(active) != wantActive {
				t.Fatalf("service active = %q, want %q (%v)", active, wantActive, err)
			}
			backups, err := filepath.Glob(configPath + ".backup.*")
			wantBackups := 0
			if test.wantRestarts > 0 || test.failure == "install" {
				wantBackups = 1
			}
			if err != nil || len(backups) != wantBackups {
				t.Fatalf("backups = %v, want %d (%v)", backups, wantBackups, err)
			}
			for _, backup := range backups {
				data, err := os.ReadFile(backup)
				if err != nil || !bytes.Equal(data, previous) {
					t.Fatalf("backup lost working configuration: %q (%v)", data, err)
				}
			}
			staged, err := filepath.Glob(configPath + ".install.*")
			if err != nil || len(staged) != 0 {
				t.Fatalf("staged configuration retained: %v (%v)", staged, err)
			}
			if test.argument == "--check" {
				if _, err := os.Stat(filepath.Join(dir, "systemctl.log")); !os.IsNotExist(err) {
					t.Fatalf("dry run changed services: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "run/lock/detent-hub-deploy.lock")); !os.IsNotExist(err) {
					t.Fatalf("dry run wrote deployment lock: %v", err)
				}
			}
		})
	}
}

func TestProductionDeployAcceptsBinaryWithoutReceiverUpgrade(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("deploy-from-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		wrongBinary bool
	}{
		{name: "existing binary protocol"},
		{name: "wrong binary refuses deployment", wrongBinary: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range []string{"bin", "opt/detent-hub/releases", "run/lock", "var/lib/detent-hub", "tmp"} {
				if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			stubs := map[string]string{
				"sudo": `shift
if [ "$1" = -u ]; then shift 2; fi
case "$1" in
  install) exec /usr/bin/install -m "$3" "$8" "$9" ;;
  readlink) exec readlink "$FIXTURE_ROOT/opt/detent-hub/detent" ;;
  *) exec "$@" ;;
esac
`,
				"stat":  "wc -c < \"$3\"\n",
				"flock": "exit 0\n",
				"systemctl": `printf '%s\n' "$*" >> "$FIXTURE_ROOT/systemctl.log"
case "$1" in
  show) printf '1\n' ;;
esac
`,
				"pgrep": "exit 1\n",
				"curl":  "printf '200'\n",
			}
			for name, body := range stubs {
				if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/bash\nset -eu\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			commit := strings.Repeat("a", 40)
			reported := commit
			if test.wrongBinary {
				reported = strings.Repeat("b", 40)
			}
			binary := "#!/bin/bash\nprintf '%s\\n' '{\"commit\":\"" + reported + "\"}'\nexit 0\n" + strings.Repeat(" ", 1000001)
			isolated := string(script)
			isolated = strings.NewReplacer(
				"/opt/", filepath.ToSlash(dir)+"/opt/",
				"/etc/", filepath.ToSlash(dir)+"/etc/",
				"/run/lock/", filepath.ToSlash(dir)+"/run/lock/",
				"/var/lib/", filepath.ToSlash(dir)+"/var/lib/",
				"/tmp/", filepath.ToSlash(dir)+"/tmp/",
			).Replace(isolated)
			command := exec.CommandContext(t.Context(), "bash", "-c", isolated)
			command.Stdin = strings.NewReader(binary)
			command.Env = []string{
				"PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin",
				"TMPDIR=" + dir,
				"SSH_ORIGINAL_COMMAND=deploy " + commit,
				"FIXTURE_ROOT=" + dir,
			}
			output, err := command.CombinedOutput()
			refused := test.wrongBinary
			if (err != nil) != refused {
				t.Fatalf("deploy = %v, want refused %t: %s", err, refused, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "systemctl.log"))
			if test.wrongBinary {
				if !os.IsNotExist(err) {
					t.Fatalf("unverified deployment changed services: %s (%v)", calls, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(calls), "start detent-cloud\n") || strings.Contains(string(calls), "litestream") {
					t.Fatalf("binary deployment changed replication or did not restart Hub: %s", calls)
				}
				deployed, err := os.ReadFile(filepath.Join(dir, "var/lib/detent-hub/DEPLOYED_SHA"))
				if err != nil || strings.TrimSpace(string(deployed)) != commit {
					t.Fatalf("deployment receipt = %q, %v", deployed, err)
				}
				installed, err := os.ReadFile(filepath.Join(dir, "opt/detent-hub/detent"))
				if err != nil || string(installed) != binary {
					t.Fatalf("installed binary differs from upload: %v", err)
				}
			}
			staged, err := filepath.Glob(filepath.Join(dir, "detent-hub-upload.*"))
			if err != nil || len(staged) != 0 {
				t.Fatalf("upload retained: %v %v", staged, err)
			}
		})
	}
}
