package hub

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionDeployInstallsLitestreamBeforeApplicationRestart(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("deploy-from-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile("litestream-production.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		missingConfig bool
		wrongBinary   bool
		restartFails  bool
	}{
		{name: "install and activate config"},
		{name: "missing config refuses deployment", missingConfig: true},
		{name: "wrong binary refuses config", wrongBinary: true},
		{name: "replication restart failure refuses deployment", restartFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range []string{"bin", "opt/detent-hub/releases", "etc/litestream-detent", "run/lock", "var/lib/detent-hub"} {
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
  restart) test "$FIXTURE_RESTART_FAILS" = false ;;
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
			var upload bytes.Buffer
			archive := tar.NewWriter(&upload)
			files := map[string]string{"detent": binary}
			if !test.missingConfig {
				files["litestream-production.json"] = string(config)
			}
			for name, data := range files {
				if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(archive, data); err != nil {
					t.Fatal(err)
				}
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			isolated := string(script)
			for _, path := range []string{"/opt/", "/etc/", "/run/lock/", "/var/lib/"} {
				isolated = strings.ReplaceAll(isolated, path, filepath.ToSlash(dir)+path)
			}
			command := exec.CommandContext(t.Context(), "bash", "-c", isolated)
			command.Stdin = &upload
			command.Env = []string{
				"PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin",
				"TMPDIR=" + dir,
				"SSH_ORIGINAL_COMMAND=deploy " + commit,
				"FIXTURE_ROOT=" + dir,
				"FIXTURE_RESTART_FAILS=" + map[bool]string{false: "false", true: "true"}[test.restartFails],
			}
			output, err := command.CombinedOutput()
			refused := test.missingConfig || test.wrongBinary || test.restartFails
			if (err != nil) != refused {
				t.Fatalf("deploy = %v, want refused %t: %s", err, refused, output)
			}
			installed, err := os.ReadFile(filepath.Join(dir, "etc/litestream-detent/production.yml"))
			if test.missingConfig || test.wrongBinary {
				if !os.IsNotExist(err) {
					t.Fatalf("unverified deployment installed config: %v", err)
				}
				return
			}
			if err != nil || !bytes.Equal(installed, config) {
				t.Fatalf("installed config differs from tracked config: %v", err)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "systemctl.log"))
			if err != nil {
				t.Fatal(err)
			}
			replication := strings.Index(string(calls), "restart litestream-detent@production.service\n")
			application := strings.Index(string(calls), "stop detent-cloud\n")
			if replication < 0 || (test.restartFails && application >= 0) || (!test.restartFails && application <= replication) {
				t.Fatalf("application restarted before replication was ready: %s", calls)
			}
			staged, err := filepath.Glob(filepath.Join(dir, "detent-hub-upload.*"))
			if err != nil || len(staged) != 0 {
				t.Fatalf("upload retained: %v %v", staged, err)
			}
		})
	}
}
