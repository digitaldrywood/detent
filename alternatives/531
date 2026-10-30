package cli

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestHubRunnerRegisterRequiresServiceForDedicatedAccount(t *testing.T) {
	t.Parallel()
	command := newHubRunnerRegisterCommand("test", func(string) string { return "" }, nil)
	_, err := executeRegister(t, command, "--service-user", "runner")
	if err == nil || !strings.Contains(err.Error(), "--service-user requires --service") {
		t.Fatalf("dedicated account validation = %v", err)
	}
}

func TestHubRunnerRegisterDelegatesBeforeEnrollment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		flags   []string
		wantErr string
	}{
		{name: "dedicated account", flags: []string{"--service", "--service-user", "runner"}},
		{name: "stdin token", flags: []string{"--service", "--service-user", "runner", "--enrollment-token-stdin"}},
		{name: "conflicting token sources", flags: []string{"--service", "--service-user", "runner", "--enrollment-token-stdin"}, wantErr: "cannot be combined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			called := false
			started := false
			hub := newRegisterHub(t, nil)
			root := t.TempDir()
			configPath := filepath.Join(root, "config", "global.yaml")
			command := newHubRunnerRegisterCommandWithUser("test", "linux", func(string) string { return "" }, nil, func(string) error {
				t.Fatal("operator account prepared enrollment paths")
				return nil
			}, func(context.Context, globalconfig.Config, string) error { return nil }, nil, nil, workspace.ProbeCommitReport, func(cmd *cobra.Command, account, token string, args []string) error {
				called = true
				if account != "runner" || token != "det_enroll_example" {
					t.Fatal("delegation lost account or enrollment token")
				}
				if !slices.Contains(args, "--service=true") || !slices.Contains(args, "--config="+configPath) || !slices.Contains(args, "--enrollment-token-stdin") {
					t.Fatalf("delegation arguments = %v", args)
				}
				for _, arg := range args {
					if strings.Contains(arg, "det_enroll_example") || strings.HasPrefix(arg, "--service-user") || strings.HasPrefix(arg, "--token") {
						t.Fatal("delegation exposed token or retained account switch")
					}
				}
				child := newHubRunnerRegisterCommandWithServiceName("test", "linux", func(string) string { return "" }, func(_ *cobra.Command, path string) error {
					started = true
					if path != configPath {
						t.Fatalf("service config = %s", path)
					}
					cfg, err := readRunnerSetupConfig(path)
					if err != nil || cfg.ServiceName != runnerServiceName {
						t.Fatalf("service configuration = %+v, %v", cfg, err)
					}
					return nil
				}, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil }, func(context.Context) error { return nil }, testRunnerServiceName)
				child.SetIn(strings.NewReader(token))
				output, err := executeRegister(t, child, args[3:]...)
				if err != nil {
					t.Log(output)
				}
				return err
			})
			args := []string{"--url", hub.server.URL + "/organizations/org_example", "--config", configPath, "--workspace-root", filepath.Join(root, "work"), "--name", "Build host", "--capacity", "2"}
			if test.name == "stdin token" {
				command.SetIn(strings.NewReader("det_enroll_example\n"))
			} else {
				args = append(args, "--token", "det_enroll_example")
			}
			_, err := executeRegister(t, command, append(args, test.flags...)...)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || called {
					t.Fatalf("registration error = %v, delegated = %v", err, called)
				}
			} else if err != nil || !called || !started || hub.redeemed.Load() != 1 {
				t.Fatalf("registration error = %v, delegated = %v, started = %v, redemptions = %d", err, called, started, hub.redeemed.Load())
			}
		})
	}
}

func TestRunnerServiceUserDropsOperatorEnvironment(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			account := &user.User{Username: "runner", Uid: "502", HomeDir: "/home/runner"}
			command, err := runnerServiceUserCommand(t.Context(), goos, account, "/usr/local/bin/detent", []string{"hub", "runner", "register", "--service", "--enrollment-token-stdin"})
			if goos == "windows" {
				if err == nil {
					t.Fatal("unsupported account switching accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if command.Path != "/usr/bin/sudo" || !slices.Equal(command.Args[1:7], []string{"-n", "-H", "-u", "#502", "--", "/usr/bin/env"}) || command.Args[7] != "-i" || command.Dir != "/" {
				t.Fatalf("account command = %v, directory = %q", command.Args, command.Dir)
			}
			if !slices.Contains(command.Args, "HOME=/home/runner") || !slices.Contains(command.Args, "USER=runner") || !slices.Contains(command.Args, "/usr/local/bin/detent") {
				t.Fatalf("missing account context: %v", command.Args)
			}
			if goos == "linux" && (!slices.Contains(command.Args, "XDG_RUNTIME_DIR=/run/user/502") || !slices.Contains(command.Args, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/502/bus")) {
				t.Fatalf("missing user service manager: %v", command.Args)
			}
			if len(command.Env) != 1 || !strings.HasPrefix(command.Env[0], "PATH=") {
				t.Fatalf("sudo inherited operator environment: %v", command.Env)
			}
			if goos == "darwin" && runtime.GOOS == "darwin" || goos == "linux" && runtime.GOOS == "linux" {
				binaryIndex := slices.Index(command.Args, "/usr/local/bin/detent")
				child := exec.CommandContext(t.Context(), "/usr/bin/env", append(slices.Clone(command.Args[7:binaryIndex]), "/usr/bin/env")...)
				child.Env = []string{"CODEX_HOME=/operator/.codex", "GITHUB_TOKEN=operator-fixture", "XDG_CONFIG_HOME=/operator/.config", "npm_config_cache=/operator/npm"}
				output, err := child.Output()
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(output), "operator") || !strings.Contains(string(output), "HOME=/home/runner\n") {
					t.Fatalf("target process environment = %s", output)
				}
			}
		})
	}
}

func TestRunnerServiceUserRejectsInvalidAccount(t *testing.T) {
	t.Parallel()
	for _, account := range []*user.User{
		{Username: "root", Uid: "0", HomeDir: "/root"},
		{Username: "runner", Uid: "invalid", HomeDir: "/home/runner"},
		{Username: "runner", Uid: "502", HomeDir: "relative"},
	} {
		if _, err := runnerServiceUserCommand(t.Context(), "linux", account, "/usr/local/bin/detent", nil); err == nil {
			t.Fatalf("invalid account accepted: %+v", account)
		}
	}
}

func TestDedicatedRunnerInventoryExcludesOperatorHome(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			operatorHome, runnerHome := t.TempDir(), t.TempDir()
			for _, dir := range []string{
				filepath.Join(operatorHome, ".agents", "skills", "operator"),
				filepath.Join(operatorHome, ".codex", "skills", "operator"),
			} {
				writeUserSkillFixture(t, dir)
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: operator\n---\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(operatorHome, ".codex", "config.toml"), []byte("[mcp_servers.operator]\ncommand = 'operator-mcp'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			account := &user.User{Username: "runner", Uid: "502", HomeDir: runnerHome}
			command, err := runnerServiceUserCommand(t.Context(), goos, account, "/usr/local/bin/detent", nil)
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, arg := range command.Args[8 : len(command.Args)-1] {
				key, value, ok := strings.Cut(arg, "=")
				if ok {
					env[key] = value
				}
			}
			var profile string
			for range 2 {
				worktree := t.TempDir()
				repositorySkill := filepath.Join(worktree, ".agents", "skills", "repository")
				writeUserSkillFixture(t, repositorySkill)
				operatorInventory, err := skills.DiscoverProviderSkills(worktree, operatorHome)
				if err != nil || len(operatorInventory) != 2 {
					t.Fatalf("operator fixture inventory = %+v, %v", operatorInventory, err)
				}
				inventory, err := skills.DiscoverProviderSkills(worktree, env["HOME"])
				if err != nil || len(inventory) != 1 || inventory[0].Path != filepath.Join(repositorySkill, "SKILL.md") || !inventory[0].Enabled {
					t.Fatalf("dedicated account inventory = %+v, %v", inventory, err)
				}
				prepared, err := prepareCodexCommandForService("codex app-server", goos, "", func(key string) (string, bool) { value, ok := env[key]; return value, ok }, func() (string, error) { return env["HOME"], nil })
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(prepared.Environment["CODEX_HOME"], "config.toml"))
				if err != nil {
					t.Fatal(err)
				}
				var settings map[string]any
				if err := toml.Unmarshal(raw, &settings); err != nil {
					t.Fatal(err)
				}
				if _, ok := settings["mcp_servers"]; ok || strings.Contains(string(raw), operatorHome) {
					t.Fatal("operator instructions reached worker configuration")
				}
				if len(prepared.Environment) != 1 || prepared.Environment["CODEX_HOME"] == "" || !strings.HasPrefix(prepared.Environment["CODEX_HOME"], runnerHome+string(os.PathSeparator)) {
					t.Fatalf("worker changed native homes or caches: %v", prepared.Environment)
				}
				if profile != "" && profile != prepared.Environment["CODEX_HOME"] {
					t.Fatal("worker profile home changed between runs")
				}
				profile = prepared.Environment["CODEX_HOME"]
			}
		})
	}
}
