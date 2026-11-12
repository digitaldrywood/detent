package workspace

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitFixtureEnvironmentIsolation(t *testing.T) {
	if os.Getenv("DETENT_TEST_GIT_FIXTURE_CHILD") == "1" {
		fixture := initSourceRepo(t)
		common := strings.TrimSpace(runGit(t, fixture, "rev-parse", "--path-format=absolute", "--git-common-dir"))
		if got, want := mustCanonicalExistingPath(t, common), mustCanonicalExistingPath(t, filepath.Join(fixture, ".git")); got != want {
			t.Fatalf("fixture common directory = %q, want %q", got, want)
		}
		if got := strings.TrimSpace(runGit(t, fixture, "log", "-1", "--format=%an <%ae>")); got != "Test User <test@example.com>" {
			t.Fatalf("fixture author = %q", got)
		}
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		variables []string
	}{
		{name: "linked worktree cwd"},
		{name: "linked git directory", variables: []string{"GIT_DIR"}},
		{name: "common directory", variables: []string{"GIT_COMMON_DIR"}},
		{name: "config file", variables: []string{"GIT_CONFIG"}},
		{name: "worktree and index", variables: []string{"GIT_WORK_TREE", "GIT_INDEX_FILE"}},
		{name: "linked worktree hook", variables: []string{"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"}},
		{name: "config parameters", variables: []string{"GIT_CONFIG_PARAMETERS"}},
		{name: "config entries", variables: []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"}},
		{name: "author identity", variables: []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := initSourceRepo(t)
			runGit(t, source, "config", "user.name", "Source Owner")
			runGit(t, source, "config", "user.email", "source@example.com")
			linked := filepath.Join(t.TempDir(), "linked")
			runGit(t, source, "worktree", "add", "-b", "linked", linked)
			common := filepath.Join(source, ".git")
			configPath := filepath.Join(common, "config")
			values := map[string]string{
				"GIT_DIR":               linkedWorktreeGitDir(t, linked),
				"GIT_COMMON_DIR":        common,
				"GIT_CONFIG":            configPath,
				"GIT_WORK_TREE":         source,
				"GIT_INDEX_FILE":        filepath.Join(common, "index"),
				"GIT_CONFIG_PARAMETERS": "'core.bare=true'",
				"GIT_CONFIG_COUNT":      "1",
				"GIT_CONFIG_KEY_0":      "core.bare",
				"GIT_CONFIG_VALUE_0":    "true",
				"GIT_AUTHOR_NAME":       "Inherited Author",
				"GIT_AUTHOR_EMAIL":      "inherited@example.com",
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestGitFixtureEnvironmentIsolation$")
			cmd.Dir = linked
			cmd.Env = append(os.Environ(), "DETENT_TEST_GIT_FIXTURE_CHILD=1")
			for _, key := range tt.variables {
				cmd.Env = append(cmd.Env, key+"="+values[key])
			}
			output, runErr := cmd.CombinedOutput()
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("fixture setup changed source/common repository configuration")
			}
			if runErr != nil {
				t.Fatalf("fixture child: %v\n%s", runErr, output)
			}
		})
	}
}
