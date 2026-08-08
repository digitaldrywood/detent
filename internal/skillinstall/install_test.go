package skillinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/operatorskill"
)

var testBuild = buildinfo.Info{
	Version: "v1.2.3",
	Commit:  "abcdef1234567890",
	Date:    "2026-08-08T00:00:00Z",
}

func TestCompatibilityMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target    Target
		client    string
		segments  []string
		discovery string
	}{
		{target: TargetClaudeCode, client: "Claude Code", segments: []string{".claude", "skills"}, discovery: "personal skills"},
		{target: TargetCodex, client: "Codex", segments: []string{".agents", "skills"}, discovery: "user skills"},
		{target: TargetAntigravity, client: "Antigravity", segments: []string{".gemini", "config", "skills"}, discovery: "global skills"},
	}
	matrix := CompatibilityMatrix()
	if len(matrix) != len(tests) {
		t.Fatalf("CompatibilityMatrix() length = %d, want %d", len(matrix), len(tests))
	}
	for index, tt := range tests {
		got := matrix[index]
		if got.Target != tt.target || got.Client != tt.client || !slices.Equal(got.RootSegments, tt.segments) || got.Discovery != tt.discovery {
			t.Fatalf("CompatibilityMatrix()[%d] = %#v, want target=%q client=%q segments=%q discovery=%q", index, got, tt.target, tt.client, tt.segments, tt.discovery)
		}
	}
}

func TestRootsUseInjectedHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	roots, err := Roots(home)
	if err != nil {
		t.Fatalf("Roots() error = %v", err)
	}
	tests := []struct {
		target Target
		want   string
	}{
		{target: TargetClaudeCode, want: filepath.Join(home, ".claude", "skills")},
		{target: TargetCodex, want: filepath.Join(home, ".agents", "skills")},
		{target: TargetAntigravity, want: filepath.Join(home, ".gemini", "config", "skills")},
	}
	for _, tt := range tests {
		t.Run(string(tt.target), func(t *testing.T) {
			t.Parallel()
			if got := roots[tt.target]; got.Base != home || got.Skills != tt.want {
				t.Fatalf("root = %#v, want base %q skills %q", got, home, tt.want)
			}
		})
	}
}

func TestParseTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		values  []string
		want    []Target
		wantErr bool
	}{
		{name: "matrix order and deduplication", values: []string{"antigravity", "codex", "claude-code", "codex"}, want: []Target{TargetClaudeCode, TargetCodex, TargetAntigravity}},
		{name: "case and whitespace", values: []string{" Codex ", "CLAUDE-CODE"}, want: []Target{TargetClaudeCode, TargetCodex}},
		{name: "path traversal", values: []string{"../codex"}, wantErr: true},
		{name: "absolute path", values: []string{"/tmp/codex"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTargets(tt.values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseTargets() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("ParseTargets() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstallWritesDeterministicTargetPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target Target
	}{
		{target: TargetClaudeCode},
		{target: TargetCodex},
		{target: TargetAntigravity},
	}
	for _, tt := range tests {
		t.Run(string(tt.target), func(t *testing.T) {
			t.Parallel()
			config := testConfig(t, tt.target)
			result, err := Install(config)
			if err != nil {
				t.Fatalf("Install() error = %v", err)
			}
			if result.Status != "complete" || len(result.Targets) != 1 || result.Targets[0].Status != "installed" || result.Targets[0].Intent != "install" {
				t.Fatalf("result = %#v, want completed install", result)
			}

			destination := filepath.Join(config.Roots[tt.target].Skills, operatorskill.Directory)
			assertFile(t, filepath.Join(destination, operatorskill.SkillFile), operatorskill.Content(), fileMode)
			manifestPath := filepath.Join(destination, ManifestFile)
			manifestRaw := readFile(t, manifestPath)
			var manifest Manifest
			if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", manifestPath, err)
			}
			if manifest.Schema != SchemaVersion || manifest.Skill != operatorskill.Name || manifest.BundleVersion != operatorskill.Version || manifest.BundleSHA256 != digest(operatorskill.Content()) || manifest.DetentBuild != testBuild {
				t.Fatalf("manifest = %#v, want embedded bundle and build stamp", manifest)
			}
			if bytes.Contains(manifestRaw, []byte(config.Roots[tt.target].Base)) {
				t.Fatalf("manifest contains runtime path %q", config.Roots[tt.target].Base)
			}
			assertFileMode(t, manifestPath, fileMode)
			assertFileMode(t, destination, directoryMode)
			if strings.Contains(destination, filepath.Join(".detent", "skills")) {
				t.Fatalf("destination %q uses worker metadata", destination)
			}
			assertActionsCoverManagedPaths(t, result, destination)
		})
	}
}

func TestInstallDryRunAndIdempotency(t *testing.T) {
	t.Parallel()

	config := testConfig(t, TargetClaudeCode, TargetCodex, TargetAntigravity)
	dryRunConfig := config
	dryRunConfig.DryRun = true
	dryRun, err := Install(dryRunConfig)
	if err != nil {
		t.Fatalf("Install(dry run) error = %v", err)
	}
	if dryRun.Status != "dry_run" || len(dryRun.Targets) != 3 {
		t.Fatalf("dry run result = %#v", dryRun)
	}
	for _, target := range dryRun.Targets {
		if target.Status != "dry_run" {
			t.Fatalf("target %q status = %q, want dry_run", target.Target, target.Status)
		}
		if _, statErr := os.Lstat(target.Destination); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("dry run destination %q exists or returned %v", target.Destination, statErr)
		}
	}

	if _, err := Install(config); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	result, err := Install(config)
	if err != nil {
		t.Fatalf("Install(reinstall) error = %v", err)
	}
	if result.Status != "complete" {
		t.Fatalf("reinstall status = %q, want complete", result.Status)
	}
	for _, target := range result.Targets {
		if target.Intent != "reinstall" || target.Status != "unchanged" {
			t.Fatalf("reinstall target = %#v, want unchanged reinstall", target)
		}
	}
	for _, action := range result.Actions {
		if action.Status == "completed" || action.Action == "write" || action.Action == "backup" {
			t.Fatalf("idempotent reinstall action = %#v", action)
		}
	}
}

func TestInstallRefusesDifferingManagedContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
	}{
		{name: "modified skill", file: operatorskill.SkillFile},
		{name: "modified manifest", file: ManifestFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig(t, TargetCodex)
			if _, err := Install(config); err != nil {
				t.Fatalf("Install() error = %v", err)
			}
			destination := filepath.Join(config.Roots[TargetCodex].Skills, operatorskill.Directory)
			path := filepath.Join(destination, tt.file)
			modified := []byte("user-modified\n")
			if err := os.WriteFile(path, modified, fileMode); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			result, err := Install(config)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("Install() error = %v, want %v", err, ErrConflict)
			}
			if result.Status != "failed" || result.Targets[0].Status != "failed" {
				t.Fatalf("result = %#v, want failed conflict", result)
			}
			if got := readFile(t, path); !bytes.Equal(got, modified) {
				t.Fatalf("modified content = %q, want preserved %q", got, modified)
			}
			matches, globErr := filepath.Glob(path + ".detent-backup-*")
			if globErr != nil || len(matches) != 0 {
				t.Fatalf("backup matches = %q, error = %v, want none", matches, globErr)
			}
		})
	}
}

func TestInstallPreflightsEveryTargetBeforeWriting(t *testing.T) {
	t.Parallel()

	config := testConfig(t, TargetClaudeCode, TargetCodex)
	codexDestination := filepath.Join(config.Roots[TargetCodex].Skills, operatorskill.Directory)
	if err := os.MkdirAll(codexDestination, directoryMode); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	modified := []byte("existing user skill\n")
	if err := os.WriteFile(filepath.Join(codexDestination, operatorskill.SkillFile), modified, fileMode); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := Install(config)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Install() error = %v, want %v", err, ErrConflict)
	}
	claudeDestination := filepath.Join(config.Roots[TargetClaudeCode].Skills, operatorskill.Directory)
	if _, statErr := os.Lstat(claudeDestination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("first target destination exists or returned %v", statErr)
	}
	if got := readFile(t, filepath.Join(codexDestination, operatorskill.SkillFile)); !bytes.Equal(got, modified) {
		t.Fatalf("conflicting target changed to %q", got)
	}
	if result.Targets[0].Status != "not_applied" || result.Targets[1].Status != "failed" {
		t.Fatalf("target results = %#v, want not_applied then failed", result.Targets)
	}
}

func TestInstallForceBacksUpAndPreservesUnrelatedFiles(t *testing.T) {
	t.Parallel()

	config := testConfig(t, TargetAntigravity)
	if _, err := Install(config); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	destination := filepath.Join(config.Roots[TargetAntigravity].Skills, operatorskill.Directory)
	skillPath := filepath.Join(destination, operatorskill.SkillFile)
	modified := []byte("user-modified skill\n")
	if err := os.WriteFile(skillPath, modified, 0o644); err != nil {
		t.Fatalf("WriteFile(skill) error = %v", err)
	}
	unrelatedPath := filepath.Join(destination, "operator-notes.md")
	unrelated := []byte("preserve me\n")
	if err := os.WriteFile(unrelatedPath, unrelated, 0o644); err != nil {
		t.Fatalf("WriteFile(unrelated) error = %v", err)
	}

	config.Force = true
	result, err := Install(config)
	if err != nil {
		t.Fatalf("Install(force) error = %v", err)
	}
	if result.Targets[0].Intent != "repair" || result.Targets[0].Status != "installed" {
		t.Fatalf("target result = %#v, want installed repair", result.Targets[0])
	}
	assertFile(t, skillPath, operatorskill.Content(), fileMode)
	assertFile(t, backupPath(skillPath, modified), modified, fileMode)
	assertFile(t, unrelatedPath, unrelated, 0o644)
}

func TestInstallSecuresExactManagedFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX permission bits")
	}
	t.Parallel()

	config := testConfig(t, TargetCodex)
	if _, err := Install(config); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	destination := filepath.Join(config.Roots[TargetCodex].Skills, operatorskill.Directory)
	skillPath := filepath.Join(destination, operatorskill.SkillFile)
	if err := os.Chmod(skillPath, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	result, err := Install(config)
	if err != nil {
		t.Fatalf("Install(reinstall) error = %v", err)
	}
	if result.Targets[0].Intent != "reinstall" || result.Targets[0].Status != "installed" {
		t.Fatalf("target = %#v, want permission repair", result.Targets[0])
	}
	assertFile(t, skillPath, operatorskill.Content(), fileMode)
	for _, action := range result.Actions {
		if action.Path == skillPath && action.Action != "secure_permissions" {
			t.Fatalf("skill action = %#v, want secure_permissions", action)
		}
	}
}

func TestInstallUpgradeAndDowngradeRequireForce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		bundleVersion int
		intent        string
	}{
		{name: "upgrade", bundleVersion: operatorskill.Version - 1, intent: "upgrade"},
		{name: "downgrade", bundleVersion: operatorskill.Version + 1, intent: "downgrade"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig(t, TargetClaudeCode)
			if _, err := Install(config); err != nil {
				t.Fatalf("Install() error = %v", err)
			}
			destination := filepath.Join(config.Roots[TargetClaudeCode].Skills, operatorskill.Directory)
			manifestPath := filepath.Join(destination, ManifestFile)
			oldManifest, err := encodeManifest(Manifest{
				Schema:        SchemaVersion,
				Skill:         operatorskill.Name,
				BundleVersion: tt.bundleVersion,
				BundleSHA256:  strings.Repeat("a", 64),
				DetentBuild:   buildinfo.Info{Version: "v0.9.0", Commit: "old", Date: "2026-01-01T00:00:00Z"},
			})
			if err != nil {
				t.Fatalf("encodeManifest() error = %v", err)
			}
			if err := os.WriteFile(manifestPath, oldManifest, fileMode); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			refused, err := Install(config)
			if !errors.Is(err, ErrConflict) || refused.Targets[0].Intent != tt.intent {
				t.Fatalf("Install() result = %#v, error = %v, want %s conflict", refused, err, tt.intent)
			}
			config.Force = true
			installed, err := Install(config)
			if err != nil {
				t.Fatalf("Install(force) error = %v", err)
			}
			if installed.Targets[0].Intent != tt.intent || installed.Targets[0].Status != "installed" {
				t.Fatalf("Install(force) target = %#v", installed.Targets[0])
			}
			assertFile(t, backupPath(manifestPath, oldManifest), oldManifest, fileMode)
		})
	}
}

func TestInstallRejectsSymlinkEscapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, Config) string
	}{
		{
			name: "skills root component",
			setup: func(t *testing.T, config Config) string {
				claudeRoot := filepath.Join(config.Roots[TargetClaudeCode].Base, ".claude")
				if err := os.MkdirAll(claudeRoot, directoryMode); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				link := filepath.Join(claudeRoot, "skills")
				createSymlink(t, t.TempDir(), link)
				return link
			},
		},
		{
			name: "destination",
			setup: func(t *testing.T, config Config) string {
				if err := os.MkdirAll(config.Roots[TargetClaudeCode].Skills, directoryMode); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				link := filepath.Join(config.Roots[TargetClaudeCode].Skills, operatorskill.Directory)
				createSymlink(t, t.TempDir(), link)
				return link
			},
		},
		{
			name: "skill file",
			setup: func(t *testing.T, config Config) string {
				destination := filepath.Join(config.Roots[TargetClaudeCode].Skills, operatorskill.Directory)
				if err := os.MkdirAll(destination, directoryMode); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				victim := filepath.Join(t.TempDir(), "victim")
				if err := os.WriteFile(victim, []byte("do not replace"), fileMode); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				link := filepath.Join(destination, operatorskill.SkillFile)
				createSymlink(t, victim, link)
				return link
			},
		},
		{
			name: "manifest file",
			setup: func(t *testing.T, config Config) string {
				destination := filepath.Join(config.Roots[TargetClaudeCode].Skills, operatorskill.Directory)
				if err := os.MkdirAll(destination, directoryMode); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				victim := filepath.Join(t.TempDir(), "victim")
				if err := os.WriteFile(victim, []byte("do not replace"), fileMode); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				link := filepath.Join(destination, ManifestFile)
				createSymlink(t, victim, link)
				return link
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig(t, TargetClaudeCode)
			path := tt.setup(t, config)
			result, err := Install(config)
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Install() error = %v, want %v", err, ErrUnsafePath)
			}
			if result.Status != "failed" || result.Targets[0].Status != "failed" {
				t.Fatalf("result = %#v, want unsafe-path failure", result)
			}
			info, statErr := os.Lstat(path)
			if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("Lstat(%s) = %v, %v, want preserved symlink", path, info, statErr)
			}
		})
	}
}

func TestInstallRejectsTraversalAndSymlinkedBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		root func(*testing.T) Root
	}{
		{
			name: "skills root outside base",
			root: func(t *testing.T) Root {
				base := t.TempDir()
				return Root{Base: base, Skills: filepath.Join(filepath.Dir(base), "escaped-skills")}
			},
		},
		{
			name: "symlinked base",
			root: func(t *testing.T) Root {
				realBase := t.TempDir()
				link := filepath.Join(t.TempDir(), "home")
				createSymlink(t, realBase, link)
				return Root{Base: link, Skills: filepath.Join(link, ".agents", "skills")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := tt.root(t)
			result, err := Install(Config{
				Roots:   map[Target]Root{TargetCodex: root},
				Targets: []Target{TargetCodex},
				Build:   testBuild,
			})
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Install() error = %v, want %v", err, ErrUnsafePath)
			}
			if result.Status != "failed" {
				t.Fatalf("status = %q, want failed", result.Status)
			}
		})
	}
}

func TestInstallRejectsSymlinkedBackupPath(t *testing.T) {
	t.Parallel()

	config := testConfig(t, TargetClaudeCode)
	if _, err := Install(config); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	destination := filepath.Join(config.Roots[TargetClaudeCode].Skills, operatorskill.Directory)
	skillPath := filepath.Join(destination, operatorskill.SkillFile)
	modified := []byte("user-modified skill\n")
	if err := os.WriteFile(skillPath, modified, fileMode); err != nil {
		t.Fatalf("WriteFile(skill) error = %v", err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	victimContent := []byte("do not replace\n")
	if err := os.WriteFile(victim, victimContent, fileMode); err != nil {
		t.Fatalf("WriteFile(victim) error = %v", err)
	}
	backup := backupPath(skillPath, modified)
	createSymlink(t, victim, backup)

	config.Force = true
	result, err := Install(config)
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Install(force) error = %v, want %v", err, ErrUnsafePath)
	}
	if result.Status != "failed" || result.Targets[0].Status != "failed" {
		t.Fatalf("result = %#v, want rejected backup symlink", result)
	}
	assertFile(t, skillPath, modified, fileMode)
	assertFile(t, victim, victimContent, fileMode)
}

func TestInstallRollsBackPartialMultiTargetFailure(t *testing.T) {
	t.Parallel()

	config := testConfig(t, TargetClaudeCode, TargetCodex)
	failed := false
	result, err := install(config, fileOperations{
		mkdir:  os.Mkdir,
		remove: os.Remove,
		atomicWrite: func(path string, content []byte, mode os.FileMode) error {
			codexSkill := filepath.Join(config.Roots[TargetCodex].Skills, operatorskill.Directory, operatorskill.SkillFile)
			if path == codexSkill && !failed {
				failed = true
				return errors.New("injected write failure")
			}
			return atomicWrite(path, content, mode)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("install() error = %v, want injected failure", err)
	}
	if result.Status != "failed" || len(result.Rollback) == 0 {
		t.Fatalf("result = %#v, want failed result with rollback actions", result)
	}
	for _, target := range result.Targets {
		if _, statErr := os.Lstat(target.Destination); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rolled-back destination %q exists or returned %v", target.Destination, statErr)
		}
	}
}

func testConfig(t *testing.T, targets ...Target) Config {
	t.Helper()
	home := t.TempDir()
	roots, err := Roots(home)
	if err != nil {
		t.Fatalf("Roots() error = %v", err)
	}
	return Config{Roots: roots, Targets: slices.Clone(targets), Build: testBuild}
}

func createSymlink(t *testing.T, target string, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("create symlink: %v", err)
		}
		t.Fatalf("Symlink() error = %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return content
}

func assertFile(t *testing.T, path string, want []byte, mode os.FileMode) {
	t.Helper()
	if got := readFile(t, path); !bytes.Equal(got, want) {
		t.Fatalf("file %s = %q, want %q", path, got, want)
	}
	assertFileMode(t, path, mode)
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want.Perm() {
		t.Fatalf("mode(%s) = %o, want %o", path, got, want)
	}
}

func assertActionsCoverManagedPaths(t *testing.T, result Result, destination string) {
	t.Helper()
	want := map[string]bool{
		filepath.Join(destination, operatorskill.SkillFile): false,
		filepath.Join(destination, ManifestFile):            false,
	}
	for _, action := range result.Actions {
		if _, ok := want[action.Path]; ok {
			want[action.Path] = true
		}
	}
	for path, covered := range want {
		if !covered {
			t.Fatalf("actions do not include managed path %s: %#v", path, result.Actions)
		}
	}
}
