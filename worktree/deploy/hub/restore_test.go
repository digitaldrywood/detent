package hub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
			for _, dir := range []string{"bin", "etc/systemd/system/detent-cloud.service.d", "opt/releases", "state", "lock", "release-hub"} {
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
			write("release-hub/detent", "#!/bin/sh\nprintf '%s\\n' '{\"version\":\"1.2.3\",\"commit\":\""+reported+"\",\"socket_activation_supported\":true}'\n"+strings.Repeat("\n", 1000000))
			write("etc/systemd/system/detent-cloud.socket", "[Unit]\nDescription=Detent detent-cloud listener\n\n[Socket]\nListenStream=127.0.0.1:8017\nAccept=no\nBacklog=65535\n\n[Install]\nWantedBy=sockets.target\n")
			write("etc/systemd/system/detent-cloud.service.d/socket.conf", "[Unit]\nRequires=detent-cloud.socket\nAfter=detent-cloud.socket\n\n[Service]\nSockets=detent-cloud.socket\n")
			rewritten := strings.NewReplacer(
				"OPT=/opt/$NAME", "OPT=\"$WORK/opt\"",
				"STATE=/var/lib/$NAME", "STATE=\"$WORK/state\"",
				"/run/lock/$NAME-deploy.lock", "\"$WORK/lock/deploy.lock\"",
				"/etc/", "$WORK/etc/",
				"/tmp/$NAME-", "$WORK/temporary-",
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
			write("bin/curl", `#!/bin/sh
printf '{"status":"ok","commit":"%s","schema_version":1,"schema_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","registry_schema_version":1,"registry_schema_fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","auth_schema_version":1,"auth_schema_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","platform_schema_version":1,"platform_schema_fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","hub_schema_applicable":true,"registry_schema_applicable":true,"auth_schema_applicable":true,"platform_schema_applicable":true,"schema_facts_complete":true,"migrations_running":false}' "$FIXTURE_COMMIT"
`)
			write("bin/systemctl", "#!/bin/sh\nif [ \"$1\" = show ]; then printf '42\\n'; fi\n")
			write("bin/stat", "#!/bin/sh\nwc -c < \"$3\"\n")
			write("bin/sudo", `#!/bin/bash
shift
if [ "$1" = -u ]; then shift 2; fi
if [ "$1" = install ]; then
  shift
  mode=$2
  shift 2
  while [ "$1" = -o ] || [ "$1" = -g ]; do shift 2; done
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
			cmd.Env = append(cmd.Env, "FIXTURE_COMMIT="+commit[:9])
			output, err := cmd.CombinedOutput()
			if (err != nil) != test.invalidIdentity {
				t.Fatalf("deploy error = %v, output = %s", err, output)
			}

		})
	}
}
