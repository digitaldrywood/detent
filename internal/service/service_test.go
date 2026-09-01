package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/update"
)

func TestControllerStartDecisionMatrix(t *testing.T) {
	t.Parallel()

	installMethods := []update.InstallSource{
		update.InstallSourceRelease,
		update.InstallSourceHomebrew,
		update.InstallSourceGoInstall,
		update.InstallSourceDevelopment,
	}
	for _, installMethod := range installMethods {
		for _, servicePresent := range []bool{false, true} {
			for _, running := range []bool{false, true} {
				name := string(installMethod) + "/present=" + boolName(servicePresent) + "/running=" + boolName(running)
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					state := StateStopped
					if running {
						state = StateRunning
					}
					manager := &serviceManagerStub{inspection: Inspection{Present: servicePresent, State: state, PID: 42}}
					controller := controllerForTest(t, Config{
						Install: update.InstallInfo{Source: installMethod},
						Manager: manager,
						InspectManual: func(string) (Inspection, error) {
							if servicePresent {
								return Inspection{State: StateStopped}, nil
							}
							return Inspection{State: state, PID: 84}, nil
						},
					})

					result, err := controller.Start(t.Context(), StartOptions{})
					if err != nil {
						t.Fatalf("Start() error = %v", err)
					}
					switch {
					case servicePresent && running:
						assertStartResult(t, result, ActionAlreadyActive, ManagerSystemd, 42)
					case servicePresent:
						assertStartResult(t, result, ActionStarted, ManagerSystemd, 42)
						if manager.startCalls != 1 {
							t.Fatalf("start calls = %d, want 1", manager.startCalls)
						}
					case running:
						assertStartResult(t, result, ActionAlreadyActive, ManagerManual, 84)
					case !servicePresent:
						assertStartResult(t, result, ActionNeedsInstall, ManagerSystemd, 0)
					}
				})
			}
		}
	}
}

func TestControllerStartInstallsAndRestarts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		inspection    Inspection
		opts          StartOptions
		wantAction    Action
		wantInstall   int
		wantStart     int
		wantRestart   int
		wantWait      int
		definitionSet bool
	}{
		{
			name:          "installs missing service",
			inspection:    Inspection{State: StateStopped},
			opts:          StartOptions{Install: true},
			wantAction:    ActionInstalled,
			wantInstall:   1,
			wantStart:     1,
			definitionSet: true,
		},
		{
			name:        "restarts running service",
			inspection:  Inspection{Present: true, State: StateRunning, PID: 42},
			opts:        StartOptions{Restart: true},
			wantAction:  ActionRestarted,
			wantRestart: 1,
		},
		{
			name:       "waits for stopping service",
			inspection: Inspection{Present: true, State: StateStopping, PID: 42},
			wantAction: ActionStarted,
			wantStart:  1,
			wantWait:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := &serviceManagerStub{inspection: tt.inspection}
			controller := controllerForTest(t, Config{
				Manager: manager,
				InspectManual: func(string) (Inspection, error) {
					return Inspection{State: StateStopped}, nil
				},
			})
			result, err := controller.Start(t.Context(), tt.opts)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if result.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", result.Action, tt.wantAction)
			}
			if manager.installCalls != tt.wantInstall || manager.startCalls != tt.wantStart || manager.restartCalls != tt.wantRestart || manager.waitCalls != tt.wantWait {
				t.Fatalf("calls = install:%d start:%d restart:%d wait:%d", manager.installCalls, manager.startCalls, manager.restartCalls, manager.waitCalls)
			}
			if got := result.Definition != nil; got != tt.definitionSet {
				t.Fatalf("Definition set = %t, want %t", got, tt.definitionSet)
			}
		})
	}
}

func TestControllerRefusesManualRestart(t *testing.T) {
	t.Parallel()

	controller := controllerForTest(t, Config{
		Manager: &serviceManagerStub{inspection: Inspection{State: StateStopped}},
		InspectManual: func(string) (Inspection, error) {
			return Inspection{State: StateRunning, PID: 84}, nil
		},
	})
	result, err := controller.Start(t.Context(), StartOptions{Restart: true})
	if !errors.Is(err, ErrManualRestart) {
		t.Fatalf("Start() error = %v, want %v", err, ErrManualRestart)
	}
	assertStartResult(t, result, ActionAlreadyActive, ManagerManual, 84)
}

func TestControllerStatusReportsManagedAndManualStates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		managed        Inspection
		manual         Inspection
		wantManager    ManagerName
		wantExpected   bool
		wantRunning    bool
		wantPID        int
		wantUptimeSecs int64
	}{
		{
			name:           "managed running",
			managed:        Inspection{Present: true, State: StateRunning, PID: 42, StartedAt: now.Add(-time.Hour)},
			wantManager:    ManagerSystemd,
			wantExpected:   true,
			wantRunning:    true,
			wantPID:        42,
			wantUptimeSecs: 3600,
		},
		{
			name:         "managed stopped",
			managed:      Inspection{Present: true, State: StateStopped},
			wantManager:  ManagerSystemd,
			wantExpected: true,
		},
		{
			name:        "manual running",
			manual:      Inspection{State: StateRunning, PID: 84},
			wantManager: ManagerManual,
			wantRunning: true,
			wantPID:     84,
		},
		{
			name:        "manual stopped",
			manual:      Inspection{State: StateStopped},
			wantManager: ManagerManual,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			controller := controllerForTest(t, Config{
				Install:      update.InstallInfo{Source: update.InstallSourceRelease},
				Version:      "v1.2.3",
				AutoUpdate:   "apply enabled",
				DashboardURL: "http://localhost:4000",
				ConfigPath:   "/tmp/global.yaml",
				Manager:      &serviceManagerStub{inspection: tt.managed},
				InspectManual: func(string) (Inspection, error) {
					return tt.manual, nil
				},
				Now: func() time.Time { return now },
			})
			status, err := controller.Status(t.Context())
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if status.ServiceManager != tt.wantManager || status.Expected != tt.wantExpected || status.Running() != tt.wantRunning || status.PID != tt.wantPID || status.UptimeSeconds != tt.wantUptimeSecs {
				t.Fatalf("Status() = %#v", status)
			}
			if status.InstallMethod != update.InstallSourceRelease || status.Version != "v1.2.3" || status.AutoUpdate != "apply enabled" || status.DashboardURL != "http://localhost:4000" {
				t.Fatalf("metadata = %#v", status)
			}
		})
	}
}

func TestStatusRunningRequiresRunningState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state State
		want  bool
	}{
		{state: StateStopped, want: false},
		{state: StateStarting, want: false},
		{state: StateRunning, want: true},
		{state: StateStopping, want: false},
	}
	for _, tt := range tests {
		if got := (Status{State: tt.state}).Running(); got != tt.want {
			t.Errorf("Running() for %q = %t, want %t", tt.state, got, tt.want)
		}
	}
}

func TestManagerFromProcessEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		goos string
		env  map[string]string
		want ManagerName
	}{
		{
			name: "explicit manager",
			goos: "darwin",
			env:  map[string]string{ManagerEnvironment: string(ManagerLaunchd)},
			want: ManagerLaunchd,
		},
		{
			name: "legacy launchd definition",
			goos: "darwin",
			env:  map[string]string{launchdServiceEnvironment: launchdLabel},
			want: ManagerLaunchd,
		},
		{
			name: "unrelated XPC service",
			goos: "darwin",
			env:  map[string]string{launchdServiceEnvironment: "application.com.example.client"},
		},
		{
			name: "non-macOS process",
			goos: "linux",
			env:  map[string]string{launchdServiceEnvironment: launchdLabel},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ManagerFromProcessEnvironment(tt.goos, func(key string) (string, bool) {
				value, ok := tt.env[key]
				return value, ok
			})
			if got != tt.want {
				t.Fatalf("ManagerFromProcessEnvironment() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInspectManualUsesInstanceLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	lock, err := instancelock.Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	inspection, err := inspectManual(path)
	if err != nil {
		t.Fatalf("inspectManual() error = %v", err)
	}
	if inspection.State != StateRunning || inspection.PID != os.Getpid() || inspection.StartedAt.IsZero() {
		t.Fatalf("inspection = %#v", inspection)
	}
}

func TestProcessStartedAt(t *testing.T) {
	t.Parallel()

	startedAt := processStartedAt(t.Context(), func(_ context.Context, name string, args ...string) (string, error) {
		if name != "ps" || !reflect.DeepEqual(args, []string{"-o", "lstart=", "-p", "42"}) {
			t.Fatalf("command = %s %#v", name, args)
		}
		return "Thu Jul 16 10:30:00 2026\n", nil
	}, 42)
	if startedAt.IsZero() || startedAt.Hour() != 10 || startedAt.Minute() != 30 {
		t.Fatalf("processStartedAt() = %v", startedAt)
	}
}

func controllerForTest(t *testing.T, cfg Config) *Controller {
	t.Helper()
	controller, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return controller
}

func assertStartResult(t *testing.T, got StartResult, action Action, manager ManagerName, pid int) {
	t.Helper()
	if got.Action != action || got.Manager.Name != manager || got.PID != pid {
		t.Fatalf("StartResult = %#v, want action=%q manager=%q pid=%d", got, action, manager, pid)
	}
}

func boolName(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

type serviceManagerStub struct {
	inspection   Inspection
	inspectErr   error
	installErr   error
	startErr     error
	restartErr   error
	waitErr      error
	installCalls int
	startCalls   int
	restartCalls int
	waitCalls    int
}

func (s *serviceManagerStub) Info() ManagerInfo {
	return ManagerInfo{Name: ManagerSystemd, Scope: "user", Unit: systemdUnitName, DefinitionPath: "/tmp/detent.service"}
}

func (s *serviceManagerStub) Definition() Definition {
	return Definition{Path: "/tmp/detent.service", Content: "unit\n"}
}

func (s *serviceManagerStub) Inspect(context.Context) (Inspection, error) {
	return s.inspection, s.inspectErr
}

func (s *serviceManagerStub) Install(context.Context) (Definition, error) {
	s.installCalls++
	return s.Definition(), s.installErr
}

func (s *serviceManagerStub) Start(context.Context) error {
	s.startCalls++
	return s.startErr
}

func (s *serviceManagerStub) Restart(context.Context) error {
	s.restartCalls++
	return s.restartErr
}

func (s *serviceManagerStub) WaitStopped(context.Context) error {
	s.waitCalls++
	return s.waitErr
}

func TestWriteDefinitionDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "service", "detent.service")
	definition := Definition{Path: path, Content: "first\n"}
	if err := writeDefinition(definition); err != nil {
		t.Fatalf("writeDefinition() error = %v", err)
	}
	if err := writeDefinition(Definition{Path: path, Content: "second\n"}); err == nil {
		t.Fatal("writeDefinition() error = nil, want existing-file error")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.TrimSpace(string(raw)) != "first" {
		t.Fatalf("contents = %q, want first", raw)
	}
}
