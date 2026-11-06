package hub

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPostgresCutoverFencesCopiesAndSelectsPostgresRelease(t *testing.T) {
	for _, test := range []cutoverCase{
		{name: "complete cutover", wantPostgres: true},
		{name: "relative configuration paths retain authority", relativeConfigPaths: true, wantPostgres: true},
		{name: "import failure restores SQLite", importFailure: true},
		{name: "health failure retains PostgreSQL fence", healthFailure: true, wantPostgres: true},
		{name: "alias failure retains PostgreSQL fence", aliasFailure: true, wantPostgres: true},
		{name: "remaining child prevents copy and restart", childRemains: true},
		{name: "checkpointed WAL source preserves committed data", checkpointedWALSource: true, wantPostgres: true},
		{name: "empty WAL metadata change preserves committed data", checkpointedWALSource: true, existingEmptyWALSource: true, wantPostgres: true},
		{name: "changed WAL bytes refuse a sealed copy", sourceWALMutation: true},
		{name: "inactive service clears its control group", emptyControlGroupAfterStop: true, wantPostgres: true},
		{name: "empty control group does not hide a remaining child", childRemains: true, emptyControlGroupAfterStop: true},
		{name: "marker created while waiting for lock", lockConflict: "marker"},
		{name: "backup destination created while waiting for lock", lockConflict: "backup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cutoverFixture(t, test)
		})
	}
}

type cutoverCase struct {
	name                       string
	importFailure              bool
	healthFailure              bool
	aliasFailure               bool
	childRemains               bool
	checkpointedWALSource      bool
	existingEmptyWALSource     bool
	sourceWALMutation          bool
	emptyControlGroupAfterStop bool
	lockConflict               string
	relativeConfigPaths        bool
	wantPostgres               bool
}

func cutoverFixture(t *testing.T, test cutoverCase) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"bin", "config", "run/lock", "var/lib/detent-hub", "opt/detent-hub/releases", "source-state", "source-repo",
		"tenant-root/org_example", "sys/fs/cgroup/system.slice/detent-cloud.service", "proc/111", "proc/112", "proc/222",
	} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	serviceUser := currentUser
	serviceGroupID := currentUser.Gid
	realSetpriv, setprivErr := exec.LookPath("setpriv")
	realPrivdrop := os.Geteuid() == 0 && setprivErr == nil
	if realPrivdrop {
		serviceUser, err = user.Lookup("nobody")
		if err != nil {
			t.Skip("a separate unprivileged service identity is unavailable")
		}
		serviceGroupID = serviceUser.Gid
	}
	currentGroup, err := user.LookupGroupId(serviceGroupID)
	if err != nil {
		t.Fatal(err)
	}
	cloudEnv := filepath.Join(root, "cloud.env")
	attachmentsEnv := filepath.Join(root, "attachments.env")
	entryConfig := filepath.Join(root, "config/cloud-postgres.yaml")
	cloudConfig := filepath.Join(root, "config/cloud.yaml")
	credentials := map[string][]byte{
		cloudEnv:       []byte("DETENT_CLOUD_ENV_PRESENT=1\n"),
		attachmentsEnv: []byte("DETENT_ATTACHMENTS_ACCESS_KEY_ID=cutover-test-key\n"),
		entryConfig:    []byte("backend: postgres\n"),
		cloudConfig:    []byte("backend: sqlite\n"),
	}
	for path, contents := range credentials {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if realPrivdrop {
		serviceGID, err := strconv.Atoi(serviceGroupID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(root, 0, serviceGID); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0770); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"opt", "opt/detent-hub", "opt/detent-hub/releases"} {
			if err := os.Chmod(filepath.Join(root, name), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	registryPath := filepath.Join(root, "source-state/registry.db")
	startRegistryWriter(t, registryPath)
	for _, path := range []string{
		filepath.Join(root, "tenant-root/org_example/hub.db.metrics.sqlite"),
	} {
		createSQLiteFile(t, path)
	}
	tenantDatabase := filepath.Join(root, "tenant-root/org_example/hub.db")
	if test.checkpointedWALSource || test.existingEmptyWALSource {
		createCheckpointedWALSQLiteFile(t, tenantDatabase)
		if _, err := os.Lstat(tenantDatabase + "-wal"); !os.IsNotExist(err) {
			t.Fatalf("checkpointed tenant source WAL sidecar exists before cutover: %v", err)
		}
		if test.existingEmptyWALSource {
			if err := os.WriteFile(tenantDatabase+"-wal", nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		createSQLiteFile(t, tenantDatabase)
	}
	createSQLiteFile(t, filepath.Join(root, "source-state/auth.db"))

	script, err := os.ReadFile("cutover-postgres.sh")
	if err != nil {
		t.Fatal(err)
	}
	if test.sourceWALMutation {
		destination := filepath.Join(root, "sealed-mutated-wal.db")
		if err := runSealedCopy(t, string(script), registryPath, destination, true); err == nil {
			t.Fatal("sealed copy accepted changed WAL bytes")
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatalf("changed-WAL destination exists after refusal: %v", err)
		}
		return
	}
	if test.checkpointedWALSource || test.existingEmptyWALSource {
		directCopy := filepath.Join(root, "sealed-checkpointed-wal.db")
		if err := runSealedCopy(t, string(script), tenantDatabase, directCopy, false); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(directCopy); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("sealed tenant copy file = %v, %v", info, err)
		}
		verify := exec.Command("python3", "-c", `import pathlib,sqlite3,sys; db=sqlite3.connect(pathlib.Path(sys.argv[1]).resolve().as_uri()+"?mode=ro&immutable=1",uri=True); assert db.execute("SELECT value FROM fixture").fetchone()[0]=='committed-through-wal'`, directCopy)
		if result, err := verify.CombinedOutput(); err != nil {
			t.Fatalf("sealed tenant copy omitted committed WAL data: %v: %s", err, result)
		}
		sourceWAL, err := os.Stat(tenantDatabase + "-wal")
		if err != nil || sourceWAL.Size() != 0 {
			t.Fatalf("read-only source WAL sidecar = %v, %v; want newly created empty sidecar", sourceWAL, err)
		}
	}
	isolated := strings.NewReplacer(
		"/opt/detent-hub", filepath.ToSlash(filepath.Join(root, "opt/detent-hub")),
		"/var/lib/detent-hub", filepath.ToSlash(filepath.Join(root, "var/lib/detent-hub")),
		"/usr/bin/setpriv", filepath.ToSlash(filepath.Join(root, "bin/setpriv")),
		"/run/lock/detent-hub-deploy.lock", filepath.ToSlash(filepath.Join(root, "run/lock/detent-hub-deploy.lock")),
		"/sys/fs/cgroup", filepath.ToSlash(filepath.Join(root, "sys/fs/cgroup")),
		"/proc/", filepath.ToSlash(filepath.Join(root, "proc"))+"/",
		"SERVICE_USER=detent-hub", "SERVICE_USER="+serviceUser.Username,
		"SERVICE_GROUP=detent-hub", "SERVICE_GROUP="+currentGroup.Name,
		"os.chown(temporary, 0, group)", "os.chown(temporary, os.getuid(), group)",
	).Replace(string(script))
	fixtureSource := filepath.Join(root, "source-repo")
	if err := os.MkdirAll(filepath.Join(fixtureSource, "deploy/hub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureSource, "README.md"), []byte("frozen source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtureSource, "deploy/hub/cutover-postgres.sh"), []byte(isolated), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", fixtureSource).CombinedOutput(); err != nil {
		t.Fatalf("git init source = %v: %s", err, output)
	}
	runSourceGit := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", fixtureSource}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v = %v: %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runSourceGit("config", "user.name", "Example Operator")
	runSourceGit("config", "user.email", "operator@example.test")
	runSourceGit("add", "README.md")
	runSourceGit("commit", "-qm", "frozen source base")
	runSourceGit("add", "deploy/hub/cutover-postgres.sh")
	runSourceGit("commit", "-qm", "frozen cutover source")
	commit := runSourceGit("rev-parse", "HEAD")
	candidate := filepath.Join(root, "opt/detent-hub/releases/detent-candidate")
	previous := filepath.Join(root, "opt/detent-hub/releases/detent-previous")
	currentLink := filepath.Join(root, "opt/detent-hub/detent")
	if err := os.MkdirAll(filepath.Dir(currentLink), 0700); err != nil {
		t.Fatal(err)
	}
	binary := fmt.Sprintf(`#!/bin/bash
set -eu
if [ "$FIXTURE_CHECK_SERVICE_IDENTITY" = true ]; then
  [ "$(id -un)" = "$FIXTURE_SERVICE_USER" ]
  [ "$DETENT_CLOUD_ENV_PRESENT" = 1 ]
  [ "$DETENT_ATTACHMENTS_ACCESS_KEY_ID" = cutover-test-key ]
fi
if [ "$1" = version ]; then
  printf '%%s\n' '{"commit":"%s","postgres_storage_supported":true,"socket_activation_supported":true}'
elif [ "$1" = hub ] && [ "$2" = import-core-postgres ] && [ "${3:-}" = --help ]; then
  exit 0
elif [ "$1" = hub ] && [ "$2" = import-core-postgres ]; then
  printf 'tenant\n' >> "$FIXTURE_ROOT/imports.log"
  if [ "$FIXTURE_IMPORT_FAILURE" = true ]; then exit 1; fi
  printf '%%s\n' '{"core_import_complete":true}'
else
  exit 2
fi
`, commit)
	for path, contents := range map[string][]byte{
		candidate: []byte(binary),
		previous:  []byte("#!/bin/bash\nexit 0\n"),
	} {
		mode := os.FileMode(0700)
		if realPrivdrop {
			mode = 0755
		}
		if err := os.WriteFile(path, contents, mode); err != nil {
			t.Fatal(err)
		}
	}
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		t.Fatal(err)
	}
	previous, err = filepath.EvalSymlinks(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previous, currentLink); err != nil {
		t.Fatal(err)
	}
	for pid, executable := range map[string]string{"111": previous, "112": previous, "222": candidate} {
		if err := os.Symlink(executable, filepath.Join(root, "proc", pid, "exe")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "proc", pid, "stat"), processStat(pid, "1000"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	processes := "111\n"
	if test.childRemains {
		processes += "112\n"
	}
	if err := os.WriteFile(filepath.Join(root, "sys/fs/cgroup/system.slice/detent-cloud.service/cgroup.procs"), []byte(processes), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service-active"), []byte("true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "socket-active"), []byte("true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main-pid"), []byte("111\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var healthRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			http.Error(writer, "unexpected authorization header", http.StatusBadRequest)
			return
		}
		count := healthRequests.Add(1)
		status := "ok"
		factsComplete := true
		migrationsRunning := false
		if test.healthFailure && count > 1 {
			status = "starting"
			factsComplete = false
			migrationsRunning = true
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"commit":%q,"status":%q,"schema_facts_complete":%t,"migrations_running":%t}`, commit, status, factsComplete, migrationsRunning)
	}))
	t.Cleanup(server.Close)

	state := filepath.Join(root, "var/lib/detent-hub")
	serviceUnit := `#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$FIXTURE_ROOT/systemctl.log"
unit=$1
shift
case "$unit" in
  show)
    [ "$1" = -p ]
    property=$2
    case "$property" in
      ActiveState)
        statefile=service-active
        case "${!#}" in detent-cloud.socket) statefile=socket-active ;; esac
        if [ "$(cat "$FIXTURE_ROOT/$statefile")" = true ]; then echo active; else echo inactive; fi
        ;;
      MainPID) cat "$FIXTURE_ROOT/main-pid" ;;
      ControlGroup)
        if [ "$(cat "$FIXTURE_ROOT/service-active")" = false ] && [ "$FIXTURE_EMPTY_CONTROL_GROUP_AFTER_STOP" = true ]; then exit 0; fi
        echo /system.slice/detent-cloud.service
        ;;
      KillMode) echo control-group ;;
      EnvironmentFiles) printf '%s (ignore_errors=no) %s (ignore_errors=no)\n' "$FIXTURE_CLOUD_ENV" "$FIXTURE_ATTACHMENTS_ENV" ;;
      *) exit 2 ;;
    esac
    ;;
  stop)
    if [ "$1" = detent-cloud.socket ]; then echo false > "$FIXTURE_ROOT/socket-active"; fi
    if [ "$1" = detent-cloud ]; then
      echo false > "$FIXTURE_ROOT/service-active"
      echo 0 > "$FIXTURE_ROOT/main-pid"
      if [ "$FIXTURE_CHILD_REMAINS" = true ]; then
        echo 112 > "$FIXTURE_ROOT/sys/fs/cgroup/system.slice/detent-cloud.service/cgroup.procs"
      else
        : > "$FIXTURE_ROOT/sys/fs/cgroup/system.slice/detent-cloud.service/cgroup.procs"
      fi
      rm -rf "$FIXTURE_ROOT/proc/111"
    fi
    ;;
  start)
    if [ "$1" = detent-cloud.socket ]; then echo true > "$FIXTURE_ROOT/socket-active"; fi
    if [ "$1" = detent-cloud ]; then
      echo true > "$FIXTURE_ROOT/service-active"
      echo 222 > "$FIXTURE_ROOT/main-pid"
      echo 222 > "$FIXTURE_ROOT/sys/fs/cgroup/system.slice/detent-cloud.service/cgroup.procs"
      target=$(readlink -f "$FIXTURE_CURRENT_LINK")
      ln -sf "$target" "$FIXTURE_ROOT/proc/222/exe"
    fi
    ;;
  is-active)
    unit=${!#}
    statefile=service-active
    [ "$unit" != detent-cloud.socket ] || statefile=socket-active
    [ "$(cat "$FIXTURE_ROOT/$statefile")" = true ]
    ;;
  *) exit 2 ;;
esac
`
	stubs := map[string]string{
		"sudo": `#!/bin/bash
set -eu
shift
if [ "${1:-}" = -u ]; then shift 2; fi
case "${1:-}" in
  install)
    shift
    args=()
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -o|-g) shift 2 ;;
        *) args+=("$1"); shift ;;
      esac
    done
    exec /usr/bin/install "${args[@]}"
    ;;
  *) exec "$@" ;;
esac
`,
		"flock": `#!/bin/bash
set -eu
case "$FIXTURE_LOCK_CONFLICT" in
  marker) : > "$FIXTURE_STATE/POSTGRES_CUTOVER"; exit 0 ;;
  backup) mkdir "$FIXTURE_BACKUP_ROOT"; exit 0 ;;
esac
exec "$FIXTURE_REAL_FLOCK" "$@"
`,
		"systemctl": serviceUnit,
		"mv": `#!/bin/bash
set -eu
if [ "$FIXTURE_ALIAS_FAILURE" = true ] && [ "${!#}" = "$FIXTURE_ENTRY_ARGUMENT" ]; then exit 1; fi
exec "$FIXTURE_REAL_MV" "$@"
`,
		"seq": "#!/bin/bash\necho 1\n",
		"getent": `#!/bin/bash
set -eu
[ "$1" = passwd ] || exit 2
printf '%s:x:%s:%s::%s:/bin/bash\n' "$FIXTURE_SERVICE_USER" "$FIXTURE_SERVICE_UID" "$FIXTURE_SERVICE_GID" "$FIXTURE_SERVICE_HOME"
`,
		"setpriv": `#!/bin/bash
set -eu
if [ "$FIXTURE_REAL_PRIVDROP" = true ]; then exec "$FIXTURE_REAL_SETPRIV" "$@"; fi
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
[ "$1" = -- ] || exit 2
shift
exec "$@"
`,
	}
	binDir := filepath.Join(root, "bin")
	for name, contents := range stubs {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	scriptPath := filepath.Join(root, "cutover-postgres.sh")
	if err := os.WriteFile(scriptPath, []byte(isolated), 0700); err != nil {
		t.Fatal(err)
	}
	backupRoot := filepath.Join(root, "sealed")
	realFlock, err := exec.LookPath("flock")
	if err != nil {
		t.Fatal(err)
	}
	entryArgument, cloudArgument := entryConfig, cloudConfig
	if test.relativeConfigPaths {
		entryArgument = filepath.Join("config", filepath.Base(entryConfig))
		cloudArgument = filepath.Join("config", filepath.Base(cloudConfig))
	}
	command := exec.CommandContext(t.Context(), "bash", scriptPath,
		"--binary", candidate,
		"--entry-config", entryArgument,
		"--cloud-config", cloudArgument,
		"--repository-root", filepath.Join(root, "source-repo"),
		"--cloud-env", cloudEnv,
		"--attachments-env", attachmentsEnv,
		"--source-state", filepath.Join(root, "source-state"),
		"--tenant-root", filepath.Join(root, "tenant-root"),
		"--backup-root", backupRoot,
		"--health-url", server.URL,
	)
	command.Dir = root
	realMove, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+root,
		"FIXTURE_ROOT="+root,
		"FIXTURE_CLOUD_ENV="+cloudEnv,
		"FIXTURE_ATTACHMENTS_ENV="+attachmentsEnv,
		"FIXTURE_CURRENT_LINK="+currentLink,
		"FIXTURE_STATE="+state,
		"FIXTURE_BACKUP_ROOT="+backupRoot,
		"FIXTURE_REAL_FLOCK="+realFlock,
		"FIXTURE_REAL_MV="+realMove,
		"FIXTURE_ENTRY_ARGUMENT="+entryArgument,
		fmt.Sprintf("FIXTURE_ALIAS_FAILURE=%t", test.aliasFailure),
		fmt.Sprintf("FIXTURE_IMPORT_FAILURE=%t", test.importFailure),
		fmt.Sprintf("FIXTURE_HEALTH_FAILURE=%t", test.healthFailure),
		fmt.Sprintf("FIXTURE_CHILD_REMAINS=%t", test.childRemains),
		fmt.Sprintf("FIXTURE_EMPTY_CONTROL_GROUP_AFTER_STOP=%t", test.emptyControlGroupAfterStop),
		"FIXTURE_LOCK_CONFLICT="+test.lockConflict,
		"FIXTURE_SERVICE_USER="+serviceUser.Username,
		"FIXTURE_SERVICE_UID="+serviceUser.Uid,
		"FIXTURE_SERVICE_GID="+serviceGroupID,
		"FIXTURE_SERVICE_HOME="+serviceUser.HomeDir,
		"FIXTURE_CHECK_SERVICE_IDENTITY=true",
		fmt.Sprintf("FIXTURE_REAL_PRIVDROP=%t", realPrivdrop),
		"FIXTURE_REAL_SETPRIV="+realSetpriv,
	)
	output, runErr := command.CombinedOutput()
	if test.wantPostgres {
		if test.healthFailure || test.aliasFailure {
			if runErr == nil || !strings.Contains(string(output), "PostgreSQL startup was attempted") {
				t.Fatalf("cutover = %v, want fenced health failure: %s", runErr, output)
			}
		} else if runErr != nil {
			t.Fatalf("cutover failed: %v: %s", runErr, output)
		}
		selected, err := os.Readlink(currentLink)
		if err != nil || selected != candidate {
			t.Fatalf("selected release = %q, want %q (%v)", selected, candidate, err)
		}
		config, err := os.ReadFile(cloudConfig)
		if err != nil || !bytes.Equal(config, credentials[entryConfig]) {
			t.Fatalf("active config = %q, want PostgreSQL config (%v)", config, err)
		}
		if _, err := os.Stat(filepath.Join(state, "POSTGRES_CUTOVER")); err != nil {
			t.Fatalf("cutover marker missing: %v", err)
		}
		if test.healthFailure || test.aliasFailure {
			for _, filename := range []string{"service-active", "socket-active"} {
				value, err := os.ReadFile(filepath.Join(root, filename))
				if err != nil || string(value) != "false\n" {
					t.Fatalf("%s = %q after health failure, want fenced (%v)", filename, value, err)
				}
			}
		} else {
			calls, err := os.ReadFile(filepath.Join(root, "systemctl.log"))
			if err != nil {
				t.Fatal(err)
			}
			callLog := string(calls)
			stopSocket := strings.Index(callLog, "\nstop detent-cloud.socket\n")
			stopService := strings.Index(callLog, "\nstop detent-cloud\n")
			startSocket := strings.Index(callLog, "\nstart detent-cloud.socket\n")
			startService := strings.Index(callLog, "\nstart detent-cloud\n")
			if stopSocket < 0 || stopService < stopSocket || startSocket < stopService || startService < startSocket {
				t.Fatalf("Cloud fencing and activation order = %s", callLog)
			}
			activePath, err := filepath.EvalSymlinks(cloudConfig)
			if err != nil {
				t.Fatal(err)
			}
			aliasPath, err := filepath.EvalSymlinks(entryConfig)
			if err != nil || aliasPath != activePath {
				t.Fatalf("entry config resolves to %q, want authoritative %q (%v)", aliasPath, activePath, err)
			}
			preserved, err := os.ReadFile(filepath.Join(backupRoot, "cloud.postgres.yaml"))
			if err != nil || !bytes.Equal(preserved, credentials[entryConfig]) {
				t.Fatalf("staged PostgreSQL configuration was not preserved (%v)", err)
			}
			updated := []byte("backend: postgres\noperator_change: retained\n")
			if err := os.WriteFile(cloudConfig+".edit", updated, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(cloudConfig+".edit", cloudConfig); err != nil {
				t.Fatal(err)
			}
			aliased, err := os.ReadFile(entryConfig)
			if err != nil || !bytes.Equal(aliased, updated) {
				t.Fatalf("entry config missed authoritative edit (%v)", err)
			}
			imports, err := os.ReadFile(filepath.Join(root, "imports.log"))
			if err != nil || string(imports) != "tenant\n" {
				t.Fatalf("tenant import execution = %q (%v)", imports, err)
			}
			registryCopy := filepath.Join(backupRoot, "platform/registry.db")
			verify := exec.Command("python3", "-c", `import sqlite3,sys; db=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro&immutable=1",uri=True); assert db.execute("SELECT id FROM organizations WHERE id='org_example'").fetchone()[0]=='org_example'`, registryCopy)
			if result, err := verify.CombinedOutput(); err != nil {
				t.Fatalf("sealed registry omitted committed WAL row: %v: %s", err, result)
			}
			if test.checkpointedWALSource {
				tenantCopy := filepath.Join(backupRoot, "tenants/org_example/hub.db")
				verify = exec.Command("python3", "-c", `import sqlite3,sys; db=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro&immutable=1",uri=True); assert db.execute("SELECT value FROM fixture").fetchone()[0]=='committed-through-wal'`, tenantCopy)
				if result, err := verify.CombinedOutput(); err != nil {
					t.Fatalf("sealed tenant copy omitted committed WAL data: %v: %s", err, result)
				}
				sourceWAL, err := os.Stat(tenantDatabase + "-wal")
				if err != nil || sourceWAL.Size() != 0 {
					t.Fatalf("read-only source WAL sidecar = %v, %v; want newly created empty sidecar", sourceWAL, err)
				}
			}
			if _, err := os.Stat(filepath.Join(backupRoot, "tenants/org_example/hub.db.metrics.sqlite")); err != nil {
				t.Fatalf("tenant metrics source was not retained: %v", err)
			}
			if err := os.RemoveAll(filepath.Join(root, "proc/222")); err != nil {
				t.Fatal(err)
			}
		}
	} else if runErr == nil {
		t.Fatalf("cutover unexpectedly succeeded: %s", output)
	}
	if !test.wantPostgres {
		for _, path := range []string{cloudConfig, entryConfig} {
			contents, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(contents, credentials[path]) {
				t.Fatalf("pre-start failure changed migration configuration %s (%v)", filepath.Base(path), err)
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("pre-start failure replaced migration input %s (%v)", filepath.Base(path), err)
			}
		}
		selected, err := os.Readlink(currentLink)
		if err != nil || selected != previous {
			t.Fatalf("failed cutover selected %q, want previous %q (%v): %s", selected, previous, err, output)
		}
		for _, filename := range []string{"service-active", "socket-active"} {
			value, err := os.ReadFile(filepath.Join(root, filename))
			want := "true\n"
			if test.childRemains {
				want = "false\n"
			}
			if err != nil || string(value) != want {
				t.Fatalf("%s = %q, want %q (%v): %s", filename, value, want, err, output)
			}
		}
		if test.lockConflict == "marker" {
			if _, err := os.Stat(filepath.Join(state, "POSTGRES_CUTOVER")); err != nil {
				t.Fatalf("concurrent cutover marker disappeared: %v", err)
			}
		} else {
			if _, err := os.Stat(filepath.Join(state, "POSTGRES_CUTOVER")); !os.IsNotExist(err) {
				t.Fatalf("pre-start failure retained the cutover marker: %v", err)
			}
		}
		if test.lockConflict != "" {
			if _, err := os.Stat(filepath.Join(root, "imports.log")); !os.IsNotExist(err) {
				t.Fatalf("imports started despite concurrent cutover state: %v", err)
			}
		}
		if test.childRemains {
			if _, err := os.Stat(filepath.Join(backupRoot, "platform/registry.db")); !os.IsNotExist(err) {
				t.Fatalf("copy started while a tenant child remained: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "imports.log")); !os.IsNotExist(err) {
				t.Fatalf("imports started while a tenant child remained: %v", err)
			}
		}
	}
}

func startRegistryWriter(t *testing.T, path string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	program := `import sqlite3,sys
db=sqlite3.connect(sys.argv[1])
db.execute("PRAGMA journal_mode=WAL")
db.execute("PRAGMA wal_autocheckpoint=0")
db.execute("PRAGMA application_id=0x44545247")
db.execute("CREATE TABLE organizations(id TEXT PRIMARY KEY,state TEXT NOT NULL,managed INTEGER NOT NULL)")
db.execute("INSERT INTO organizations VALUES('org_example','ready',1)")
db.commit()
print("ready",flush=True)
sys.stdin.readline()
db.close()
`
	command := exec.Command("python3", "-c", program, path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = stdin.Write([]byte("exit\n"))
		_ = stdin.Close()
		_ = command.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("registry writer readiness = %q, %v", line, err)
	}
	return command, stdin
}

func createSQLiteFile(t *testing.T, path string) {
	t.Helper()
	command := exec.Command("python3", "-c", `import sqlite3,sys; db=sqlite3.connect(sys.argv[1]); db.execute("CREATE TABLE fixture(value TEXT)"); db.execute("INSERT INTO fixture VALUES('ok')"); db.commit(); db.close()`, path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create SQLite fixture %s: %v: %s", path, err, output)
	}
}

func createCheckpointedWALSQLiteFile(t *testing.T, path string) {
	t.Helper()
	program := `import sqlite3,sys
db=sqlite3.connect(sys.argv[1])
db.execute("PRAGMA journal_mode=WAL")
db.execute("PRAGMA wal_autocheckpoint=0")
db.execute("CREATE TABLE fixture(value TEXT)")
db.execute("INSERT INTO fixture VALUES('committed-through-wal')")
db.commit()
db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
db.close()
`
	command := exec.Command("python3", "-c", program, path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create checkpointed WAL SQLite fixture %s: %v: %s", path, err, output)
	}
}

func runSealedCopy(t *testing.T, sourceScript, source, destination string, mutateWAL bool) error {
	t.Helper()
	start := strings.Index(sourceScript, "copy_sealed() {")
	if start < 0 {
		return fmt.Errorf("copy_sealed function is unavailable")
	}
	end := strings.Index(sourceScript[start:], "\n}\n\ncheck_copy_space() {")
	if end < 0 {
		return fmt.Errorf("copy_sealed function is unavailable")
	}
	copyFunction := sourceScript[start : start+end+2]
	if mutateWAL {
		mutation := `wal_path = pathlib.Path(str(source_path) + "-wal")
wal_info = wal_path.stat()
with wal_path.open("r+b") as sidecar:
    first = sidecar.read(1)
    if not first:
        raise SystemExit(1)
    sidecar.seek(0)
    sidecar.write(bytes([first[0] ^ 1]))
    sidecar.flush()
    os.fsync(sidecar.fileno())
os.utime(wal_path, ns=(wal_info.st_atime_ns, wal_info.st_mtime_ns))
`
		originalFunction := copyFunction
		copyFunction = strings.Replace(copyFunction, "reader.close()\nafter = os.lstat(source_path)", "reader.close()\n"+mutation+"after = os.lstat(source_path)", 1)
		if copyFunction == originalFunction {
			return fmt.Errorf("copy_sealed WAL validation point is unavailable")
		}
	}
	currentUser, err := user.Current()
	if err != nil {
		return err
	}
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		return err
	}
	program := `#!/bin/bash
set -Eeuo pipefail
SERVICE_USER=$FIXTURE_SERVICE_USER
SERVICE_GROUP=$FIXTURE_SERVICE_GROUP
sudo() { if [ "${1:-}" = -n ]; then shift; fi; "$@"; }
` + copyFunction + `
copy_sealed "$1" "$2"
`
	path := filepath.Join(t.TempDir(), "copy-sealed.sh")
	if err := os.WriteFile(path, []byte(program), 0700); err != nil {
		return err
	}
	command := exec.Command("bash", path, source, destination)
	command.Env = append(os.Environ(), "FIXTURE_SERVICE_USER="+currentUser.Username, "FIXTURE_SERVICE_GROUP="+currentGroup.Name)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy sealed SQLite fixture: %w: %s", err, output)
	}
	return nil
}

func processStat(pid, start string) []byte {
	fields := make([]string, 20)
	fields[0] = "S"
	for index := 1; index < len(fields)-1; index++ {
		fields[index] = "0"
	}
	fields[len(fields)-1] = start
	return []byte(fmt.Sprintf("%s (detent) %s\n", pid, strings.Join(fields, " ")))
}
