package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCheckMigrations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		files       []string
		directories []string
		want        string
	}{
		{"unique", []string{"hub/00017_identity.sql", "hub/00018_viewed.sql", "hub/00019_onboarding.sql"}, []string{"hub"}, ""},
		{"collision", []string{"hub/00018_viewed.sql", "hub/00018_onboarding.sql"}, []string{"hub"}, "duplicate migration version 18"},
		{"numeric collision", []string{"hub/18_viewed.sql", "hub/00018_onboarding.sql"}, []string{"hub"}, "duplicate migration version 18"},
		{"canonical new version", []string{"hub/00020_migration.sql"}, []string{"hub"}, ""},
		{"named new version", []string{"hub/00020_onboarding.sql"}, []string{"hub"}, "use hub/00020_migration.sql"},
		{"unpadded new version", []string{"hub/20_migration.sql"}, []string{"hub"}, "noncanonical migration filename"},
		{"separate schemas", []string{"hub/00018_viewed.sql", "store/00018_session.sql"}, []string{"hub", "store"}, ""},
		{"ignore other files", []string{"hub/00018_viewed.sql", "hub/README.md", "hub/nested/00018_example.sql"}, []string{"hub"}, ""},
		{"invalid version", []string{"hub/name.sql"}, []string{"hub"}, "invalid migration version"},
		{"zero version", []string{"hub/00000_name.sql"}, []string{"hub"}, "invalid migration version"},
		{"overflow", []string{"hub/99999999999999999999_name.sql"}, []string{"hub"}, "invalid migration version"},
		{"empty", []string{"hub/README.md"}, []string{"hub"}, "no SQL migrations"},
		{"missing", nil, []string{"hub"}, "read migrations"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := fstest.MapFS{}
			for _, name := range test.files {
				files[name] = &fstest.MapFile{}
			}
			var schemas []migrationSchema
			for _, directory := range test.directories {
				schemas = append(schemas, migrationSchema{directory, 19})
			}
			err := checkMigrations(files, schemas)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		args      []string
		migration string
		want      int
	}{
		{"success", nil, "", 0},
		{"canonical store addition", nil, "internal/store/migrations/00069_migration.sql", 0},
		{"noncanonical store addition", nil, "internal/store/migrations/00069_feature.sql", 1},
		{"noncanonical Hub addition", nil, "internal/hubserver/migrations/00071_feature.sql", 1},
		{"noncanonical registry addition", nil, "internal/cloudentry/migrations/registry/00006_feature.sql", 1},
		{"noncanonical auth addition", nil, "internal/cloudentry/migrations/auth/00005_feature.sql", 1},
		{"missing root", []string{"-root", filepath.Join(t.TempDir(), "missing")}, "", 1},
		{"unknown flag", []string{"-unknown"}, "", 2},
		{"positional argument", []string{"unexpected"}, "", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for _, directory := range []string{"internal/store/migrations", "internal/hubserver/migrations", "internal/cloudentry/migrations/registry", "internal/cloudentry/migrations/auth"} {
				writeMigration(t, root, directory+"/00001_initial.sql")
			}
			if test.migration != "" {
				writeMigration(t, root, test.migration)
			}
			args := test.args
			if args == nil {
				args = []string{"-root", root}
			}
			var stdout, stderr bytes.Buffer
			if got := run(args, &stdout, &stderr); got != test.want {
				t.Fatalf("exit = %d, want %d: %s", got, test.want, stderr.String())
			}
			if stdout.Len()+stderr.Len() == 0 {
				t.Fatal("missing diagnostic")
			}
			if test.migration != "" && test.want == 1 && !strings.Contains(stderr.String(), "noncanonical migration filename") {
				t.Fatalf("missing filename diagnostic: %s", stderr.String())
			}
		})
	}
}

func TestIndividuallyValidBranchesRejectIntegratedCollision(t *testing.T) {
	t.Parallel()
	attributes, err := os.ReadFile("../../.gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		leftPath     string
		rightPath    string
		migration    bool
		wantConflict bool
	}{
		{"historical named collision", "hub/00018_onboarding.sql", "hub/00018_viewed.sql", true, false},
		{"canonical migration collision", "hub/00020_migration.sql", "hub/00020_migration.sql", true, true},
		{"independent migration versions", "hub/00020_migration.sql", "hub/00021_migration.sql", true, false},
		{"generated sqlc edits", "internal/store/sqlc/models.go", "internal/store/sqlc/models.go", false, true},
		{"generated templ edits", "internal/web/templates/work_templ.go", "internal/web/templates/work_templ.go", false, true},
		{"generated CSS edits", "static/css/output.css", "static/css/output.css", false, true},
		{"ordinary source edits", "internal/example/source.go", "internal/example/source.go", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			git := func(args ...string) (string, error) {
				t.Helper()
				command := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=Test User", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
				command.Dir = root
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
						command.Env = append(command.Env, entry)
					}
				}
				command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
				output, err := command.CombinedOutput()
				return string(output), err
			}
			mustGit := func(args ...string) string {
				t.Helper()
				output, err := git(args...)
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
				return strings.TrimSpace(output)
			}
			write := func(name, content string) {
				t.Helper()
				filename := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			check := func() error {
				return checkMigrations(os.DirFS(root), []migrationSchema{{"hub", 19}})
			}
			mustGit("init", "-b", "main")
			write(".gitattributes", string(attributes))
			writeMigration(t, root, "hub/00017_identity.sql")
			initial := "one\ntwo\nthree\nfour\nfive\nsix\nseven\n"
			if !test.migration {
				write(test.leftPath, initial)
			}
			mustGit("add", ".")
			mustGit("commit", "-m", "initial schema and generated files")
			mustGit("checkout", "-b", "onboarding")
			if test.migration {
				write(test.leftPath, "-- +goose Up\nCREATE TABLE onboarding (id INTEGER);\n")
			} else {
				write(test.leftPath, strings.Replace(initial, "one", "ONE", 1))
			}
			if err := check(); err != nil {
				t.Fatal(err)
			}
			mustGit("add", ".")
			mustGit("commit", "-m", "onboarding change")
			mustGit("checkout", "main")
			if test.migration {
				write(test.rightPath, "-- +goose Up\nCREATE TABLE viewed (id INTEGER);\n")
			} else {
				write(test.rightPath, strings.Replace(initial, "seven", "SEVEN", 1))
			}
			if err := check(); err != nil {
				t.Fatal(err)
			}
			mustGit("add", ".")
			mustGit("commit", "-m", "viewed change")
			for _, args := range [][]string{
				{"merge-tree", "--write-tree", "--name-only", "main", "onboarding"},
				{"merge", "--no-edit", "onboarding"},
			} {
				output, err := git(args...)
				if test.wantConflict {
					if err == nil || !strings.Contains(output, "CONFLICT") || !strings.Contains(output, test.leftPath) {
						t.Fatalf("git %v = %v: %s; want conflict for %s", args, err, output, test.leftPath)
					}
				} else if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
			}
			if test.wantConflict {
				if got := mustGit("diff", "--name-only", "--diff-filter=U"); got != test.leftPath {
					t.Fatalf("unmerged path = %q, want %q", got, test.leftPath)
				}
				return
			}
			if test.name == "historical named collision" {
				err := check()
				if err == nil || !strings.Contains(err.Error(), "duplicate migration version 18") || !strings.Contains(err.Error(), test.leftPath) || !strings.Contains(err.Error(), test.rightPath) {
					t.Fatalf("integrated error = %v", err)
				}
			} else if err := check(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func writeMigration(t *testing.T, root, name string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("-- +goose Up\nSELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
