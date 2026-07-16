package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
