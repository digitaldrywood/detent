package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestPrepareCodexCommandForServiceCreatesLaunchdProfile(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sourceHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(sourceHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte("auth.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	for range 2 {
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
		if _, err := os.Stat(filepath.Join(profileHome, launchdCodexProfileMarker)); err != nil {
			t.Fatalf("profile marker error = %v", err)
		}
	}
}

func TestPrepareWorkerCodexHomeDropsOperatorTools(t *testing.T) {
	t.Parallel()
	const hostConfig = `model = "gpt-5.6-codex"
model_provider = "openai"
model_reasoning_effort = "high"
notify = ["say", "done"]

[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"

[mcp_servers.personal]
command = "personal-mcp"

[plugins.personal]
enabled = true

[hooks]
session_start = "operator-hook"

[skills]
enabled = true
`
	for _, tt := range []struct {
		name    string
		launchd bool
		stale   bool
	}{
		{name: "fresh worker profile"},
		{name: "fresh launchd profile", launchd: true},
		{name: "previously inherited worker profile", stale: true},
		{name: "previously inherited launchd profile", launchd: true, stale: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			source := filepath.Join(home, ".codex")
			for _, dir := range []string{"plugins", "skills/personal", "rules"} {
				if err := os.MkdirAll(filepath.Join(source, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for name, content := range map[string]string{"auth.json": "credential fixture", "config.toml": hostConfig, "hooks.json": "{}"} {
				if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			profileDir := workerCodexProfileDir
			if tt.launchd {
				profileDir = launchdCodexProfileDir
			}
			profile := filepath.Join(source, profileDir)
			if tt.stale && runtime.GOOS != "windows" {
				if err := ensureLaunchdCodexProfile(profile); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"config.toml", "plugins", "hooks.json", "rules"} {
					if err := os.Symlink(filepath.Join(source, name), filepath.Join(profile, name)); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(filepath.Join(profile, "skills"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(source, "skills", "personal"), filepath.Join(profile, "skills", "personal")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := prepareWorkerCodexHome(source, home, tt.launchd)
			if err != nil {
				t.Fatal(err)
			}
			if got != profile {
				t.Fatalf("profile = %q, want %q", got, profile)
			}
			for _, name := range []string{"plugins", "hooks.json", "rules", filepath.Join("skills", "personal")} {
				if _, err := os.Lstat(filepath.Join(profile, name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("worker inherited %s: %v", name, err)
				}
			}
			auth, err := os.Stat(filepath.Join(profile, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(filepath.Join(source, "auth.json"))
			if err != nil || !os.SameFile(auth, original) {
				t.Fatalf("auth.json does not share its source: %v", err)
			}
			info, err := os.Lstat(filepath.Join(profile, "config.toml"))
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("worker config.toml = %v, %v; want a generated file", info, err)
			}
			raw, err := os.ReadFile(filepath.Join(profile, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := toml.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"plugins", "hooks", "skills", "notify"} {
				if _, ok := settings[key]; ok {
					t.Errorf("worker config kept %s:\n%s", key, raw)
				}
			}
			servers, _ := settings["mcp_servers"].(map[string]any)
			if servers["personal"] == nil {
				t.Errorf("worker config dropped MCP servers that workflows use for browser verification:\n%s", raw)
			}
			providers, _ := settings["model_providers"].(map[string]any)
			if settings["model"] != "gpt-5.6-codex" || settings["model_provider"] != "openai" || settings["model_reasoning_effort"] != "high" || providers["openai"] == nil {
				t.Fatalf("worker config lost model settings:\n%s", raw)
			}
			host, err := os.ReadFile(filepath.Join(source, "config.toml"))
			if err != nil || string(host) != hostConfig {
				t.Fatalf("host config changed: %v", err)
			}
		})
	}
}

func TestPrepareCodexCommandForServiceIsolatesUserSkills(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, goos, manager string }{
		{name: "launchd", goos: "darwin", manager: string(servicepkg.ManagerLaunchd)},
		{name: "manual macOS", goos: "darwin", manager: "manual"},
		{name: "Linux service", goos: "linux", manager: "systemd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			workspace := t.TempDir()
			elsewhere := t.TempDir()
			skills := map[string]string{
				"personal":      filepath.Join(home, ".agents", "skills", "personal"),
				"nested":        filepath.Join(home, ".agents", "skills", "group", "nested"),
				"linked target": filepath.Join(elsewhere, "linked"),
				"repository":    filepath.Join(workspace, ".agents", "skills", "repository-skill"),
			}
			for _, dir := range skills {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: fixture\n---\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(skills["linked target"], filepath.Join(home, ".agents", "skills", "linked")); err != nil {
				t.Fatal(err)
			}
			prepared, err := prepareCodexCommandForService("codex app-server", tt.goos, tt.manager, func(string) (string, bool) { return "", false }, func() (string, error) { return home, nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := prepared.Environment["HOME"]; ok {
				t.Fatal("worker HOME was overridden")
			}
			raw, err := os.ReadFile(filepath.Join(prepared.Environment["CODEX_HOME"], "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				Skills struct {
					Config []struct {
						Path    string
						Enabled *bool
					}
				}
			}
			if err := toml.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			disabled := map[string]bool{}
			for _, entry := range settings.Skills.Config {
				if entry.Enabled == nil || *entry.Enabled {
					t.Fatalf("skills.config entry %q is not disabled:\n%s", entry.Path, raw)
				}
				disabled[entry.Path] = true
			}
			for _, path := range []string{
				filepath.Join(skills["personal"], "SKILL.md"),
				filepath.Join(skills["nested"], "SKILL.md"),
				filepath.Join(home, ".agents", "skills", "linked", "SKILL.md"),
			} {
				if !disabled[path] {
					t.Errorf("user skill %s stays enabled:\n%s", path, raw)
				}
			}
			if len(disabled) != 3 || strings.Contains(string(raw), workspace) {
				t.Fatalf("worker config disables repository skills:\n%s", raw)
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
			for _, name := range []string{"AGENTS.md", "AGENTS.override.md", "auth.json"} {
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
			for _, name := range []string{"goals_1.sqlite", "logs_2.sqlite-shm", "logs_2.sqlite-wal"} {
				hostPath := filepath.Join(source, name)
				if err := os.WriteFile(hostPath, []byte("host state"), 0o600); err != nil {
					t.Fatal(err)
				}
				profilePath := filepath.Join(profile, name)
				if err := ensureProfileLink(hostPath, profilePath); err != nil {
					t.Fatal(err)
				}
			}
			localSQLite := filepath.Join(profile, "local.sqlite")
			if err := os.WriteFile(localSQLite, []byte("worker state"), 0o600); err != nil {
				t.Fatal(err)
			}
			staleSQLite := filepath.Join(profile, "stale.sqlite-wal")
			if runtime.GOOS != "windows" {
				if err := os.Symlink(filepath.Join(source, "removed.sqlite-wal"), staleSQLite); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareWorkerCodexHome(source, home, tt.manager == "launchd"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"goals_1.sqlite", "logs_2.sqlite-shm", "logs_2.sqlite-wal"} {
				if _, err := os.Lstat(filepath.Join(profile, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("worker shares %s: %v", name, err)
				}
			}
			if runtime.GOOS != "windows" {
				if _, err := os.Lstat(staleSQLite); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale worker SQLite link remains: %v", err)
				}
			}
			content, err := os.ReadFile(localSQLite)
			if err != nil || string(content) != "worker state" {
				t.Fatalf("local SQLite state = %q, %v", content, err)
			}
			for _, name := range []string{"auth.json"} {
				target, err := os.Stat(filepath.Join(profile, name))
				if err != nil {
					t.Fatal(err)
				}
				original, err := os.Stat(filepath.Join(source, name))
				if err != nil || !os.SameFile(target, original) {
					t.Fatalf("%s does not share its source: %v", name, err)
				}
			}
		})
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
			_, err := prepareWorkerCodexHome(source, home, true)
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

func TestPrepareWorkerCodexHomeInstructionCase(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"agents.md", "agents.override.md", "AgEnTs.Md"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, name), []byte("Ask for confirmation"), 0o600); err != nil {
				t.Fatal(err)
			}
			profile, err := prepareWorkerCodexHome(source, t.TempDir(), false)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(profile)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if codexInstructionFilename(entry.Name()) {
					t.Fatalf("worker inherited %s", entry.Name())
				}
			}
		})
	}
}

func TestEnsureWindowsCodexProfileLink(t *testing.T) {
	t.Parallel()
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%t", directory), func(t *testing.T) {
			t.Parallel()
			if directory && runtime.GOOS != "windows" {
				t.Skip("Windows junction support")
			}
			root := t.TempDir()
			source := filepath.Join(root, "source & profile café")
			destination := filepath.Join(root, "destination & profile")
			if directory {
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(source, []byte("credential fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := ensureWindowsCodexProfileLink(source, destination); err != nil {
					t.Fatal(err)
				}
				original, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				linked, err := os.Stat(destination)
				if err != nil || !os.SameFile(original, linked) {
					t.Fatalf("profile does not share source: %v", err)
				}
			}
			if !directory {
				replacement := filepath.Join(root, "replacement")
				if err := os.WriteFile(replacement, []byte("refreshed credential"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, source); err != nil {
					t.Fatal(err)
				}
				if err := ensureWindowsCodexProfileLink(source, destination); err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(destination)
				if err != nil || string(content) != "refreshed credential" {
					t.Fatalf("refreshed profile = %q, %v", content, err)
				}
			}
		})
	}
}

func TestCodexTransportLaunchRequiresPreparedProfile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture command")
	}
	for _, tt := range []struct {
		name    string
		change  func(t *testing.T, home string, source string)
		wantErr string
	}{
		{name: "newly added user skill is disabled before launch", change: func(t *testing.T, home string, _ string) {
			writeUserSkillFixture(t, filepath.Join(home, ".agents", "skills", "late"))
		}},
		{name: "malformed source config refuses launch", wantErr: "parse Codex config", change: func(t *testing.T, _ string, source string) {
			if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = [unterminated"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unreadable new user skill refuses launch", wantErr: "user skill directory", change: func(t *testing.T, home string, _ string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads every directory")
			}
			locked := filepath.Join(home, ".agents", "skills", "locked")
			writeUserSkillFixture(t, locked)
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			source := filepath.Join(home, "codex-source")
			if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = \"gpt-5.6-codex\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeUserSkillFixture(t, filepath.Join(home, ".agents", "skills", "personal"))
			marker := filepath.Join(t.TempDir(), "launched-config.toml")
			command := "env CODEX_HOME='" + source + "' sh -c 'cp \"$CODEX_HOME/config.toml\" \"" + marker + "\"; cat >/dev/null'"
			prepare := func(command string) (codexServiceCommand, error) {
				return prepareCodexCommandForService(command, runtime.GOOS, "manual", func(string) (string, bool) { return "", false }, func() (string, error) { return home, nil })
			}
			factory, _, err := newCodexTransportFactory(command, "sh", prepare)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, home, source)
			transport, err := factory.NewTransport(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewTransport() error = %v, want %q", err, tt.wantErr)
				}
				if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("provider process started after failed preparation: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := transport.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			launched, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			for _, skill := range []string{"personal", "late"} {
				if !strings.Contains(string(launched), filepath.Join(home, ".agents", "skills", skill, "SKILL.md")) {
					t.Errorf("launched config does not disable %s:\n%s", skill, launched)
				}
			}
		})
	}
}

func TestPrepareWorkerCodexHomeNeverExposesMissingConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	source := filepath.Join(home, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = \"gpt-5.6-codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeUserSkillFixture(t, filepath.Join(home, ".agents", "skills", "personal"))
	profile, err := prepareWorkerCodexHome(source, home, false)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(profile, "config.toml")
	done := make(chan struct{})
	writers := make(chan error, 4)
	for range 4 {
		go func() {
			for range 25 {
				if _, err := prepareWorkerCodexHome(source, home, false); err != nil {
					writers <- err
					return
				}
			}
			writers <- nil
		}()
	}
	readers := make(chan error, 4)
	for range 4 {
		go func() {
			for {
				select {
				case <-done:
					readers <- nil
					return
				default:
				}
				raw, err := os.ReadFile(config)
				if err != nil {
					readers <- err
					return
				}
				var settings map[string]any
				if err := toml.Unmarshal(raw, &settings); err != nil || settings["model"] != "gpt-5.6-codex" || settings["skills"] == nil {
					readers <- fmt.Errorf("partial worker config %q: %w", raw, err)
					return
				}
			}
		}()
	}
	for range 4 {
		if err := <-writers; err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	for range 4 {
		if err := <-readers; err != nil {
			t.Fatal(err)
		}
	}
}

func writeUserSkillFixture(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: fixture\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
