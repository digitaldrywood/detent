package invariants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestSQLiteWorkspacePolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, source, policy, want string
		brokenGit                  bool
	}{
		{name: "bundle without Git metadata", source: "package example; const BackendSQLite = 0", policy: `{"files":{"example.go":{"backend_sqlite":1}},"modules":{}}`},
		{name: "exact checkout needs no Git refs", brokenGit: true, source: "package example; const BackendSQLite = 0", policy: `{"files":{"example.go":{"backend_sqlite":1}},"modules":{}}`},
		{name: "checked-out frozen policy is local authority", source: "package example; const BackendSQLite = 0; var backend = BackendSQLite", policy: `{"files":{"example.go":{"backend_sqlite":2}},"modules":{}}`},
		{name: "new usage rejected", source: "package example; const BackendSQLite = 0", policy: `{"files":{},"modules":{}}`, want: "exceeds frozen baseline 0"},
		{name: "empty policy reaches dependency completion", source: "package example", policy: `{"files":{},"modules":{}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			sqliteFixtureWrite(t, root, "go.mod", "module example.org/project\ngo 1.26.0\n")
			sqliteFixtureWrite(t, root, "example.go", tt.source)
			sqliteFixtureWrite(t, root, "internal/invariants/sqlite_policy.json", tt.policy)
			if tt.brokenGit {
				sqliteFixtureWrite(t, root, ".git", "gitdir: missing-directory\n")
			}
			problems := checkSQLiteRepository(root)
			if tt.want == "" {
				if len(problems) > 0 {
					t.Fatalf("history-free workspace rejected: %v", problems)
				}
			} else if !strings.Contains(strings.Join(problems, "\n"), tt.want) {
				t.Fatalf("got %v, want %q", problems, tt.want)
			}
		})
	}
}

func TestSQLiteDependencyCompletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		hidden    bool
		linuxOnly bool
	}{
		{name: "clean provider"},
		{name: "hidden dependency rejected", hidden: true},
		{name: "Linux-only dependency rejected", linuxOnly: true},
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
			if tt.linuxOnly {
				sqliteFixtureWrite(t, provider, "provider_linux.go", "package provider\nimport _ \"example.org/provider/sqlite\"\n")
			}
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
			if tt.hidden || tt.linuxOnly {
				if err == nil || !strings.Contains(err.Error(), "SQLite remains in final Go dependencies for GOOS=linux: example.org/provider/sqlite") {
					t.Fatalf("hidden dependency escaped completion check: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
