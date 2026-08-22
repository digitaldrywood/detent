package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/update"
)

func TestStartCommandOffersAndInstallsService(t *testing.T) {
	t.Parallel()

	runner := &serviceRunnerStub{
		startResults: []servicepkg.StartResult{
			{
				Action:  servicepkg.ActionNeedsInstall,
				Manager: servicepkg.ManagerInfo{Name: servicepkg.ManagerSystemd, Scope: "user", Unit: "detent.service"},
				State:   servicepkg.StateStopped,
				Definition: &servicepkg.Definition{
					Path:    "/home/user/.config/systemd/user/detent.service",
					Content: "[Unit]\nDescription=Detent\n",
				},
			},
			{
				Action:  servicepkg.ActionInstalled,
				Manager: servicepkg.ManagerInfo{Name: servicepkg.ManagerSystemd, Scope: "user", Unit: "detent.service"},
				State:   servicepkg.StateRunning,
				Definition: &servicepkg.Definition{
					Path:    "/home/user/.config/systemd/user/detent.service",
					Content: "[Unit]\nDescription=Detent\n",
				},
			},
		},
	}
	cmd := NewRootCommand(t.Context(), WithVersion("v1.2.3"), WithServiceFactory(serviceFactoryFor(runner)))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetArgs([]string{"--format", "pretty", "start"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(runner.startOptions) != 2 || runner.startOptions[0].Install || !runner.startOptions[1].Install {
		t.Fatalf("start options = %#v", runner.startOptions)
	}
	for _, want := range []string{
		"Install the systemd user service at /home/user/.config/systemd/user/detent.service? [y/N]",
		"Wrote /home/user/.config/systemd/user/detent.service:\n[Unit]\nDescription=Detent\n",
		"Started detent.service via systemd (user).",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStartCommandYesInstallsWithoutPrompt(t *testing.T) {
	t.Parallel()

	runner := &serviceRunnerStub{startResults: []servicepkg.StartResult{{
		Action:  servicepkg.ActionInstalled,
		Manager: servicepkg.ManagerInfo{Name: servicepkg.ManagerLaunchd, Scope: "user", Unit: "com.digitaldrywood.detent"},
		State:   servicepkg.StateRunning,
		Definition: &servicepkg.Definition{
			Path:    "/Users/name/Library/LaunchAgents/com.digitaldrywood.detent.plist",
			Content: "<plist/>\n",
		},
	}}}
	cmd := NewRootCommand(t.Context(), WithServiceFactory(serviceFactoryFor(runner)))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "start", "--yes"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(runner.startOptions) != 1 || !runner.startOptions[0].Install {
		t.Fatalf("start options = %#v", runner.startOptions)
	}
	if strings.Contains(stdout.String(), "[y/N]") {
		t.Fatalf("output contains prompt:\n%s", stdout.String())
	}
}

func TestStartCommandJSONRequiresExplicitInstallConfirmation(t *testing.T) {
	t.Parallel()

	runner := &serviceRunnerStub{startResults: []servicepkg.StartResult{{
		Action:     servicepkg.ActionNeedsInstall,
		Manager:    servicepkg.ManagerInfo{Name: servicepkg.ManagerSystemd, Scope: "user", Unit: "detent.service"},
		State:      servicepkg.StateStopped,
		Definition: &servicepkg.Definition{Path: "/tmp/detent.service", Content: "unit\n"},
	}}}
	cmd := NewRootCommand(t.Context(), WithServiceFactory(serviceFactoryFor(runner)))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "start"})

	err := cmd.Execute()
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("Execute() error = %v, want validation", err)
	}
	var result servicepkg.StartResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
	}
	if result.Action != servicepkg.ActionNeedsInstall || len(runner.startOptions) != 1 {
		t.Fatalf("result/options = %#v %#v", result, runner.startOptions)
	}
}

func TestStartCommandReportsAlreadyRunning(t *testing.T) {
	t.Parallel()

	runner := &serviceRunnerStub{startResults: []servicepkg.StartResult{{
		Action:  servicepkg.ActionAlreadyActive,
		Manager: servicepkg.ManagerInfo{Name: servicepkg.ManagerSystemd, Scope: "system", Unit: "detent.service"},
		State:   servicepkg.StateRunning,
		PID:     42,
	}}}
	cmd := NewRootCommand(t.Context(), WithServiceFactory(serviceFactoryFor(runner)))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "start"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, want := range []string{"already running", "systemd (system)", "PID 42", "detent start --restart"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStatusCommandReportsFieldsAndFailsWhenExpectedServiceStopped(t *testing.T) {
	t.Parallel()

	runner := &serviceRunnerStub{status: servicepkg.Status{
		InstallMethod:  update.InstallSourceRelease,
		ServiceManager: servicepkg.ManagerSystemd,
		ServiceScope:   "user",
		Service:        "detent.service",
		DefinitionPath: "/home/user/.config/systemd/user/detent.service",
		Expected:       true,
		State:          servicepkg.StateStopped,
		Version:        "v1.2.3",
		AutoUpdate:     "apply enabled",
		DashboardURL:   "http://localhost:4000",
		ConfigPath:     "/home/user/.config/detent/global.yaml",
	}}
	cmd := NewRootCommand(t.Context(), WithServiceFactory(serviceFactoryFor(runner)))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "status"})

	err := cmd.Execute()
	if !errors.Is(err, servicepkg.ErrNotRunning) {
		t.Fatalf("Execute() error = %v, want %v", err, servicepkg.ErrNotRunning)
	}
	for _, want := range []string{
		"Install method: release",
		"Service manager: systemd (user)",
		"Service: detent.service",
		"State: stopped",
		"Version: v1.2.3",
		"Auto-update: apply enabled",
		"Dashboard: http://localhost:4000",
		"Definition: /home/user/.config/systemd/user/detent.service",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStatusCommandManualStoppedIsSuccessful(t *testing.T) {
	t.Parallel()

	runner := &serviceRunnerStub{status: servicepkg.Status{
		InstallMethod:  update.InstallSourceDevelopment,
		ServiceManager: servicepkg.ManagerManual,
		Service:        string(servicepkg.ManagerManual),
		State:          servicepkg.StateStopped,
		Version:        "dev",
		AutoUpdate:     "disabled",
		DashboardURL:   "http://localhost:4000",
		ConfigPath:     "/tmp/global.yaml",
	}}
	cmd := NewRootCommand(t.Context(), WithServiceFactory(serviceFactoryFor(runner)))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "status"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var status servicepkg.Status
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
	}
	if status.ServiceManager != servicepkg.ManagerManual || status.Expected {
		t.Fatalf("status = %#v", status)
	}
}

func TestStoppedManagedServiceUsesNonzeroExitCode(t *testing.T) {
	t.Parallel()

	if got := ExitCode(servicepkg.ErrNotRunning); got == ExitSuccess {
		t.Fatalf("ExitCode() = %d, want nonzero", got)
	}
}

func TestSystemdDefinitionArgumentsDecodesLiteralPercents(t *testing.T) {
	t.Parallel()

	content := []byte(`ExecStart="/opt/detent%%worker" --config "/config/tenant%%2/global.yaml" --host "fe80::1%%eth0"`)
	want := []string{"/opt/detent%worker", "--config", "/config/tenant%2/global.yaml", "--host", "fe80::1%eth0"}

	if got := systemdDefinitionArguments(content); !reflect.DeepEqual(got, want) {
		t.Fatalf("systemdDefinitionArguments() = %#v, want %#v", got, want)
	}
}

func TestServiceCommandResolvesConfigAndRuntime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	cfg, err := globalconfig.DefaultAt(path)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	port := 4100
	cfg.Port = &port
	cfg.Update.AutoApplyEnabled = true
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	var captured servicepkg.Config
	runner := &serviceRunnerStub{status: servicepkg.Status{ServiceManager: servicepkg.ManagerManual, Service: string(servicepkg.ManagerManual), State: servicepkg.StateStopped}}
	cmd := NewRootCommand(t.Context(), WithVersion("dev"), WithServiceFactory(func(cfg servicepkg.Config) (ServiceRunner, error) {
		captured = cfg
		return runner, nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", path, "status"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if captured.ConfigPath != path || captured.AutoUpdate != "apply enabled" || captured.DashboardURL != "http://localhost:4100" || captured.Install.Source != update.InstallSourceDevelopment {
		t.Fatalf("captured config = %#v", captured)
	}
	if want := []string{"--config", path, "--headless"}; !reflect.DeepEqual(captured.Arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", captured.Arguments, want)
	}
}

func TestServiceCommandsLoadLegacyWorkflowPort(t *testing.T) {
	t.Setenv("PORT", "")

	root := t.TempDir()
	path := filepath.Join(root, "global.yaml")
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	writeRuntimeWorkflow(t, workflowPath, 4109)

	cfg, err := globalconfig.DefaultAt(path)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	cfg.Projects = []globalconfig.Project{{
		ID:       "legacy",
		Workflow: workflowPath,
		Workdir:  root,
		Weight:   1,
	}}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	tests := []struct {
		name        string
		args        []string
		startResult servicepkg.StartResult
		wantRestart bool
	}{
		{
			name: "status",
			args: []string{"--format", "json", "--config", path, "status"},
		},
		{
			name: "start with restart",
			args: []string{"--format", "json", "--config", path, "start", "--restart"},
			startResult: servicepkg.StartResult{
				Action:  servicepkg.ActionAlreadyActive,
				Manager: servicepkg.ManagerInfo{Name: servicepkg.ManagerManual, Unit: string(servicepkg.ManagerManual)},
				State:   servicepkg.StateRunning,
			},
			wantRestart: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured servicepkg.Config
			runner := &serviceRunnerStub{
				startResults: []servicepkg.StartResult{tt.startResult},
				status: servicepkg.Status{
					ServiceManager: servicepkg.ManagerManual,
					Service:        string(servicepkg.ManagerManual),
					State:          servicepkg.StateStopped,
				},
			}
			cmd := NewRootCommand(t.Context(), WithServiceFactory(func(cfg servicepkg.Config) (ServiceRunner, error) {
				captured = cfg
				return runner, nil
			}))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if captured.DashboardURL != "http://localhost:4109" {
				t.Fatalf("DashboardURL = %q, want workflow port", captured.DashboardURL)
			}
			if tt.wantRestart {
				if len(runner.startOptions) != 1 || !runner.startOptions[0].Restart {
					t.Fatalf("start options = %#v, want restart", runner.startOptions)
				}
			}
		})
	}
}

func TestStatusCommandResolvesDashboardPort(t *testing.T) {
	t.Setenv("PORT", "4200")

	tests := []struct {
		name             string
		configPort       int
		writeConfig      bool
		definition       string
		manager          servicepkg.ManagerName
		wantDashboardURL string
		wantFactoryURL   string
	}{
		{
			name:             "ambient port with config port",
			configPort:       4100,
			writeConfig:      true,
			manager:          servicepkg.ManagerManual,
			wantDashboardURL: "http://localhost:4100",
			wantFactoryURL:   "http://localhost:4100",
		},
		{
			name:             "installed unit port",
			configPort:       4100,
			writeConfig:      true,
			definition:       "[Service]\nExecStart=\"/usr/bin/detent\" \"--config\" \"/tmp/global.yaml\" \"--port\" \"4300\" \"--headless\"\n",
			manager:          servicepkg.ManagerSystemd,
			wantDashboardURL: "http://localhost:4300",
			wantFactoryURL:   "http://localhost:4100",
		},
		{
			name:        "installed launchd port",
			configPort:  4100,
			writeConfig: true,
			definition: `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.digitaldrywood.detent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/detent</string>
    <string>--config</string>
    <string>/tmp/global.yaml</string>
    <string>--port</string>
    <string>4400</string>
    <string>--headless</string>
  </array>
</dict>
</plist>`,
			manager:          servicepkg.ManagerLaunchd,
			wantDashboardURL: "http://localhost:4400",
			wantFactoryURL:   "http://localhost:4100",
		},
		{
			name:             "default port without config port",
			manager:          servicepkg.ManagerManual,
			wantDashboardURL: "http://localhost:4000",
			wantFactoryURL:   "http://localhost:4000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "global.yaml")
			if tt.writeConfig {
				cfg, err := globalconfig.DefaultAt(path)
				if err != nil {
					t.Fatalf("DefaultAt() error = %v", err)
				}
				cfg.Port = &tt.configPort
				if err := globalconfig.Write(path, cfg); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
			}

			definitionPath := ""
			if tt.definition != "" {
				definitionPath = filepath.Join(root, "detent.service")
				if err := os.WriteFile(definitionPath, []byte(tt.definition), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}

			var captured servicepkg.Config
			runner := &serviceRunnerStub{status: servicepkg.Status{
				ServiceManager: tt.manager,
				Service:        string(tt.manager),
				DefinitionPath: definitionPath,
				State:          servicepkg.StateRunning,
			}}
			cmd := NewRootCommand(t.Context(), WithServiceFactory(func(cfg servicepkg.Config) (ServiceRunner, error) {
				captured = cfg
				return runner, nil
			}))
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--format", "json", "--config", path, "status"})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			var status servicepkg.Status
			if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if status.DashboardURL != tt.wantDashboardURL {
				t.Fatalf("DashboardURL = %q, want %q", status.DashboardURL, tt.wantDashboardURL)
			}
			if captured.DashboardURL != tt.wantFactoryURL {
				t.Fatalf("factory DashboardURL = %q, want %q", captured.DashboardURL, tt.wantFactoryURL)
			}
		})
	}
}

func TestStartCommandPersistsEnvironmentPort(t *testing.T) {
	t.Setenv("PORT", "4200")

	path := filepath.Join(t.TempDir(), "global.yaml")
	var captured servicepkg.Config
	runner := &serviceRunnerStub{startResults: []servicepkg.StartResult{{
		Action:  servicepkg.ActionAlreadyActive,
		Manager: servicepkg.ManagerInfo{Name: servicepkg.ManagerManual, Unit: string(servicepkg.ManagerManual)},
		State:   servicepkg.StateRunning,
	}}}
	cmd := NewRootCommand(t.Context(), WithServiceFactory(func(cfg servicepkg.Config) (ServiceRunner, error) {
		captured = cfg
		return runner, nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "--config", path, "start"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if captured.DashboardURL != "http://localhost:4200" {
		t.Fatalf("DashboardURL = %q, want environment port", captured.DashboardURL)
	}
	want := []string{"--config", path, "--port", "4200", "--headless"}
	if !reflect.DeepEqual(captured.Arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", captured.Arguments, want)
	}
}

func TestServiceBinaryPathPreservesVerifiedInvocation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stable := filepath.Join(root, "bin", "detent")
	cellar := filepath.Join(root, "Cellar", "detent", "1.2.3", "bin", "detent")
	other := filepath.Join(root, "other", "detent")
	tests := []struct {
		name       string
		invocation string
		lookPath   func(string) (string, error)
		eval       func(string) (string, error)
		want       string
	}{
		{
			name:       "absolute stable symlink",
			invocation: stable,
			lookPath:   exec.LookPath,
			eval: func(path string) (string, error) {
				if path == stable || path == cellar {
					return cellar, nil
				}
				return path, nil
			},
			want: stable,
		},
		{
			name:       "path lookup",
			invocation: "detent",
			lookPath: func(string) (string, error) {
				return stable, nil
			},
			eval: func(string) (string, error) {
				return cellar, nil
			},
			want: stable,
		},
		{
			name:       "different executable",
			invocation: other,
			lookPath:   exec.LookPath,
			eval: func(path string) (string, error) {
				if path == other {
					return other, nil
				}
				return cellar, nil
			},
			want: cellar,
		},
		{
			name:       "lookup failure",
			invocation: "detent",
			lookPath: func(string) (string, error) {
				return "", errors.New("not found")
			},
			eval: func(path string) (string, error) {
				return path, nil
			},
			want: cellar,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := serviceBinaryPath(cellar, tt.invocation, tt.lookPath, tt.eval); got != tt.want {
				t.Fatalf("serviceBinaryPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func serviceFactoryFor(runner ServiceRunner) ServiceFactory {
	return func(servicepkg.Config) (ServiceRunner, error) {
		return runner, nil
	}
}

type serviceRunnerStub struct {
	startResults []servicepkg.StartResult
	startErrs    []error
	startOptions []servicepkg.StartOptions
	status       servicepkg.Status
	statusErr    error
}

func (s *serviceRunnerStub) Start(_ context.Context, opts servicepkg.StartOptions) (servicepkg.StartResult, error) {
	s.startOptions = append(s.startOptions, opts)
	index := len(s.startOptions) - 1
	var result servicepkg.StartResult
	if index < len(s.startResults) {
		result = s.startResults[index]
	}
	var err error
	if index < len(s.startErrs) {
		err = s.startErrs[index]
	}
	return result, err
}

func (s *serviceRunnerStub) Status(context.Context) (servicepkg.Status, error) {
	return s.status, s.statusErr
}
