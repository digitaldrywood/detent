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
	"strings"
	"sync/atomic"
	"testing"
)

func TestPostgresCutoverFencesCopiesAndSelectsPostgresRelease(t *testing.T) {
	for _, test := range []cutoverCase{
		{name: "complete cutover", wantPostgres: true},
		{name: "import failure restores SQLite", importFailure: true},
		{name: "health failure retains PostgreSQL fence", healthFailure: true, wantPostgres: true},
		{name: "remaining child prevents copy and restart", childRemains: true},
		{name: "marker created while waiting for lock", lockConflict: "marker"},
		{name: "backup destination created while waiting for lock", lockConflict: "backup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cutoverFixture(t, test)
		})
	}
}

type cutoverCase struct {
	name          string
	importFailure bool
	healthFailure bool
	childRemains  bool
	lockConflict  string
	wantPostgres  bool
}

func cutoverFixture(t *testing.T, test cutoverCase) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"bin", "run/lock", "var/lib/detent-hub", "opt/detent-hub/releases", "source-state", "source-repo",
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
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		t.Fatal(err)
	}
	cloudEnv := filepath.Join(root, "cloud.env")
	attachmentsEnv := filepath.Join(root, "attachments.env")
	entryConfig := filepath.Join(root, "cloud-postgres.yaml")
	sqliteConfig := filepath.Join(root, "cloud.yaml")
	credentials := map[string][]byte{
		cloudEnv:       []byte("DETENT_CLOUD_ENV_PRESENT=1\n"),
		attachmentsEnv: []byte("DETENT_ATTACHMENTS_ACCESS_KEY_ID=cutover-test-key\n"),
		entryConfig:    []byte("backend: postgres\n"),
		sqliteConfig:   []byte("backend: sqlite\n"),
	}
	for path, contents := range credentials {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	registryPath := filepath.Join(root, "source-state/registry.db")
	startRegistryWriter(t, registryPath)
	for _, path := range []string{
		filepath.Join(root, "source-state/auth.db"),
		filepath.Join(root, "tenant-root/org_example/hub.db"),
		filepath.Join(root, "tenant-root/org_example/hub.db.metrics.sqlite"),
	} {
		createSQLiteFile(t, path)
	}

	commit := strings.Repeat("a", 40)
	candidate := filepath.Join(root, "opt/detent-hub/releases/detent-candidate")
	previous := filepath.Join(root, "opt/detent-hub/releases/detent-previous")
	currentLink := filepath.Join(root, "opt/detent-hub/detent")
	if err := os.MkdirAll(filepath.Dir(currentLink), 0700); err != nil {
		t.Fatal(err)
	}
	binary := fmt.Sprintf(`#!/bin/bash
set -eu
if [ "$1" = version ]; then
  printf '%%s\n' '{"commit":"%s","postgres_storage_supported":true,"socket_activation_supported":true}'
elif [ "$1" = cloud ] && [ "$2" = import-platform-postgres ] && [ "${3:-}" = --help ]; then
  exit 0
elif [ "$1" = hub ] && [ "$2" = import-core-postgres ] && [ "${3:-}" = --help ]; then
  exit 0
elif [ "$1" = cloud ] && [ "$2" = import-platform-postgres ]; then
  printf 'platform\n' >> "$FIXTURE_ROOT/imports.log"
  if [ "$FIXTURE_IMPORT_FAILURE" = true ]; then exit 1; fi
  printf '%%s\n' '{"done":true,"processed":2}'
elif [ "$1" = hub ] && [ "$2" = import-core-postgres ]; then
  printf 'tenant\n' >> "$FIXTURE_ROOT/imports.log"
  printf '%%s\n' '{"core_import_complete":true}'
else
  exit 2
fi
`, commit)
	for path, contents := range map[string][]byte{
		candidate: []byte(binary),
		previous:  []byte("#!/bin/bash\nexit 0\n"),
	} {
		if err := os.WriteFile(path, contents, 0700); err != nil {
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
      ControlGroup) echo /system.slice/detent-cloud.service ;;
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
		"seq":       "#!/bin/bash\necho 1\n",
	}
	binDir := filepath.Join(root, "bin")
	for name, contents := range stubs {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("cutover-postgres.sh")
	if err != nil {
		t.Fatal(err)
	}
	isolated := strings.NewReplacer(
		"/opt/detent-hub", filepath.ToSlash(filepath.Join(root, "opt/detent-hub")),
		"/var/lib/detent-hub", filepath.ToSlash(state),
		"/run/lock/detent-hub-deploy.lock", filepath.ToSlash(filepath.Join(root, "run/lock/detent-hub-deploy.lock")),
		"/sys/fs/cgroup", filepath.ToSlash(filepath.Join(root, "sys/fs/cgroup")),
		"/proc/", filepath.ToSlash(filepath.Join(root, "proc"))+"/",
		"SERVICE_USER=detent-hub", "SERVICE_USER="+currentUser.Username,
		"SERVICE_GROUP=detent-hub", "SERVICE_GROUP="+currentGroup.Name,
		"os.chown(temporary, 0, group)", "os.chown(temporary, os.getuid(), group)",
	).Replace(string(script))
	scriptPath := filepath.Join(root, "cutover-postgres.sh")
	if err := os.WriteFile(scriptPath, []byte(isolated), 0700); err != nil {
		t.Fatal(err)
	}
	backupRoot := filepath.Join(root, "sealed")
	realFlock, err := exec.LookPath("flock")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bash", scriptPath,
		"--binary", candidate,
		"--entry-config", entryConfig,
		"--sqlite-config", sqliteConfig,
		"--repository-root", filepath.Join(root, "source-repo"),
		"--cloud-env", cloudEnv,
		"--attachments-env", attachmentsEnv,
		"--source-state", filepath.Join(root, "source-state"),
		"--tenant-root", filepath.Join(root, "tenant-root"),
		"--backup-root", backupRoot,
		"--health-url", server.URL,
	)
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
		fmt.Sprintf("FIXTURE_IMPORT_FAILURE=%t", test.importFailure),
		fmt.Sprintf("FIXTURE_HEALTH_FAILURE=%t", test.healthFailure),
		fmt.Sprintf("FIXTURE_CHILD_REMAINS=%t", test.childRemains),
		"FIXTURE_LOCK_CONFLICT="+test.lockConflict,
	)
	output, runErr := command.CombinedOutput()
	if test.wantPostgres {
		if test.healthFailure {
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
		config, err := os.ReadFile(sqliteConfig)
		if err != nil || !bytes.Equal(config, credentials[entryConfig]) {
			t.Fatalf("active config = %q, want PostgreSQL config (%v)", config, err)
		}
		if _, err := os.Stat(filepath.Join(state, "POSTGRES_CUTOVER")); err != nil {
			t.Fatalf("cutover marker missing: %v", err)
		}
		if test.healthFailure {
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
			imports, err := os.ReadFile(filepath.Join(root, "imports.log"))
			if err != nil || string(imports) != "platform\ntenant\n" {
				t.Fatalf("platform and tenant import order = %q (%v)", imports, err)
			}
			registryCopy := filepath.Join(backupRoot, "platform/registry.db")
			verify := exec.Command("python3", "-c", `import sqlite3,sys; db=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro&immutable=1",uri=True); assert db.execute("SELECT id FROM organizations WHERE id='org_example'").fetchone()[0]=='org_example'`, registryCopy)
			if result, err := verify.CombinedOutput(); err != nil {
				t.Fatalf("sealed registry omitted committed WAL row: %v: %s", err, result)
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

func processStat(pid, start string) []byte {
	fields := make([]string, 20)
	fields[0] = "S"
	for index := 1; index < len(fields)-1; index++ {
		fields[index] = "0"
	}
	fields[len(fields)-1] = start
	return []byte(fmt.Sprintf("%s (detent) %s\n", pid, strings.Join(fields, " ")))
}
