package hub

import (
	"bytes"
	"fmt"
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
		name                   string
		wrongBinary            bool
		unsupportedSocket      bool
		socketInactive         bool
		existingInactive       bool
		staging                bool
		rollbackBootstrap      bool
		bootstrapEnableFailure bool
		bootstrapStopFailure   bool
		bootstrapActiveFailure bool
	}{
		{name: "active socket deploy"},
		{name: "wrong binary refuses deployment", wrongBinary: true},
		{name: "binary without socket support refuses deployment", unsupportedSocket: true},
		{name: "bootstrap deploy works before socket installation", socketInactive: true},
		{name: "staging bootstrap preserves port", socketInactive: true, staging: true},
		{name: "failed install preserves inactive existing socket files", socketInactive: true, existingInactive: true},
		{name: "failed bootstrap restores direct service", socketInactive: true, rollbackBootstrap: true},
		{name: "socket enable failure restores old binary without schema metadata", socketInactive: true, bootstrapEnableFailure: true},
		{name: "unverified service stop does not use legacy rollback", socketInactive: true, bootstrapStopFailure: true},
		{name: "unavailable inactive state does not start candidate", socketInactive: true, bootstrapActiveFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			serviceName, cloudUnit := "detent-hub", "detent-cloud"
			if test.staging {
				serviceName, cloudUnit = "detent-hub-staging", "detent-cloud-staging"
			}
			for _, name := range []string{"bin", "etc/systemd/system", "opt/" + serviceName + "/releases", "run/lock", "var/lib/" + serviceName, "tmp"} {
				if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "service-active"), []byte("true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			stubs := map[string]string{
				"sudo": `shift
if [ "$1" = -u ]; then shift 2; fi
case "$1" in
  install)
    shift
    if [ "$1" = -m ]; then
      mode=$2
      shift 2
      while [ "$1" = -o ] || [ "$1" = -g ]; do shift 2; done
      exec /usr/bin/install -m "$mode" "$@"
    fi
    exec /usr/bin/install "$@"
    ;;
  readlink) exec readlink "$FIXTURE_ROOT/opt/$FIXTURE_NAME/detent" ;;
  *) exec "$@" ;;
esac
`,
				"stat":  "wc -c < \"$3\"\n",
				"flock": "exit 0\n",
				"systemctl": `printf '%s\n' "$*" >> "$FIXTURE_ROOT/systemctl.log"
case "$1" in
  stop)
    if [ "$2" = "$FIXTURE_CLOUD" ]; then
      if [ "$FIXTURE_FAIL_STOP" = true ]; then exit 1; fi
      echo false > "$FIXTURE_ROOT/service-active"
    fi
    ;;
  start)
    if [ "$2" = "$FIXTURE_CLOUD" ]; then echo true > "$FIXTURE_ROOT/service-active"; fi
    ;;
  enable) [ "$FIXTURE_FAIL_ENABLE" != true ] ;;
  show)
    if [ "$2" = -p ] && [ "$3" = ActiveState ]; then
      [ "$FIXTURE_FAIL_ACTIVE_STATE" != true ] || exit 1
      if [ "$(cat "$FIXTURE_ROOT/service-active")" = true ]; then echo active; else echo inactive; fi
      exit 0
    fi
    count=0
    if [ -f "$FIXTURE_ROOT/show-count" ]; then count=$(cat "$FIXTURE_ROOT/show-count"); fi
    count=$((count + 1))
    printf '%s' "$count" > "$FIXTURE_ROOT/show-count"
    if { [ "$FIXTURE_FAIL_CANDIDATE" = true ] || [ "$FIXTURE_FAIL_STOP" = true ] || [ "$FIXTURE_FAIL_ACTIVE_STATE" = true ]; } && [ "$count" = 2 ]; then printf '0\n'; else printf '1\n'; fi
    ;;
  is-active)
    case "$3" in
      "$FIXTURE_CLOUD") [ "$(cat "$FIXTURE_ROOT/service-active")" = true ] ;;
      "$FIXTURE_CLOUD.socket") [ "$FIXTURE_SOCKET_ACTIVE" = true ] ;;
    esac
    ;;
esac
`,
				"pgrep": "exit 1\n",
				"curl":  "cat \"$FIXTURE_ROOT/health.json\"\n",
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
			healthCommit := commit[:9]
			if test.rollbackBootstrap || test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure {
				healthCommit = strings.Repeat("b", 9)
			}
			health := fmt.Sprintf(`{"status":"ok","commit":"%s","schema_version":1,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}`, healthCommit)
			if test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure {
				health = fmt.Sprintf(`{"commit":"%s","status":"ok","version":"0.173.4"}`, strings.Repeat("b", 40))
			}
			if err := os.WriteFile(filepath.Join(dir, "health.json"), []byte(health), 0600); err != nil {
				t.Fatal(err)
			}
			socketSupported := !test.unsupportedSocket
			versionJSON := fmt.Sprintf(`{"commit":"%s","socket_activation_supported":%t,"hub_schema_version":1,"hub_schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}`, reported, socketSupported)
			binary := "#!/bin/bash\nprintf '%s\\n' '" + versionJSON + "'\nexit 0\n" + strings.Repeat(" ", 1000001)
			isolated := string(script)
			replacements := []string{
				"/opt/", filepath.ToSlash(dir) + "/opt/",
				"/etc/", filepath.ToSlash(dir) + "/etc/",
				"/run/lock/", filepath.ToSlash(dir) + "/run/lock/",
				"/var/lib/", filepath.ToSlash(dir) + "/var/lib/",
				"/tmp/", filepath.ToSlash(dir) + "/tmp/",
				"NAME=detent-hub", "NAME=" + serviceName,
				"CLOUD=detent-cloud", "CLOUD=" + cloudUnit,
			}
			if test.staging {
				replacements = append(replacements, "DOMAIN=cloud.detent.build", "DOMAIN=staging.cloud.detent.build")
			}
			if test.existingInactive {
				if err := os.MkdirAll(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".socket"), []byte("existing socket config\n"), 0640); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf"), []byte("existing drop-in config\n"), 0640); err != nil {
					t.Fatal(err)
				}
			}
			isolated = strings.NewReplacer(replacements...).Replace(isolated)
			if test.rollbackBootstrap || test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure {
				previousRelease := filepath.Join(dir, "opt", serviceName, "releases", "detent-bbbbbbbbb")
				previousVersion := `{"commit":"` + strings.Repeat("b", 40) + `"}`
				if !test.bootstrapEnableFailure && !test.bootstrapStopFailure && !test.bootstrapActiveFailure {
					previousVersion = `{"commit":"` + strings.Repeat("b", 40) + `","hub_schema_version":1,"hub_schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}`
				}
				previousBinary := "#!/bin/bash\nprintf '%s\\n' '" + previousVersion + "'\nexit 0\n"
				if err := os.WriteFile(previousRelease, []byte(previousBinary), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(previousRelease, filepath.Join(dir, "opt", serviceName, "detent")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "var/lib", serviceName, "DEPLOYED_SHA"), []byte(strings.Repeat("b", 40)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "var/lib", serviceName, "hub.db"), []byte("previous database state"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			isolatedPath := filepath.Join(dir, "deploy-from-ci.sh")
			if err := os.WriteFile(isolatedPath, []byte(isolated), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", isolatedPath)
			command.Stdin = strings.NewReader(binary)
			command.Env = []string{
				"PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin",
				"TMPDIR=" + dir,
				"SSH_ORIGINAL_COMMAND=deploy " + commit,
				"FIXTURE_SOCKET_ACTIVE=" + fmt.Sprint(!test.socketInactive),
				"FIXTURE_FAIL_CANDIDATE=" + fmt.Sprint(test.rollbackBootstrap),
				"FIXTURE_FAIL_ENABLE=" + fmt.Sprint(test.bootstrapEnableFailure),
				"FIXTURE_FAIL_STOP=" + fmt.Sprint(test.bootstrapStopFailure),
				"FIXTURE_FAIL_ACTIVE_STATE=" + fmt.Sprint(test.bootstrapActiveFailure),
				"FIXTURE_NAME=" + serviceName,
				"FIXTURE_CLOUD=" + cloudUnit,
				"FIXTURE_ROOT=" + dir,
			}
			output, err := command.CombinedOutput()
			refused := test.wrongBinary || test.unsupportedSocket || test.existingInactive || test.rollbackBootstrap || test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure
			if (err != nil) != refused {
				t.Fatalf("deploy = %v, want refused %t: %s", err, refused, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "systemctl.log"))
			if test.wrongBinary || test.unsupportedSocket {
				if !os.IsNotExist(err) {
					t.Fatalf("unverified deployment changed services: %s (%v)", calls, err)
				}
			} else if test.existingInactive {
				if err != nil {
					t.Fatal(err)
				}
				for path, want := range map[string]string{
					cloudUnit + ".socket":                "existing socket config\n",
					cloudUnit + ".service.d/socket.conf": "existing drop-in config\n",
				} {
					got, readErr := os.ReadFile(filepath.Join(dir, "etc/systemd/system", path))
					if readErr != nil || string(got) != want {
						t.Fatalf("existing systemd file %s = %q, want %q (%v)", path, got, want, readErr)
					}
				}
				if strings.Contains(string(calls), "\nstop ") || strings.Contains(string(calls), "\nstart ") || strings.Contains(string(calls), "\nenable ") || strings.Contains(string(calls), "\ndisable ") {
					t.Fatalf("refusal changed service state: %s", calls)
				}
			} else if test.bootstrapStopFailure || test.bootstrapActiveFailure {
				if err != nil {
					t.Fatal(err)
				}
				target, readErr := os.Readlink(filepath.Join(dir, "opt", serviceName, "detent"))
				if readErr != nil || !strings.Contains(filepath.Base(target), "detent-ci-"+commit[:9]+"-") {
					t.Fatalf("uncertain stop rolled back to %q, want candidate remain selected (%v)", target, readErr)
				}
				active, readErr := os.ReadFile(filepath.Join(dir, "service-active"))
				wantActive := "true\n"
				if test.bootstrapActiveFailure {
					wantActive = "false\n"
				}
				if readErr != nil || string(active) != wantActive {
					t.Fatalf("service active state = %q after unverified stop, want %q (%v)", active, wantActive, readErr)
				}
				log := string(calls)
				if strings.Contains(log, "enable "+cloudUnit+".socket") || strings.Contains(log, "start "+cloudUnit+".socket") || strings.Contains(log, "start "+cloudUnit+"\n") {
					t.Fatalf("failed stop proceeded to candidate or rollback start: %s", log)
				}
			} else if test.rollbackBootstrap || test.bootstrapEnableFailure {
				if err != nil {
					t.Fatal(err)
				}
				previousRelease := filepath.Join(dir, "opt", serviceName, "releases", "detent-bbbbbbbbb")
				target, err := os.Readlink(filepath.Join(dir, "opt", serviceName, "detent"))
				if err != nil || target != previousRelease {
					t.Fatalf("failed bootstrap selected %q, want previous %q: %v", target, previousRelease, err)
				}
				receipt, err := os.ReadFile(filepath.Join(dir, "var/lib", serviceName, "DEPLOYED_SHA"))
				if err != nil || string(receipt) != strings.Repeat("b", 40)+"\n" {
					t.Fatalf("failed bootstrap changed deployment receipt: %q, %v", receipt, err)
				}
				database, err := os.ReadFile(filepath.Join(dir, "var/lib", serviceName, "hub.db"))
				if err != nil || string(database) != "previous database state" {
					t.Fatalf("failed bootstrap changed previous database state: %q, %v", database, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "etc/systemd/system", cloudUnit+".socket")); !os.IsNotExist(err) {
					t.Fatalf("failed bootstrap left socket unit installed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf")); !os.IsNotExist(err) {
					t.Fatalf("failed bootstrap left service socket drop-in installed: %v", err)
				}
				log := string(calls)
				ordered := []string{"enable " + cloudUnit + ".socket", "stop " + cloudUnit + ".socket", "disable " + cloudUnit + ".socket", "start " + cloudUnit + "\n"}
				if test.rollbackBootstrap {
					ordered = []string{"start " + cloudUnit + ".socket", "start " + cloudUnit + "\n", "stop " + cloudUnit + "\n", "stop " + cloudUnit + ".socket", "disable " + cloudUnit + ".socket", "start " + cloudUnit + "\n"}
				}
				position := -1
				for _, call := range ordered {
					next := strings.Index(log[position+1:], call)
					if next < 0 {
						t.Fatalf("failed bootstrap control sequence missing %q: %s", call, log)
					}
					position += next + 1
				}
				if test.bootstrapEnableFailure && strings.Contains(string(calls), "start "+cloudUnit+".socket") {
					t.Fatalf("failed socket enable started listener: %s", calls)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(calls), "start "+cloudUnit+"\n") || strings.Contains(string(calls), "litestream") {
					t.Fatalf("binary deployment changed replication or did not restart Hub: %s", calls)
				}
				deployed, err := os.ReadFile(filepath.Join(dir, "var/lib", serviceName, "DEPLOYED_SHA"))
				if err != nil || strings.TrimSpace(string(deployed)) != commit {
					t.Fatalf("deployment receipt = %q, %v", deployed, err)
				}
				installed, err := os.ReadFile(filepath.Join(dir, "opt", serviceName, "detent"))
				if err != nil || string(installed) != binary {
					t.Fatalf("installed binary differs from upload: %v", err)
				}
				calls, err := os.ReadFile(filepath.Join(dir, "systemctl.log"))
				if err != nil {
					t.Fatal(err)
				}
				if test.socketInactive {
					address := "127.0.0.1:8017"
					if test.staging {
						address = "127.0.0.1:8018"
					}
					socketUnit := cloudUnit + ".socket"
					socket, err := os.ReadFile(filepath.Join(dir, "etc/systemd/system", socketUnit))
					if err != nil || !strings.Contains(string(socket), "ListenStream="+address+"\n") || !strings.Contains(string(socket), "Backlog=65535\n") {
						t.Fatalf("rendered socket unit = %q, %v", socket, err)
					}
					dropin, err := os.ReadFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf"))
					if err != nil || !strings.Contains(string(dropin), "Requires="+socketUnit) || !strings.Contains(string(dropin), "Sockets="+socketUnit) {
						t.Fatalf("rendered socket drop-in = %q, %v", dropin, err)
					}
					log := string(calls)
					ordered := []string{"stop " + cloudUnit, "enable " + socketUnit, "start " + socketUnit, "start " + cloudUnit}
					position := -1
					for _, call := range ordered {
						next := strings.Index(log[position+1:], call)
						if next < 0 {
							t.Fatalf("socket bootstrap control sequence missing %q: %s", call, log)
						}
						position += next + 1
					}
					if strings.Contains(log, "start "+serviceName+".socket") {
						t.Fatalf("Cloud bootstrap started the unused Hub socket: %s", log)
					}
				} else if strings.Contains(string(calls), "start "+cloudUnit+".socket") || strings.Contains(string(calls), "enable "+cloudUnit+".socket") {
					t.Fatalf("active socket deploy repeated bootstrap: %s", calls)
				}
			}
			staged, err := filepath.Glob(filepath.Join(dir, "detent-hub-upload.*"))
			if err != nil || len(staged) != 0 {
				t.Fatalf("upload retained: %v %v", staged, err)
			}
		})
	}
}

func TestSocketActivationInstallerIsRepeatable(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	for _, attempt := range []string{"first", "repeat"} {
		command := exec.CommandContext(t.Context(), "bash", "install-socket-activation.sh")
		command.Env = append(os.Environ(), "DESTDIR="+destination)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s install = %v: %s", attempt, err, output)
		}
	}
	for _, file := range []struct {
		destination string
		source      string
	}{
		{destination: "detent-cloud.socket", source: "cloud/detent-cloud.socket"},
		{destination: "detent-cloud.service.d/socket.conf", source: "cloud/detent-cloud.service.d/socket.conf"},
		{destination: "detent-hub.socket", source: "hub/detent-hub.socket"},
		{destination: "detent-hub.service.d/socket.conf", source: "hub/detent-hub.service.d/socket.conf"},
	} {
		installed, err := os.ReadFile(filepath.Join(destination, "etc/systemd/system", file.destination))
		if err != nil {
			t.Fatalf("read installed %s: %v", file.destination, err)
		}
		expected, err := os.ReadFile(filepath.Join(root, "docs/examples", file.source))
		if err != nil {
			t.Fatalf("read source %s: %v", file.source, err)
		}
		if !bytes.Equal(installed, expected) {
			t.Errorf("installed %s differs from source", file.destination)
		}
	}
}
