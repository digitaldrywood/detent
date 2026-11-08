package invariants

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sqliteFixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Example", "GIT_AUTHOR_EMAIL=example@example.org",
		"GIT_COMMITTER_NAME=Example", "GIT_COMMITTER_EMAIL=example@example.org",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, output)
	}
	return strings.TrimSpace(string(output))
}

func sqliteFixtureWrite(t *testing.T, root, name, data string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteIntegrationBaseline(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                                             string
		omitPolicy, fallback, unrelated, clean, noOrigin bool
	}{
		{name: "committed inflated policy rejected"},
		{name: "initial policy anchored to integration source", omitPolicy: true},
		{name: "missing base policy falls back to origin", omitPolicy: true, fallback: true},
		{name: "no common ancestor falls back to origin", unrelated: true},
		{name: "missing origin fails closed", noOrigin: true},
		{name: "committed reduction reaches deterministic zero", clean: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			policy := "internal/invariants/sqlite_policy.json"
			basePolicy := `{"files":{"example.go":{"backend_sqlite":1}},"modules":{}}`
			sqliteFixtureGit(t, root, "init", "-q", "--template=", "--initial-branch=feature")
			sqliteFixtureWrite(t, root, "go.mod", "module example.org/project\ngo 1.26.0\n")
			sqliteFixtureWrite(t, root, "example.go", "package example; const BackendSQLite = 0\n")
			if !tt.omitPolicy {
				sqliteFixtureWrite(t, root, policy, basePolicy)
			}
			sqliteFixtureGit(t, root, "add", ".")
			sqliteFixtureGit(t, root, "commit", "-qm", "test(invariants): seed integration fixture")
			if tt.fallback {
				sqliteFixtureGit(t, root, "checkout", "-qb", "integration")
				sqliteFixtureWrite(t, root, policy, basePolicy)
				sqliteFixtureGit(t, root, "add", ".")
				sqliteFixtureGit(t, root, "commit", "-qm", "test(invariants): seed origin policy")
			} else if tt.unrelated {
				sqliteFixtureGit(t, root, "checkout", "-q", "--orphan", "integration")
				sqliteFixtureGit(t, root, "commit", "-qm", "test(invariants): seed independent origin")
			}
			if !tt.noOrigin {
				sqliteFixtureGit(t, root, "update-ref", "refs/remotes/origin/develop", "HEAD")
			}
			if tt.fallback || tt.unrelated {
				sqliteFixtureGit(t, root, "checkout", "-q", "feature")
			}
			current := `{"files":{"example.go":{"backend_sqlite":2}},"modules":{}}`
			sqliteFixtureWrite(t, root, "example.go", "package example; const BackendSQLite = 0; var backend = BackendSQLite\n")
			if tt.clean {
				current = `{"files":{},"modules":{}}`
				sqliteFixtureWrite(t, root, "example.go", "package example\n")
			}
			sqliteFixtureWrite(t, root, policy, current)
			sqliteFixtureGit(t, root, "add", ".")
			sqliteFixtureGit(t, root, "commit", "-qm", "test(invariants): commit branch policy")
			problems := checkSQLiteRepository(root)
			err := shrinkSQLitePolicy(root)
			after, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(policy)))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tt.clean {
				if len(problems) > 0 || err != nil {
					t.Fatalf("clean reduction rejected: %v, %v", problems, err)
				}
				if string(after) != "{\n  \"files\": {},\n  \"modules\": {}\n}\n" {
					t.Fatalf("nonempty or nondeterministic final baseline: %s", after)
				}
				if err := shrinkSQLitePolicy(root); err != nil {
					t.Fatal(err)
				}
				second, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(policy)))
				if err != nil || string(second) != string(after) {
					t.Fatalf("second shrink differed: %s, %v", second, err)
				}
				return
			}
			want := "backend_sqlite count 2 exceeds integration baseline 1"
			if tt.noOrigin {
				want = "inspect integration SQLite baseline"
			}
			if !strings.Contains(strings.Join(problems, "\n"), want) || err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("guard or shrink trusted branch policy: %v, %v; want %q", problems, err, want)
			}
			if string(after) != current {
				t.Fatal("growth refusal modified the working policy")
			}
		})
	}
}

func TestSQLiteDependencyCompletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		hidden bool
	}{
		{name: "clean provider"},
		{name: "hidden dependency rejected", hidden: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			provider := t.TempDir()
			sqliteFixtureWrite(t, provider, "go.mod", "module example.org/provider\ngo 1.26.0\n")
			source := "package provider\n"
			if tt.hidden {
				source += "import _ \"example.org/provider/sqlite\"\n"
			}
			sqliteFixtureWrite(t, provider, "provider.go", source)
			sqliteFixtureWrite(t, provider, "sqlite/driver.go", "package sqlite\n")
			sqliteFixtureWrite(t, root, "go.mod", "module example.org/project\ngo 1.26.0\nrequire example.org/provider v0.0.0\nreplace example.org/provider => "+filepath.ToSlash(provider)+"\n")
			sqliteFixtureWrite(t, root, "main.go", "package main\nimport _ \"example.org/provider\"\nfunc main() {}\n")
			policy, err := scanSQLite(os.DirFS(root))
			if err != nil {
				t.Fatal(err)
			}
			if len(policy.Files) > 0 || len(policy.Modules) > 0 {
				t.Fatalf("importer was counted as direct debt: %+v", policy)
			}
			err = checkSQLiteDependencies(root)
			if tt.hidden {
				if err == nil || !strings.Contains(err.Error(), "SQLite remains in final Go dependencies: example.org/provider/sqlite") {
					t.Fatalf("hidden dependency escaped completion check: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
