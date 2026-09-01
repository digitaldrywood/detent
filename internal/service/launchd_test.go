package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLaunchdPlistIncludesRuntimeSettings(t *testing.T) {
	t.Parallel()

	plist := launchdPlist(Config{
		BinaryPath: "/Applications/Detent & Tools/detent",
		Arguments:  []string{"--config", "/Users/name/Library/Application Support/Detent/global.yaml", "--headless"},
		HomeDir:    "/Users/name & operator",
		Path:       "/Users/name/bin:/usr/bin",
	})
	for _, want := range []string{
		"<string>/Applications/Detent &amp; Tools/detent</string>",
		"<string>--config</string>",
		"<string>/Users/name/Library/Application Support/Detent/global.yaml</string>",
		"<key>RunAtLoad</key>",
		"<key>SuccessfulExit</key>",
		"<string>/Users/name/bin:/usr/bin</string>",
		"<key>WorkingDirectory</key>",
		"<string>/Users/name &amp; operator</string>",
		"<key>DETENT_SERVICE_MANAGER</key>",
		"<string>launchd</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestLaunchdInspectStartAndRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), launchdLabel+".plist")
	if err := os.WriteFile(path, []byte("plist"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var commands [][]string
	loaded := false
	cfg := normalizeConfig(Config{
		UID:              "501",
		LaunchdPlistPath: path,
		RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
			commands = append(commands, append([]string{name}, args...))
			if name == "ps" {
				return "Thu Jul 16 10:30:00 2026\n", nil
			}
			if len(args) > 0 && args[0] == "bootstrap" {
				loaded = true
				return "", nil
			}
			if len(args) > 0 && args[0] == "print" {
				if !loaded {
					return "Could not find service", errors.New("exit status 113")
				}
				return "state = running\npid = 42\n", nil
			}
			return "", nil
		},
	})
	manager := newLaunchdManager(cfg)
	inspection, err := manager.Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Present || inspection.State != StateStopped {
		t.Fatalf("inspection = %#v, want present stopped", inspection)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !loaded {
		t.Fatal("bootstrap was not called")
	}
	inspection, err = manager.Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() running error = %v", err)
	}
	if inspection.State != StateRunning || inspection.PID != 42 || inspection.StartedAt.IsZero() {
		t.Fatalf("running inspection = %#v", inspection)
	}
	if err := manager.Restart(t.Context()); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	wantRestart := []string{"launchctl", "kickstart", "-k", "gui/501/" + launchdLabel}
	if len(commands) == 0 {
		t.Fatal("commands are empty after restart")
	}
	if got := commands[len(commands)-1]; !reflect.DeepEqual(got, wantRestart) {
		t.Fatalf("restart command = %#v, want %#v", got, wantRestart)
	}
}

func TestLaunchdInstallWritesDefinition(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "LaunchAgents", launchdLabel+".plist")
	cfg := normalizeConfig(Config{
		UID:              "501",
		LaunchdPlistPath: path,
		BinaryPath:       "/usr/local/bin/detent",
		ConfigPath:       "/Users/name/.config/detent/global.yaml",
	})
	manager := newLaunchdManager(cfg)
	definition, err := manager.Install(t.Context())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if definition.Path != path || string(raw) != definition.Content {
		t.Fatalf("definition = %#v, contents = %q", definition, raw)
	}
}

func TestLaunchdDetectsLoadedJobWithoutDefinitionFile(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{
		UID:              "501",
		LaunchdPlistPath: filepath.Join(t.TempDir(), "missing.plist"),
		RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
			if name != "launchctl" || !reflect.DeepEqual(args, []string{"print", "gui/501/" + launchdLabel}) {
				t.Fatalf("command = %s %#v", name, args)
			}
			return "state = running\npid = 0\n", nil
		},
	})
	inspection, err := newLaunchdManager(cfg).Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Present || inspection.State != StateRunning {
		t.Fatalf("inspection = %#v", inspection)
	}
}

func TestLaunchdState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		output string
		want   State
	}{
		{output: "state = running\npid = 42", want: StateRunning},
		{output: "state = waiting", want: StateStarting},
		{output: "state = terminating", want: StateStopping},
		{output: "state = exited", want: StateStopped},
		{output: "pid = 42", want: StateRunning},
		{output: "", want: StateStopped},
	}
	for _, tt := range tests {
		if got := launchdState(tt.output); got != tt.want {
			t.Errorf("launchdState(%q) = %q, want %q", tt.output, got, tt.want)
		}
	}
}
