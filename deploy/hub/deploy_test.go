package hub

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPostgresCutoverReceiverInstallsAndInvokesExactPayload(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serviceUser := ""
	serviceUID := ""
	serviceGID := ""
	realSetpriv, setprivErr := exec.LookPath("setpriv")
	realPrivdrop := os.Geteuid() == 0 && setprivErr == nil
	if realPrivdrop {
		identity, err := user.Lookup("nobody")
		if err != nil {
			t.Skip("a separate unprivileged service identity is unavailable")
		}
		serviceUser, serviceUID, serviceGID = identity.Username, identity.Uid, identity.Gid
	} else {
		identity, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		serviceUser, serviceUID, serviceGID = identity.Username, identity.Uid, identity.Gid
	}
	for _, name := range []string{
		"bin", "etc/detent-hub", "opt/detent-hub/releases", "opt/detent-hub/sources", "run/lock", "var/lib/detent-hub/cutover", "tmp",
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if realPrivdrop {
		gid, err := strconv.Atoi(serviceGID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(root, 0, gid); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0750); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"opt", "opt/detent-hub", "opt/detent-hub/releases"} {
			path := filepath.Join(root, name)
			if err := os.Chown(path, 0, gid); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0750); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(filepath.Join(root, "tmp"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	commit := ""
	for _, name := range []string{"cloud.postgres.yaml", "cloud.yaml", "cloud.env", "attachments.env"} {
		if err := os.WriteFile(filepath.Join(root, "etc/detent-hub", name), []byte("configured\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureSource := filepath.Join(root, "frozen-source")
	if err := os.MkdirAll(filepath.Join(fixtureSource, "deploy/hub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureSource, "README.md"), []byte("frozen source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	receiver := []byte("#!/bin/bash\nset -eu\n[ \"$DETENT_DEPLOY_LOCK_HELD\" = true ]\n[ -f \"$FIXTURE_ROOT/deploy-lock-acquired\" ]\nprintf '%s\\n' \"$DETENT_DEPLOY_LOCK_HELD\" > \"$FIXTURE_ROOT/lock-held\"\nprintf '%s\\n' \"$@\" > \"$FIXTURE_ROOT/cutover-args\"\n")
	if err := os.WriteFile(filepath.Join(fixtureSource, "deploy/hub/cutover-postgres.sh"), receiver, 0700); err != nil {
		t.Fatal(err)
	}
	runGit := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", fixtureSource}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v = %v: %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	if output, err := exec.Command("git", "init", "-q", fixtureSource).CombinedOutput(); err != nil {
		t.Fatalf("git init = %v: %s", err, output)
	}
	runGit("config", "user.name", "Example Operator")
	runGit("config", "user.email", "operator@example.test")
	runGit("add", "README.md")
	runGit("commit", "-qm", "frozen source base")
	runGit("add", "deploy/hub/cutover-postgres.sh")
	runGit("commit", "-qm", "frozen cutover source")
	commit = runGit("rev-parse", "HEAD")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shallowSource := filepath.Join(root, "shallow-source")
	if err := os.MkdirAll(shallowSource, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(realGit, "-C", shallowSource, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init shallow source = %v: %s", err, output)
	}
	shallowGit := func(arguments ...string) string {
		t.Helper()
		command := exec.Command(realGit, append([]string{"-C", shallowSource}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v = %v: %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	shallowGit("fetch", "--no-tags", "--depth=1", fixtureSource, commit)
	shallowGit("checkout", "--quiet", "--detach", "FETCH_HEAD")
	sourceBundle := filepath.Join(root, "source.bundle")
	shallowGit("bundle", "create", sourceBundle, "HEAD")
	sourceData, err := os.ReadFile(sourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	versionJSON := fmt.Sprintf(`{"commit":%q,"postgres_storage_supported":true,"socket_activation_supported":true}`, commit)
	candidate := append([]byte("#!/bin/bash\nset -eu\nif [ \"$FIXTURE_REAL_PRIVDROP\" = true ]; then [ \"$(id -un)\" = \"$FIXTURE_SERVICE_USER\" ]; fi\nif [ \"${1:-}\" = version ]; then printf '%s\\n' '"+versionJSON+"'; else exit 0; fi\n"), bytes.Repeat([]byte(" "), 1000001)...)
	payloadPath := filepath.Join(root, "cutover.tar")
	payload, err := os.Create(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(payload)
	for name, data := range map[string][]byte{"detent": candidate, "cutover-postgres.sh": receiver, "source.bundle": sourceData} {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := payload.Close(); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("deploy-from-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	isolated := strings.NewReplacer(
		"/opt/", filepath.ToSlash(root)+"/opt/",
		"/etc/", filepath.ToSlash(root)+"/etc/",
		"/run/lock/", filepath.ToSlash(root)+"/run/lock/",
		"/var/lib/", filepath.ToSlash(root)+"/var/lib/",
		"/tmp/", filepath.ToSlash(root)+"/tmp/",
	).Replace(string(script))
	isolatedPath := filepath.Join(root, "deploy-from-ci.sh")
	if err := os.WriteFile(isolatedPath, []byte(isolated), 0700); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"sudo": `#!/bin/bash
set -eu
shift
if [ "${1:-}" = -u ]; then
  shift
  target=$1
  shift
  if [ "$FIXTURE_REAL_PRIVDROP" = true ] && [ "$target" = detent-hub ]; then
    exec "$FIXTURE_REAL_SETPRIV" --reuid="$FIXTURE_SERVICE_UID" --regid="$FIXTURE_SERVICE_GID" --init-groups -- "$@"
  fi
fi
case "${1:-}" in
  test|readlink|cmp) printf '%s\n' "$*" >> "$FIXTURE_ROOT/privileged-path-ops" ;;
esac
if [ "${1:-}" = chown ]; then
  [ "$FIXTURE_REAL_PRIVDROP" = true ] || exit 0
  shift
  recursive=
  if [ "${1:-}" = -R ]; then recursive=-R; shift; fi
  owner_group=${1/detent-hub/$FIXTURE_SERVICE_GID}
  shift
  exec chown $recursive "$owner_group" "$@"
fi
if [ "${1:-}" = install ]; then
  shift
  owner=root
  group=root
  args=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -o) owner=$2; shift 2 ;;
      -g) group=$2; shift 2 ;;
      *) args+=("$1"); shift ;;
    esac
  done
  if [ "$FIXTURE_REAL_PRIVDROP" != true ]; then exec /usr/bin/install "${args[@]}"; fi
  [ "$owner" != detent-hub ] || owner=$FIXTURE_SERVICE_USER
  [ "$group" != detent-hub ] || group=$FIXTURE_SERVICE_GID
  /usr/bin/install "${args[@]}"
  chown "$owner:$group" "${args[${#args[@]}-1]}"
  exit 0
fi
if [ "${1:-}" = mv ] && [ "${2:-}" = -T ]; then
  shift 2
  [ "${1:-}" = -- ] && shift
  [ ! -e "$2" ] || exit 1
  exec mv "$1" "$2"
fi
exec "$@"
`,
		"flock": "#!/bin/bash\n: > \"$FIXTURE_ROOT/deploy-lock-acquired\"\n",
		"stat":  "#!/bin/bash\n[ \"$1\" = -c ] && [ \"$2\" = %s ] || exit 2\nwc -c < \"$3\"\n",
		"git":   "#!/bin/bash\nexec \"$REAL_GIT\" \"$@\"\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.CommandContext(t.Context(), "bash", isolatedPath)
	input, err := os.Open(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	command.Stdin = input
	command.Env = []string{
		"PATH=" + filepath.Join(root, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin",
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"SSH_ORIGINAL_COMMAND=cutover-postgres " + commit,
		"FIXTURE_ROOT=" + root,
		"REAL_GIT=" + realGit,
		fmt.Sprintf("FIXTURE_REAL_PRIVDROP=%t", realPrivdrop),
		"FIXTURE_REAL_SETPRIV=" + realSetpriv,
		"FIXTURE_SERVICE_USER=" + serviceUser,
		"FIXTURE_SERVICE_UID=" + serviceUID,
		"FIXTURE_SERVICE_GID=" + serviceGID,
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("cutover install = %v: %s", err, output)
	}
	cutoverArgs, err := os.ReadFile(filepath.Join(root, "cutover-args"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(cutoverArgs))
	if len(args) != 20 {
		t.Fatalf("cutover argument count = %d, want 20: %v", len(args), args)
	}
	arguments := make(map[string]string, len(args)/2)
	for index := 0; index < len(args); index += 2 {
		arguments[args[index]] = args[index+1]
	}
	wants := map[string]string{
		"--entry-config":    filepath.Join(root, "etc/detent-hub/cloud.postgres.yaml"),
		"--cloud-config":    filepath.Join(root, "etc/detent-hub/cloud.yaml"),
		"--repository-root": filepath.Join(root, "opt/detent-hub/sources", commit),
		"--cloud-env":       filepath.Join(root, "etc/detent-hub/cloud.env"),
		"--attachments-env": filepath.Join(root, "etc/detent-hub/attachments.env"),
		"--source-state":    filepath.Join(root, "var/lib/detent-hub/cloud"),
		"--tenant-root":     filepath.Join(root, "var/lib/detent-hub/tenants"),
		"--backup-root":     filepath.Join(root, "var/lib/detent-hub/cutover", commit),
		"--health-url":      "https://cloud.detent.build/health",
	}
	for flag, want := range wants {
		if got := arguments[flag]; got != want {
			t.Fatalf("cutover %s = %q, want %q", flag, got, want)
		}
	}
	sourceRoot := arguments["--repository-root"]
	if got, err := exec.Command("git", "-c", "safe.directory="+sourceRoot, "-C", sourceRoot, "rev-parse", "HEAD").CombinedOutput(); err != nil || strings.TrimSpace(string(got)) != commit {
		t.Fatalf("installed source commit = %q (%v), want %q", got, err, commit)
	}
	if got, err := exec.Command("git", "-c", "safe.directory="+sourceRoot, "-C", sourceRoot, "status", "--porcelain", "--untracked-files=all").CombinedOutput(); err != nil || len(got) != 0 {
		t.Fatalf("installed source checkout is not clean: %q (%v)", got, err)
	}
	binary := arguments["--binary"]
	if !strings.HasPrefix(filepath.Base(binary), "detent-ci-"+commit+"-") {
		t.Fatalf("cutover binary path = %q, want exact immutable commit path", binary)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("cutover candidate binary was not installed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "lock-held")); err != nil || string(got) != "true\n" {
		t.Fatalf("deployment lock ownership = %q (%v)", got, err)
	}
	privilegedOps, err := os.ReadFile(filepath.Join(root, "privileged-path-ops"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"test -d " + sourceRoot,
		"test -L " + sourceRoot,
		"readlink -f " + sourceRoot,
		"cmp -s " + filepath.Join(root, "tmp"),
	} {
		if !strings.Contains(string(privilegedOps), expected) {
			t.Fatalf("privileged source path operation %q was not delegated: %s", expected, privilegedOps)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "opt/detent-hub/releases"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("immutable candidate and receiver entries = %v (%v)", entries, err)
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
		cutoverMarker          bool
		postgresSupported      bool
		previousPostgres       bool
		socketInactive         bool
		socketUnitUnknown      bool
		existingInactive       bool
		customActiveDropin     bool
		currentActiveDropin    bool
		activeDropinReloadFail bool
		staging                bool
		rollbackBootstrap      bool
		bootstrapEnableFailure bool
		bootstrapStopFailure   bool
		bootstrapActiveFailure bool
		execFailure            bool
		wrongProcessImage      bool
		healthyAfterExec       bool
	}{
		{name: "active socket deploy"},
		{name: "active socket rejects custom drop-in untouched", customActiveDropin: true},
		{name: "active socket with current drop-in deploys in place", currentActiveDropin: true},
		{name: "active socket reload failure preserves legacy drop-in", activeDropinReloadFail: true},
		{name: "wrong binary refuses deployment", wrongBinary: true},
		{name: "binary without socket support refuses deployment", unsupportedSocket: true},
		{name: "PostgreSQL marker refuses SQLite-only candidate", cutoverMarker: true},
		{name: "bootstrap deploy works before socket installation", socketInactive: true},
		{name: "bootstrap deploy accepts unknown socket unit with no files", socketInactive: true, socketUnitUnknown: true},
		{name: "staging bootstrap preserves port", socketInactive: true, staging: true},
		{name: "failed install preserves inactive existing socket files", socketInactive: true, existingInactive: true},
		{name: "unknown socket unit preserves existing files", socketInactive: true, socketUnitUnknown: true, existingInactive: true},
		{name: "failed bootstrap restores direct service", socketInactive: true, rollbackBootstrap: true},
		{name: "socket enable failure restores old binary without schema metadata", socketInactive: true, bootstrapEnableFailure: true},
		{name: "unverified service stop does not use legacy rollback", socketInactive: true, bootstrapStopFailure: true},
		{name: "unavailable inactive state does not start candidate", socketInactive: true, bootstrapActiveFailure: true},
		{name: "exec failure rolls back only to compatible previous release", execFailure: true},
		{name: "PostgreSQL cutover permits compatible PostgreSQL rollback", execFailure: true, cutoverMarker: true, postgresSupported: true, previousPostgres: true},
		{name: "PostgreSQL cutover refuses SQLite-only rollback", execFailure: true, cutoverMarker: true, postgresSupported: true},
		{name: "wrong running executable is not accepted", wrongProcessImage: true},
		{name: "exec starts before health facts finish loading", healthyAfterExec: true},
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
			if test.cutoverMarker {
				if err := os.WriteFile(filepath.Join(dir, "var/lib", serviceName, "POSTGRES_CUTOVER"), []byte("{\"backend\":\"postgres\"}\n"), 0600); err != nil {
					t.Fatal(err)
				}
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
  readlink)
    if [ "$FIXTURE_WRONG_PROCESS_IMAGE" = true ]; then echo /usr/bin/unexpected-detent; else exec readlink "$FIXTURE_ROOT/opt/$FIXTURE_NAME/detent"; fi
    ;;
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
    if [ "$2" = "$FIXTURE_CLOUD" ]; then
      if ! awk '$0 == "Type=exec" { found = 1 } END { exit !found }' "$FIXTURE_ROOT/etc/systemd/system/$FIXTURE_CLOUD.service.d/socket.conf"; then
        exit 2
      fi
      if [ "$FIXTURE_FAIL_EXEC" = true ] && [ ! -e "$FIXTURE_ROOT/service-start-failed" ]; then
        : > "$FIXTURE_ROOT/service-start-failed"
        echo false > "$FIXTURE_ROOT/service-active"
        exit 1
      fi
      echo true > "$FIXTURE_ROOT/service-active"
    fi
    ;;
  enable) [ "$FIXTURE_FAIL_ENABLE" != true ] ;;
  daemon-reload)
    if [ "$FIXTURE_FAIL_DAEMON_RELOAD" = true ] && [ ! -e "$FIXTURE_ROOT/daemon-reload-failed" ]; then
      : > "$FIXTURE_ROOT/daemon-reload-failed"
      exit 1
    fi
    ;;
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
    if { [ "$FIXTURE_FAIL_CANDIDATE" = true ] || [ "$FIXTURE_FAIL_STOP" = true ] || [ "$FIXTURE_FAIL_ACTIVE_STATE" = true ] || [ "$FIXTURE_FAIL_EXEC" = true ]; } && [ "$count" = 2 ]; then printf '0\n'; else printf '1\n'; fi
    ;;
  is-active)
    case "$3" in
      "$FIXTURE_CLOUD") [ "$(cat "$FIXTURE_ROOT/service-active")" = true ] ;;
      "$FIXTURE_CLOUD.socket")
        if [ "$FIXTURE_SOCKET_ACTIVE" = true ]; then exit 0; fi
        if [ "$FIXTURE_SOCKET_UNKNOWN" = true ]; then exit 4; else exit 3; fi
        ;;
    esac
    ;;
esac
`,
				"pgrep": "exit 1\n",
				"curl": `if [ "$FIXTURE_HEALTH_PENDING" = true ]; then
  count=0
  if [ -f "$FIXTURE_ROOT/health-count" ]; then count=$(cat "$FIXTURE_ROOT/health-count"); fi
  count=$((count + 1))
  printf '%s' "$count" > "$FIXTURE_ROOT/health-count"
  if [ "$count" = 1 ]; then cat "$FIXTURE_ROOT/health-pending.json"; else cat "$FIXTURE_ROOT/health.json"; fi
else
  cat "$FIXTURE_ROOT/health.json"
fi
`,
				"sleep": "printf '%s\\n' called >> \"$FIXTURE_ROOT/sleep.log\"\n",
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
			if test.rollbackBootstrap || test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure || test.execFailure || test.wrongProcessImage {
				healthCommit = strings.Repeat("b", 9)
			}
			health := fmt.Sprintf(`{"status":"ok","commit":"%s","schema_version":1,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}`, healthCommit)
			if test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure {
				health = fmt.Sprintf(`{"commit":"%s","status":"ok","version":"0.173.4"}`, strings.Repeat("b", 40))
			}
			if test.healthyAfterExec {
				pending := fmt.Sprintf(`{"status":"starting","commit":"%s","schema_version":1,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":false,"migrations_running":false}`, healthCommit)
				if err := os.WriteFile(filepath.Join(dir, "health-pending.json"), []byte(pending), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "health.json"), []byte(health), 0600); err != nil {
				t.Fatal(err)
			}
			socketSupported := !test.unsupportedSocket
			versionJSON := fmt.Sprintf(`{"commit":"%s","socket_activation_supported":%t,"postgres_storage_supported":%t,"hub_schema_version":1,"hub_schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}`, reported, socketSupported, test.postgresSupported)
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
			if !test.socketInactive {
				if err := os.MkdirAll(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d"), 0700); err != nil {
					t.Fatal(err)
				}
				address := "127.0.0.1:8017"
				if test.staging {
					address = "127.0.0.1:8018"
				}
				socketUnit := fmt.Sprintf("[Unit]\nDescription=Detent %s listener\n\n[Socket]\nListenStream=%s\nAccept=no\nBacklog=65535\n\n[Install]\nWantedBy=sockets.target\n", cloudUnit, address)
				legacyDropin := fmt.Sprintf("[Unit]\nRequires=%s.socket\nAfter=%s.socket\n\n[Service]\nSockets=%s.socket\n", cloudUnit, cloudUnit, cloudUnit)
				if test.customActiveDropin {
					legacyDropin += "Environment=UNOWNED_SETTING=true\n"
				}
				if test.currentActiveDropin {
					legacyDropin = fmt.Sprintf("[Unit]\nRequires=%s.socket\nAfter=%s.socket\n\n[Service]\nType=exec\nSockets=%s.socket\n", cloudUnit, cloudUnit, cloudUnit)
				}
				if err := os.WriteFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".socket"), []byte(socketUnit), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf"), []byte(legacyDropin), 0600); err != nil {
					t.Fatal(err)
				}
			}
			isolated = strings.NewReplacer(replacements...).Replace(isolated)
			if test.rollbackBootstrap || test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure || test.execFailure || test.wrongProcessImage {
				previousRelease := filepath.Join(dir, "opt", serviceName, "releases", "detent-bbbbbbbbb")
				previousVersion := `{"commit":"` + strings.Repeat("b", 40) + `"}`
				if !test.bootstrapEnableFailure && !test.bootstrapStopFailure && !test.bootstrapActiveFailure {
					previousVersion = fmt.Sprintf(`{"commit":"%s","postgres_storage_supported":%t,"hub_schema_version":1,"hub_schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}`, strings.Repeat("b", 40), test.previousPostgres)
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
				"FIXTURE_SOCKET_UNKNOWN=" + fmt.Sprint(test.socketUnitUnknown),
				"FIXTURE_FAIL_CANDIDATE=" + fmt.Sprint(test.rollbackBootstrap),
				"FIXTURE_FAIL_ENABLE=" + fmt.Sprint(test.bootstrapEnableFailure),
				"FIXTURE_FAIL_STOP=" + fmt.Sprint(test.bootstrapStopFailure),
				"FIXTURE_FAIL_ACTIVE_STATE=" + fmt.Sprint(test.bootstrapActiveFailure),
				"FIXTURE_FAIL_EXEC=" + fmt.Sprint(test.execFailure),
				"FIXTURE_WRONG_PROCESS_IMAGE=" + fmt.Sprint(test.wrongProcessImage),
				"FIXTURE_FAIL_DAEMON_RELOAD=" + fmt.Sprint(test.activeDropinReloadFail),
				"FIXTURE_HEALTH_PENDING=" + fmt.Sprint(test.healthyAfterExec),
				"FIXTURE_NAME=" + serviceName,
				"FIXTURE_CLOUD=" + cloudUnit,
				"FIXTURE_ROOT=" + dir,
			}
			output, err := command.CombinedOutput()
			refused := test.wrongBinary || test.unsupportedSocket || test.cutoverMarker && !test.postgresSupported || test.existingInactive || test.customActiveDropin || test.activeDropinReloadFail || test.rollbackBootstrap || test.bootstrapEnableFailure || test.bootstrapStopFailure || test.bootstrapActiveFailure || test.execFailure || test.wrongProcessImage
			if (err != nil) != refused {
				t.Fatalf("deploy = %v, want refused %t: %s", err, refused, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "systemctl.log"))
			if test.wrongBinary || test.unsupportedSocket || test.cutoverMarker && !test.postgresSupported {
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
			} else if test.customActiveDropin || test.activeDropinReloadFail {
				if err != nil {
					t.Fatal(err)
				}
				wantDropin := fmt.Sprintf("[Unit]\nRequires=%s.socket\nAfter=%s.socket\n\n[Service]\nSockets=%s.socket\n", cloudUnit, cloudUnit, cloudUnit)
				if test.customActiveDropin {
					wantDropin += "Environment=UNOWNED_SETTING=true\n"
				}
				got, readErr := os.ReadFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf"))
				if readErr != nil || string(got) != wantDropin {
					t.Fatalf("active service drop-in changed to %q, want existing %q (%v)", got, wantDropin, readErr)
				}
				if strings.Contains(string(calls), "\nstop ") || strings.Contains(string(calls), "\nstart ") || strings.Contains(string(calls), "\nenable ") {
					t.Fatalf("refused active socket deployment changed service state: %s", calls)
				}
				if _, readErr := os.Lstat(filepath.Join(dir, "opt", serviceName, "detent")); !os.IsNotExist(readErr) {
					t.Fatalf("refused active socket deployment selected candidate: %v", readErr)
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
			} else if test.execFailure && test.cutoverMarker && !test.previousPostgres {
				if err != nil {
					t.Fatal(err)
				}
				target, readErr := os.Readlink(filepath.Join(dir, "opt", serviceName, "detent"))
				if readErr != nil || !strings.Contains(filepath.Base(target), "detent-ci-"+commit[:9]+"-") {
					t.Fatalf("incompatible rollback selected %q, want candidate retained (%v)", target, readErr)
				}
				if _, readErr := os.Stat(filepath.Join(dir, "var/lib", serviceName, "POSTGRES_CUTOVER")); readErr != nil {
					t.Fatalf("incompatible rollback removed the PostgreSQL marker: %v", readErr)
				}
			} else if test.execFailure {
				if err != nil {
					t.Fatal(err)
				}
				previousRelease := filepath.Join(dir, "opt", serviceName, "releases", "detent-bbbbbbbbb")
				target, readErr := os.Readlink(filepath.Join(dir, "opt", serviceName, "detent"))
				if readErr != nil || target != previousRelease {
					t.Fatalf("failed exec selected %q, want previous %q (%v)", target, previousRelease, readErr)
				}
				receipt, readErr := os.ReadFile(filepath.Join(dir, "var/lib", serviceName, "DEPLOYED_SHA"))
				if readErr != nil || string(receipt) != strings.Repeat("b", 40)+"\n" {
					t.Fatalf("failed exec changed deployment receipt: %q (%v)", receipt, readErr)
				}
				database, readErr := os.ReadFile(filepath.Join(dir, "var/lib", serviceName, "hub.db"))
				if readErr != nil || string(database) != "previous database state" {
					t.Fatalf("failed exec changed database: %q (%v)", database, readErr)
				}
				calls, readErr := os.ReadFile(filepath.Join(dir, "systemctl.log"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if strings.Count(string(calls), "start "+cloudUnit+"\n") != 2 || strings.Contains(string(calls), "stop "+cloudUnit+".socket") {
					t.Fatalf("failed exec did not roll back with active listener preserved: %s", calls)
				}
				updatedDropin, readErr := os.ReadFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf"))
				if readErr != nil || !strings.Contains(string(updatedDropin), "Type=exec\n") {
					t.Fatalf("exec failure lost socket start contract: %q (%v)", updatedDropin, readErr)
				}
			} else if test.wrongProcessImage {
				if err != nil {
					t.Fatal(err)
				}
				candidate := filepath.Join(dir, "opt", serviceName, "releases", "detent-ci-"+commit[:9])
				target, readErr := os.Readlink(filepath.Join(dir, "opt", serviceName, "detent"))
				if readErr != nil || !strings.Contains(target, candidate) {
					t.Fatalf("wrong running image was treated as success or rolled back: %q (%v)", target, readErr)
				}
				receipt, readErr := os.ReadFile(filepath.Join(dir, "var/lib", serviceName, "DEPLOYED_SHA"))
				if readErr != nil || string(receipt) != strings.Repeat("b", 40)+"\n" {
					t.Fatalf("wrong running image changed deployment receipt: %q (%v)", receipt, readErr)
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
				if !strings.Contains(string(calls), "start "+cloudUnit+"\n") {
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
					if err != nil || !strings.Contains(string(dropin), "Requires="+socketUnit) || !strings.Contains(string(dropin), "Sockets="+socketUnit) || !strings.Contains(string(dropin), "Type=exec") {
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
				} else {
					dropin, readErr := os.ReadFile(filepath.Join(dir, "etc/systemd/system", cloudUnit+".service.d/socket.conf"))
					if readErr != nil || !strings.Contains(string(dropin), "Type=exec\n") {
						t.Fatalf("active socket drop-in = %q, want Type=exec (%v)", dropin, readErr)
					}
					if !strings.Contains(string(calls), "daemon-reload\n") || strings.Contains(string(calls), "start "+cloudUnit+".socket") || strings.Contains(string(calls), "enable "+cloudUnit+".socket") || strings.Contains(string(calls), "stop "+cloudUnit+".socket") {
						t.Fatalf("active socket upgrade did not reload in place: %s", calls)
					}
				}
				if test.healthyAfterExec {
					count, readErr := os.ReadFile(filepath.Join(dir, "health-count"))
					if readErr != nil || string(count) != "2" {
						t.Fatalf("health probes = %q, want incomplete then complete (%v)", count, readErr)
					}
					sleeps, readErr := os.ReadFile(filepath.Join(dir, "sleep.log"))
					if readErr != nil || string(sleeps) != "called\n" {
						t.Fatalf("readiness wait calls = %q, want one condition retry (%v)", sleeps, readErr)
					}
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
