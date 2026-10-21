package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestScheduledTaskDefinition(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, binary, config, home, wantCommand string
	}{
		{"spaces and backslashes", `C:\Program Files\Detent\detent.exe`, `C:\Users\Example User\runner\global.yaml`, `C:\Users\Example User`, `& 'C:\Program Files\Detent\detent.exe' '--config' 'C:\Users\Example User\runner\global.yaml' '--headless'`},
		{"shell metacharacters and unicode", `C:\Tools & Apps\detent.exe`, `C:\Users\Example's $user\配置\global.yaml`, `C:\Users\Example's $user`, `& 'C:\Tools & Apps\detent.exe' '--config' 'C:\Users\Example''s $user\配置\global.yaml' '--headless'`},
		{"unicode quote injection", `C:\Tools‘\detent.exe`, `C:\Users\x’; Start-Process calc; #‚‛\global.yaml`, `C:\Users\Example`, `& 'C:\Tools‘‘\detent.exe' '--config' 'C:\Users\x’’; Start-Process calc; #‚‚‛‛\global.yaml' '--headless'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{GOOS: "windows", Name: "detent.runner", UID: "S-1-5-21-1234", BinaryPath: test.binary, ConfigPath: test.config, HomeDir: test.home, Path: `C:\Tools;C:\Program Files\Provider`})
			manager := newScheduledTaskManager(cfg)
			var task struct {
				XMLName xml.Name
				Trigger struct {
					Enabled bool
					UserID  string `xml:"UserId"`
				} `xml:"Triggers>LogonTrigger"`
				Principal struct {
					ID                  string `xml:"id,attr"`
					UserID              string `xml:"UserId"`
					LogonType, RunLevel string
				} `xml:"Principals>Principal"`
				Settings struct {
					MultipleInstancesPolicy                            string
					DisallowStartIfOnBatteries, StopIfGoingOnBatteries string
					StartWhenAvailable                                 bool
					ExecutionTimeLimit                                 string
					Restart                                            struct {
						Interval string
						Count    int
					} `xml:"RestartOnFailure"`
				} `xml:"Settings"`
				Actions struct {
					Context string `xml:"Context,attr"`
					Exec    struct{ Command, Arguments, WorkingDirectory string }
				} `xml:"Actions"`
			}
			definition := manager.Definition()
			if err := xml.Unmarshal([]byte(definition.Content), &task); err != nil {
				t.Fatal(err)
			}
			if task.XMLName.Space != "http://schemas.microsoft.com/windows/2004/02/mit/task" || !task.Trigger.Enabled || task.Trigger.UserID != "S-1-5-21-1234" || task.Principal.UserID != "S-1-5-21-1234" || task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" || task.Actions.Context != task.Principal.ID {
				t.Fatalf("incorrect task trigger or principal: %+v", task)
			}
			settings := task.Settings
			if settings.MultipleInstancesPolicy != "IgnoreNew" || settings.DisallowStartIfOnBatteries != "false" || settings.StopIfGoingOnBatteries != "false" || !settings.StartWhenAvailable || settings.ExecutionTimeLimit != "PT0S" || settings.Restart.Interval != "PT1M" || settings.Restart.Count != 999 {
				t.Fatalf("incorrect task settings: %+v", settings)
			}
			if task.Actions.Exec.Command != "powershell.exe" || task.Actions.Exec.WorkingDirectory != test.home {
				t.Fatalf("incorrect task action: %+v", task.Actions.Exec)
			}
			prefix := "-NoProfile -NonInteractive -EncodedCommand "
			if !strings.HasPrefix(task.Actions.Exec.Arguments, prefix) {
				t.Fatal(task.Actions.Exec.Arguments)
			}
			script := decodePowerShell(t, strings.TrimPrefix(task.Actions.Exec.Arguments, prefix))
			for _, want := range []string{test.wantCommand, `$env:DETENT_SERVICE_MANAGER = 'scheduled-task'`, `$env:DETENT_SERVICE_TASK = 'detent.runner-S-1-5-21-1234'`, `$env:PATH = 'C:\Tools;C:\Program Files\Provider'`, `exit $LASTEXITCODE`} {
				if !strings.Contains(script, want) {
					t.Errorf("script %q missing %q", script, want)
				}
			}
			if manager.Info().Unit != "detent.runner-S-1-5-21-1234" || manager.Info().Scope != "user" {
				t.Fatal(manager.Info())
			}
		})
	}
}

func TestScheduledTaskInspection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		output     string
		commandErr error
		present    bool
		state      State
		wantErr    bool
	}{
		{"missing\r\n", nil, false, StateStopped, false},
		{"1", nil, true, StateStopped, false},
		{"2", nil, true, StateStarting, false},
		{"3", nil, true, StateStopped, false},
		{"4\r\n", nil, true, StateRunning, false},
		{"0", nil, true, StateUnknown, false},
		{"garbled", nil, false, "", true},
		{"", errors.New("access denied"), false, "", true},
	} {
		t.Run(test.output+"/"+string(test.state), func(t *testing.T) {
			t.Parallel()
			manager := newScheduledTaskManager(normalizeConfig(Config{GOOS: "windows", RunCommand: func(context.Context, string, ...string) (string, error) { return test.output, test.commandErr }}))
			got, err := manager.Inspect(t.Context())
			if (err != nil) != test.wantErr || got.Present != test.present || got.State != test.state {
				t.Fatalf("Inspect = %+v, %v", got, err)
			}
		})
	}
}

func TestScheduledTaskLifecycle(t *testing.T) {
	t.Parallel()
	for _, failInstall := range []bool{false, true} {
		t.Run(boolName(failInstall), func(t *testing.T) {
			t.Parallel()
			var calls []string
			cfg := normalizeConfig(Config{GOOS: "windows", Name: "detent.runner", UID: "S-1-5-21-1234", HomeDir: t.TempDir()})
			cfg.ConfigPath = filepath.Join(cfg.HomeDir, "global.yaml")
			cfg.RunCommand = func(_ context.Context, command string, args ...string) (string, error) {
				if command != "powershell.exe" || len(args) != 4 || !reflect.DeepEqual(args[:3], []string{"-NoProfile", "-NonInteractive", "-EncodedCommand"}) {
					t.Fatalf("unexpected command %s %v", command, args)
				}
				script := decodePowerShell(t, args[3])
				switch {
				case strings.Contains(script, ".RegisterTask("):
					calls = append(calls, "install")
					if failInstall {
						return "", errors.New("registration denied")
					}
					if !strings.Contains(script, ", 2, 'S-1-5-21-1234', $null, 3, $null)") {
						t.Fatal(script)
					}
				case strings.Contains(script, ".Run($null)"):
					calls = append(calls, "start")
				case strings.Contains(script, ".Stop(0)"):
					calls = append(calls, "stop")
				case strings.Contains(script, ".DeleteTask("):
					calls = append(calls, "delete")
				default:
					return "3", nil
				}
				return "", nil
			}
			controller, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			manager, ok := controller.manager.(*scheduledTaskManager)
			if !ok {
				t.Fatalf("windows manager = %T", controller.manager)
			}
			definition, err := manager.Install(t.Context())
			if failInstall {
				if err == nil {
					t.Fatal("install succeeded")
				}
				if _, err := os.Stat(manager.Definition().Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed install left definition: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{definition.Path, definition.StandardOutPath, definition.StandardErrorPath} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := manager.Restart(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := manager.Uninstall(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"install", "start", "stop", "start", "stop", "delete"}) {
				t.Fatal(calls)
			}
			if _, err := os.Stat(definition.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("uninstall left definition: %v", err)
			}
		})
	}
}

func TestScheduledTaskInstallWithStaleDefinition(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"missing", "3", "access denied"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			cfg := normalizeConfig(Config{GOOS: "windows", UID: "S-1-5-21-1234", ConfigPath: filepath.Join(root, "global.yaml"), HomeDir: root})
			registrations := 0
			cfg.RunCommand = func(_ context.Context, _ string, args ...string) (string, error) {
				script := decodePowerShell(t, args[3])
				if strings.Contains(script, ".RegisterTask(") {
					registrations++
					return "", nil
				}
				if state == "access denied" {
					return "", errors.New("access denied")
				}
				return state, nil
			}
			manager := newScheduledTaskManager(cfg)
			definition := manager.Definition()
			if err := os.WriteFile(definition.Path, []byte("stale definition"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := manager.Install(t.Context())
			if state == "missing" {
				if err != nil || registrations != 1 {
					t.Fatalf("repair install: registrations=%d error=%v", registrations, err)
				}
				data, err := os.ReadFile(definition.Path)
				if err != nil || !strings.Contains(string(data), "<LogonTrigger>") {
					t.Fatalf("replacement definition: %s, %v", data, err)
				}
			} else {
				if err == nil || registrations != 0 {
					t.Fatalf("replaced task without proving absence: registrations=%d error=%v", registrations, err)
				}
				data, err := os.ReadFile(definition.Path)
				if err != nil || string(data) != "stale definition" {
					t.Fatalf("existing definition changed: %s, %v", data, err)
				}
			}
		})
	}
}

func decodePowerShell(t *testing.T, encoded string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(data)%2 != 0 {
		t.Fatal("odd UTF-16 byte count")
	}
	words := make([]uint16, len(data)/2)
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return string(utf16.Decode(words))
}
