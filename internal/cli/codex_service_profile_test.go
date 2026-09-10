package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestPrepareCodexCommandForServiceCreatesLaunchdProfile(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sourceHome := filepath.Join(home, ".codex")
	for _, path := range []string{
		sourceHome,
		filepath.Join(sourceHome, "plugins"),
		filepath.Join(sourceHome, "skills", ".system"),
		filepath.Join(sourceHome, "skills", "local"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", path, err)
		}
	}
	for _, name := range []string{"auth.json", "config.toml"} {
		if err := os.WriteFile(filepath.Join(sourceHome, name), []byte(name), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	protectedTarget := filepath.Join(home, "Library", "CloudStorage", "Dropbox", "templui-pro")
	if err := os.MkdirAll(protectedTarget, 0o700); err != nil {
		t.Fatalf("MkdirAll() protected target error = %v", err)
	}
	if err := os.Symlink(protectedTarget, filepath.Join(sourceHome, "skills", "templui-pro")); err != nil {
		t.Fatalf("Symlink() protected skill error = %v", err)
	}

	prepared, err := prepareCodexCommandForService(
		"codex app-server",
		"darwin",
		string(servicepkg.ManagerLaunchd),
		func(string) (string, bool) { return "", false },
		func() (string, error) { return home, nil },
	)
	if err != nil {
		t.Fatalf("prepareCodexCommandForService() error = %v", err)
	}
	profileHome := filepath.Join(sourceHome, launchdCodexProfileDir)
	if prepared.Command != "codex app-server" {
		t.Fatalf("Command = %q, want unchanged command", prepared.Command)
	}
	if got := prepared.Environment["CODEX_HOME"]; got != profileHome {
		t.Fatalf("CODEX_HOME = %q, want %q", got, profileHome)
	}
	if !reflect.DeepEqual(prepared.OmittedSkills, []string{"templui-pro"}) {
		t.Fatalf("OmittedSkills = %#v, want templui-pro", prepared.OmittedSkills)
	}
	for _, path := range []string{
		filepath.Join(profileHome, "auth.json"),
		filepath.Join(profileHome, "config.toml"),
		filepath.Join(profileHome, "plugins"),
		filepath.Join(profileHome, "skills", ".system"),
		filepath.Join(profileHome, "skills", "local"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat(%q) error = %v", path, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink", path)
		}
	}
	if _, err := os.Lstat(filepath.Join(profileHome, "skills", "templui-pro")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protected profile skill error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(profileHome, launchdCodexProfileMarker)); err != nil {
		t.Fatalf("profile marker error = %v", err)
	}

	if _, err := prepareCodexCommandForService(
		"codex app-server",
		"darwin",
		string(servicepkg.ManagerLaunchd),
		func(string) (string, bool) { return "", false },
		func() (string, error) { return home, nil },
	); err != nil {
		t.Fatalf("second prepareCodexCommandForService() error = %v", err)
	}
}

func TestPrepareCodexCommandForServiceRewritesConfiguredHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sourceHome := filepath.Join(home, "codex profile")
	if err := os.MkdirAll(sourceHome, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	command := "env CODEX_HOME='" + sourceHome + "' codex app-server"
	prepared, err := prepareCodexCommandForService(
		command,
		"darwin",
		string(servicepkg.ManagerLaunchd),
		func(string) (string, bool) { return "", false },
		func() (string, error) { return home, nil },
	)
	if err != nil {
		t.Fatalf("prepareCodexCommandForService() error = %v", err)
	}
	profileHome := filepath.Join(sourceHome, launchdCodexProfileDir)
	want := "env CODEX_HOME='" + profileHome + "' codex app-server"
	if prepared.Command != want {
		t.Fatalf("Command = %q, want %q", prepared.Command, want)
	}
	if prepared.Environment["CODEX_HOME"] != profileHome {
		t.Fatalf("Environment = %#v, want CODEX_HOME %q", prepared.Environment, profileHome)
	}
}

func TestPrepareCodexCommandForServiceIsolatesInstructions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, goos, manager string }{
		{"manual macOS", "darwin", "manual"},
		{"Linux service", "linux", "systemd"},
		{"launchd", "darwin", "launchd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			source := filepath.Join(home, ".codex")
			if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"AGENTS.md", "AGENTS.override.md", "auth.json", "config.toml"} {
				if err := os.WriteFile(filepath.Join(source, name), []byte("Ask for confirmation before bulk changes"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := prepareCodexCommandForService("codex app-server", tt.goos, tt.manager,
				func(string) (string, bool) { return "", false }, func() (string, error) { return home, nil })
			if err != nil {
				t.Fatal(err)
			}
			profile := prepared.Environment["CODEX_HOME"]
			if profile == "" || profile == source {
				t.Fatalf("worker CODEX_HOME = %q", profile)
			}
			for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
				if _, err := os.Stat(filepath.Join(profile, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("worker sees %s: %v", name, err)
				}
				if _, err := os.Stat(filepath.Join(source, name)); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"auth.json", "config.toml"} {
				target, err := os.Readlink(filepath.Join(profile, name))
				if err != nil || target != filepath.Join(source, name) {
					t.Fatalf("%s link = %q, %v", name, target, err)
				}
			}
		})
	}
}

func TestPrepareCodexCommandForServiceRejectsProtectedAgentSkill(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sourceHome := filepath.Join(home, ".codex")
	agentSkills := filepath.Join(home, ".agents", "skills")
	protectedTarget := filepath.Join(home, "Documents", "shared-skill")
	for _, path := range []string{sourceHome, agentSkills, protectedTarget} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", path, err)
		}
	}
	if err := os.Symlink(protectedTarget, filepath.Join(agentSkills, "shared")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	_, err := prepareCodexCommandForService(
		"codex app-server",
		"darwin",
		string(servicepkg.ManagerLaunchd),
		func(string) (string, bool) { return "", false },
		func() (string, error) { return home, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "protected ~/.agents/skills links: shared") {
		t.Fatalf("error = %v, want protected agent skill detail", err)
	}
}

func TestLaunchdProtectedSkillLinkResolvesSymlinkedParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realHome := filepath.Join(root, "real-home")
	if err := os.MkdirAll(realHome, 0o700); err != nil {
		t.Fatalf("MkdirAll() home error = %v", err)
	}
	home := filepath.Join(root, "home")
	if err := os.Symlink(realHome, home); err != nil {
		t.Fatalf("Symlink() home error = %v", err)
	}
	protectedRoot := filepath.Join(home, "Documents", "shared")
	if err := os.MkdirAll(filepath.Join(protectedRoot, "skill"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	linkedParent := filepath.Join(home, "shared")
	if err := os.Symlink(protectedRoot, linkedParent); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	skillLink := filepath.Join(home, ".codex", "skills", "shared")
	if err := os.MkdirAll(filepath.Dir(skillLink), 0o700); err != nil {
		t.Fatalf("MkdirAll() skill root error = %v", err)
	}
	if err := os.Symlink(filepath.Join(linkedParent, "skill"), skillLink); err != nil {
		t.Fatalf("Symlink() skill error = %v", err)
	}

	protected, err := launchdProtectedSkillLink(skillLink, home)
	if err != nil {
		t.Fatalf("launchdProtectedSkillLink() error = %v", err)
	}
	if !protected {
		t.Fatal("launchdProtectedSkillLink() = false, want true")
	}
}

func TestPrepareWorkerCodexHomeExistingInstructions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		link bool
	}{{"legacy symlink", true}, {"local file", false}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			source := filepath.Join(home, ".codex")
			profile := filepath.Join(source, launchdCodexProfileDir)
			if err := ensureLaunchdCodexProfile(profile); err != nil {
				t.Fatal(err)
			}
			hostFile := filepath.Join(source, "AGENTS.md")
			if err := os.WriteFile(hostFile, []byte("Ask for confirmation"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(profile, "AGENTS.md")
			if tt.link {
				if err := os.Symlink(hostFile, target); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("Local instructions"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := prepareWorkerCodexHome(source, home, true)
			if tt.link {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy link remains: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), target) {
				t.Fatalf("error = %v, want local instruction path", err)
			}
			if _, err := os.Stat(hostFile); err != nil {
				t.Fatal(err)
			}
		})
	}
}
