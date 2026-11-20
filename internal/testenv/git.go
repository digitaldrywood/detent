package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func ClearGitEnvironment() error {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			if err := os.Unsetenv(key); err != nil {
				return fmt.Errorf("clear %s: %w", key, err)
			}
		}
	}
	return nil
}

func UseGitHubFixtureRemote(t testing.TB, checkout, fixture, repository string) {
	t.Helper()
	script := `case "$3" in
		git-upload-pack\ *) exec git upload-pack "$1";;
		git-receive-pack\ *) exec git receive-pack "$1";;
		*) exit 1;;
	esac`
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", checkout}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("configure fixture transport: %v: %s", err, output)
		}
	}
	git("config", "core.sshCommand", "sh -c "+quote(script)+" -- "+quote(fixture))
	git("config", "ssh.variant", "simple")
	git("remote", "set-url", "origin", "git@github.com:"+strings.TrimPrefix(repository, "https://github.com/")+".git")
}
