package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/operatorskill"
	"github.com/digitaldrywood/detent/internal/skillinstall"
)

func TestSkillInstallCommandRequiresExplicitTarget(t *testing.T) {
	t.Parallel()

	called := false
	cmd := newSkillInstallCommand(skillInstallDeps{
		homeDir: func() (string, error) { return t.TempDir(), nil },
		install: func(skillinstall.Config) (skillinstall.Result, error) {
			called = true
			return skillinstall.Result{}, nil
		},
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)

	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--target is required") {
		t.Fatalf("ExecuteContext() error = %v, want required target", err)
	}
	if called {
		t.Fatal("installer called without --target")
	}
}

func TestSkillInstallCommandDryRunJSONListsEveryAction(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	cmd := newSkillInstallCommand(skillInstallDeps{
		homeDir: func() (string, error) { return home, nil },
		install: skillinstall.Install,
		build: buildinfo.Info{
			Version: "v1.2.3",
			Commit:  "abcdef1",
			Date:    "2026-08-08T00:00:00Z",
		},
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--target", "antigravity", "--target", "claude-code", "--dry-run"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	var result skillinstall.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
	}
	if result.Status != "dry_run" || len(result.Targets) != 2 || len(result.Actions) == 0 {
		t.Fatalf("result = %#v, want two-target dry run with actions", result)
	}
	for _, target := range result.Targets {
		if _, err := os.Lstat(target.Destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry-run destination %q exists or returned %v", target.Destination, err)
		}
		for _, name := range []string{operatorskill.SkillFile, skillinstall.ManifestFile} {
			path := filepath.Join(target.Destination, name)
			if !resultHasActionPath(result, path) {
				t.Fatalf("result actions do not list %s: %#v", path, result.Actions)
			}
		}
	}
}

func TestSkillInstallCommandJSONConflictNeverPrompts(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	roots, err := skillinstall.Roots(home)
	if err != nil {
		t.Fatalf("Roots() error = %v", err)
	}
	destination := filepath.Join(roots[skillinstall.TargetCodex].Skills, operatorskill.Directory)
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	skillPath := filepath.Join(destination, operatorskill.SkillFile)
	modified := []byte("user content\n")
	if err := os.WriteFile(skillPath, modified, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := newSkillInstallCommand(skillInstallDeps{
		homeDir: func() (string, error) { return home, nil },
		install: skillinstall.Install,
		build:   buildinfo.Info{Version: "v1.2.3", Commit: "abcdef1", Date: "2026-08-08T00:00:00Z"},
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetArgs([]string{"--target", "codex"})

	err = cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("ExecuteContext() error = %v, want explicit --force hint", err)
	}
	var result skillinstall.Result
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", decodeErr, stdout.String())
	}
	got := readSkillCLIFile(t, skillPath)
	if result.Status != "failed" || !bytes.Equal(got, modified) {
		t.Fatalf("result = %#v, content = %q, want safe conflict", result, got)
	}
}

func TestRootCommandCatalogsSkillInstall(t *testing.T) {
	t.Parallel()

	root := newRootCommand(context.Background())
	command, _, err := root.Find([]string{"skill", "install"})
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if command == nil || command.CommandPath() != "detent skill install" {
		t.Fatalf("command = %#v, want detent skill install", command)
	}
	for _, flag := range []string{"target", "dry-run", "force"} {
		if command.Flags().Lookup(flag) == nil {
			t.Fatalf("skill install does not catalog --%s", flag)
		}
	}
}

func resultHasActionPath(result skillinstall.Result, path string) bool {
	for _, action := range result.Actions {
		if action.Path == path || action.BackupPath == path {
			return true
		}
	}
	return false
}

func readSkillCLIFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return content
}
