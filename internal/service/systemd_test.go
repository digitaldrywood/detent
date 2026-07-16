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

func TestSystemdUnitIncludesRuntimeAndShutdownSettings(t *testing.T) {
	t.Parallel()

	unit := systemdUnit(Config{
		BinaryPath: "/opt/Detent Release/detent",
		Arguments:  []string{"--config", "/home/user/.config/detent/global.yaml", "--port", "4100", "--headless"},
		Path:       `/home/user/bin:/usr/bin:%h/bin`,
	})
	for _, want := range []string{
		`ExecStart="/opt/Detent Release/detent" "--config" "/home/user/.config/detent/global.yaml" "--port" "4100" "--headless"`,
		`Environment="PATH=/home/user/bin:/usr/bin:%%h/bin"`,
		"Restart=on-failure",
		"KillMode=control-group",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdInspectAndCommands(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	userPath := filepath.Join(root, "detent.service")
	if err := os.WriteFile(userPath, []byte("unit"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var commands [][]string
	cfg := normalizeConfig(Config{
		UserSystemdPath: userPath,
		RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
			commands = append(commands, append([]string{name}, args...))
			switch name {
			case "systemctl":
				return "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\n", nil
			case "ps":
				return "Thu Jul 16 10:30:00 2026\n", nil
			default:
				return "", errors.New("unexpected command")
			}
		},
	})
	manager := newSystemdManager(cfg)
	inspection, err := manager.Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Present || inspection.State != StateRunning || inspection.PID != 42 || inspection.StartedAt.IsZero() {
		t.Fatalf("inspection = %#v", inspection)
	}
	if len(commands) != 2 || !reflect.DeepEqual(commands[0][:3], []string{"systemctl", "--user", "show"}) {
		t.Fatalf("commands = %#v", commands)
	}
	if err := manager.Restart(t.Context()); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if got := commands[len(commands)-1]; !reflect.DeepEqual(got, []string{"systemctl", "--user", "restart", systemdUnitName}) {
		t.Fatalf("restart command = %#v", got)
	}
}

func TestSystemdUsesSystemScopeForSystemUnit(t *testing.T) {
	t.Parallel()

	systemPath := filepath.Join(t.TempDir(), "detent.service")
	if err := os.WriteFile(systemPath, []byte("unit"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var command []string
	cfg := normalizeConfig(Config{
		UserSystemdPath: filepath.Join(t.TempDir(), "missing.service"),
		SystemUnitPaths: []string{systemPath},
		RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
			command = append([]string{name}, args...)
			return "LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\n", nil
		},
	})
	manager := newSystemdManager(cfg)
	inspection, err := manager.Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Present || inspection.State != StateStopped || manager.Info().Scope != "system" || manager.Info().DefinitionPath != systemPath {
		t.Fatalf("inspection/info = %#v %#v", inspection, manager.Info())
	}
	if len(command) < 2 || command[1] != "show" {
		t.Fatalf("command = %#v, want system scope without --user", command)
	}
}

func TestSystemdDetectsLoadedUnitWithoutDefinitionFile(t *testing.T) {
	t.Parallel()

	var commands [][]string
	cfg := normalizeConfig(Config{
		HomeDir:         t.TempDir(),
		UserSystemdPath: filepath.Join(t.TempDir(), "missing.service"),
		SystemUnitPaths: []string{filepath.Join(t.TempDir(), "missing-system.service")},
		RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
			commands = append(commands, append([]string{name}, args...))
			return "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=0\n", nil
		},
	})
	manager := newSystemdManager(cfg)
	inspection, err := manager.Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Present || inspection.State != StateRunning || manager.Info().Scope != "user" || manager.Info().DefinitionPath != "" {
		t.Fatalf("inspection/info = %#v %#v", inspection, manager.Info())
	}
	if len(commands) != 1 || !reflect.DeepEqual(commands[0][:3], []string{"systemctl", "--user", "show"}) {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestSystemdInstallWritesEnablesAndStarts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "systemd", systemdUnitName)
	var commands [][]string
	cfg := normalizeConfig(Config{
		UserSystemdPath: path,
		BinaryPath:      "/usr/local/bin/detent",
		ConfigPath:      "/home/user/.config/detent/global.yaml",
		RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
			commands = append(commands, append([]string{name}, args...))
			return "", nil
		},
	})
	manager := newSystemdManager(cfg)
	definition, err := manager.Install(t.Context())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if definition.Path != path {
		t.Fatalf("definition path = %q, want %q", definition.Path, path)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	wantCommands := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", systemdUnitName},
		{"systemctl", "--user", "start", systemdUnitName},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
}

func TestSystemdState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		active string
		sub    string
		want   State
	}{
		{active: "active", sub: "running", want: StateRunning},
		{active: "activating", sub: "start", want: StateStarting},
		{active: "deactivating", sub: "stop-sigterm", want: StateStopping},
		{active: "inactive", sub: "dead", want: StateStopped},
		{active: "unexpected", sub: "unexpected", want: StateUnknown},
	}
	for _, tt := range tests {
		if got := systemdState(tt.active, tt.sub); got != tt.want {
			t.Errorf("systemdState(%q, %q) = %q, want %q", tt.active, tt.sub, got, tt.want)
		}
	}
}
