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
		ConfigPath: "/Users/name & operator/.config/detent-runner/global.yaml",
		Path:       "/Users/name/bin:/usr/bin",
	})
	for _, want := range []string{
		"<string>/Applications/Detent &amp; Tools/detent</string>",
		"<string>--config</string>",
		"<string>/Users/name/Library/Application Support/Detent/global.yaml</string>",
		"<key>StandardOutPath</key>",
		"<string>/Users/name &amp; operator/.config/detent-runner/logs/service.out.log</string>",
		"<key>StandardErrorPath</key>",
		"<string>/Users/name &amp; operator/.config/detent-runner/logs/service.err.log</string>",
		"<key>ThrottleInterval</key>",
		"<integer>60</integer>",
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

	path := filepath.Join(t.TempDir(), LaunchdLabel(DefaultName)+".plist")
	if err := os.WriteFile(path, []byte("plist"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var commands [][]string
	loaded := false
	stopped := false
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
			if len(args) > 0 && args[0] == "kill" {
				if !reflect.DeepEqual(args, []string{"kill", "SIGTERM", "gui/501/" + LaunchdLabel(DefaultName)}) {
					t.Errorf("signal command = %v", args)
				}
				stopped = true
			}
			if len(args) > 0 && args[0] == "print" {
				if stopped {
					return "state = exited\n", nil
				}
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
	wantRestart := []string{"launchctl", "kickstart", "gui/501/" + LaunchdLabel(DefaultName)}
	if len(commands) == 0 {
		t.Fatal("commands are empty after restart")
	}
	if got := commands[len(commands)-1]; !reflect.DeepEqual(got, wantRestart) {
		t.Fatalf("restart command = %#v, want %#v", got, wantRestart)
	}
}

func TestLaunchdInstallWritesDefinition(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "LaunchAgents", LaunchdLabel(DefaultName)+".plist")
	cfg := normalizeConfig(Config{
		UID:              "501",
		LaunchdPlistPath: path,
		BinaryPath:       "/usr/local/bin/detent",
		ConfigPath:       filepath.Join(t.TempDir(), "global.yaml"),
	})
	manager := newLaunchdManager(cfg)
	definition, err := manager.Install(t.Context())
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	for _, logPath := range []string{definition.StandardOutPath, definition.StandardErrorPath} {
		info, err := os.Stat(logPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("service log %s: info=%v err=%v", logPath, info, err)
		}
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
			if name != "launchctl" || !reflect.DeepEqual(args, []string{"print", "gui/501/" + LaunchdLabel(DefaultName)}) {
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

func TestLaunchdBootstrapFailureDiagnosesDisabledLabel(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, disabled string
		queryErr       bool
		wantHint       bool
	}{
		{name: "runner disabled", disabled: "disabled services = {\n\t\"com.digitaldrywood.detent.runner\" => disabled\n}", wantHint: true},
		{name: "board disabled only", disabled: "disabled services = {\n\t\"com.digitaldrywood.detent\" => disabled\n}"},
		{name: "runner enabled", disabled: "disabled services = {\n\t\"com.digitaldrywood.detent.runner\" => enabled\n}"},
		{name: "query fails", queryErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bootstrapErr := errors.New("Bootstrap failed: 5: Input/output error")
			manager := newLaunchdManager(normalizeConfig(Config{
				Name: "detent.runner", UID: "501", HomeDir: t.TempDir(),
				RunCommand: func(_ context.Context, name string, args ...string) (string, error) {
					switch args[0] {
					case "print":
						return "Could not find service", errors.New("exit status 113")
					case "bootstrap":
						return "", bootstrapErr
					case "print-disabled":
						if !reflect.DeepEqual(args, []string{"print-disabled", "gui/501"}) {
							t.Fatalf("command = %s %v", name, args)
						}
						if test.queryErr {
							return "", errors.New("query failed")
						}
						return test.disabled, nil
					default:
						t.Fatalf("unexpected command = %s %v", name, args)
						return "", nil
					}
				},
			}))
			err := manager.Start(t.Context())
			if !errors.Is(err, bootstrapErr) {
				t.Fatalf("err = %v; lost bootstrap failure", err)
			}
			if got := strings.Contains(err.Error(), "launchctl enable gui/501/com.digitaldrywood.detent.runner"); got != test.wantHint {
				t.Fatalf("err = %v, want disabled hint %v", err, test.wantHint)
			}
		})
	}
}
