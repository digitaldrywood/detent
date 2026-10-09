package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseDeployBindsBinaryAndRestrictedSSH(t *testing.T) {
	for _, test := range []struct {
		name, environment, version string
		wrongBinary, sshFailure    bool
		refused                    bool
	}{
		{name: "staging", environment: "staging", version: "v1.2.4"},
		{name: "production", environment: "production", version: "v1.2.4"},
		{name: "operator staging", environment: "staging", version: "v1.2.4-op.aaaaaaaaaaaa"},
		{name: "operator production", environment: "production", version: "v1.2.4-op.aaaaaaaaaaaa"},
		{name: "operator wrong source", environment: "production", version: "v1.2.4-op.bbbbbbbbbbbb", refused: true},
		{name: "unsigned operator version", environment: "production", version: "operator-landed-aaaaaaaaaaaa", refused: true},
		{name: "wrong binary", environment: "production", version: "v1.2.4", wrongBinary: true},
		{name: "invalid tag", environment: "production", version: "v1.2.4;bad", refused: true},
		{name: "SSH failure cleans key", environment: "production", version: "v1.2.4", sshFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "release-hub"), 0700); err != nil {
				t.Fatal(err)
			}
			commit := strings.Repeat("a", 40)
			binaryVersion := strings.TrimPrefix(test.version, "v")
			if test.wrongBinary {
				binaryVersion = "1.2.5"
			}
			binary := "#!/usr/bin/env bash\nprintf '%s\\n' '{\"version\":\"" + binaryVersion + "\",\"commit\":\"" + commit + "\"}'\n"
			if err := os.WriteFile(filepath.Join(dir, "release-hub", "detent"), []byte(binary), 0700); err != nil {
				t.Fatal(err)
			}
			ssh := "#!/usr/bin/env bash\nset -euo pipefail\nprintf '%s\\n' \"$*\" > \"$FIXTURE_LOG\"\ncat > \"$FIXTURE_LOG.binary\"\ntest \"$FIXTURE_SSH_FAILURE\" = false\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(ssh), 0700); err != nil {
				t.Fatal(err)
			}
			root, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", "scripts/deploy-release.sh", test.environment, commit, test.version)
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+dir, "SSH_KEY=fixture-key", "KNOWN_HOSTS=fixture-host-key", "FIXTURE_LOG="+filepath.Join(dir, "calls"), "FIXTURE_SSH_FAILURE="+map[bool]string{true: "true", false: "false"}[test.sshFailure])
			output, err := command.CombinedOutput()
			if (err != nil) != (test.wrongBinary || test.sshFailure || test.refused) {
				t.Fatalf("deploy=%v %s", err, output)
			}
			calls, readErr := os.ReadFile(filepath.Join(dir, "calls"))
			if test.wrongBinary || test.refused {
				if !os.IsNotExist(readErr) {
					t.Fatal("unverified binary reached SSH")
				}
			} else {
				prefix := ""
				if test.environment == "staging" {
					prefix = "staging."
				}
				for _, required := range []string{"StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "HostKeyAlias=" + prefix + "hub.detent.build", "apprunner@" + prefix + "cloud.detent.build deploy " + commit} {
					if !strings.Contains(string(calls), required) {
						t.Fatalf("SSH lost restriction %q: %s", required, calls)
					}
				}
				streamed, err := os.ReadFile(filepath.Join(dir, "calls.binary"))
				if err != nil || string(streamed) != binary {
					t.Fatal("deployment streamed a different binary")
				}
			}
			keys, err := filepath.Glob(filepath.Join(dir, "release-ssh.*"))
			if err != nil || len(keys) != 0 {
				t.Fatalf("SSH material retained: %v %v", keys, err)
			}
		})
	}
}
