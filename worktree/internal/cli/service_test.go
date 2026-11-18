package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/update"
)

func TestStoppedManagedServiceUsesNonzeroExitCode(t *testing.T) {
	t.Parallel()

	if got := ExitCode(servicepkg.ErrNotRunning); got == ExitSuccess {
		t.Fatalf("ExitCode() = %d, want nonzero", got)
	}
}

func TestServiceDefinitionArguments(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, content string
		parse         func([]byte) []string
		want          []string
	}{
		{
			name: "systemd literal percents", parse: systemdDefinitionArguments,
			content: `ExecStart="/opt/detent%%worker" --config "/config/tenant%%2/global.yaml" --host "fe80::1%%eth0"`,
			want:    []string{"/opt/detent%worker", "--config", "/config/tenant%2/global.yaml", "--host", "fe80::1%eth0"},
		},
		{
			name: "launchd escaped config", parse: launchdDefinitionArguments,
			content: `<plist><dict><key>ProgramArguments</key><array><string>/opt/detent</string><string>--config</string><string>/config/first &amp; second/global.yaml</string></array></dict></plist>`,
			want:    []string{"/opt/detent", "--config", "/config/first & second/global.yaml"},
		},
		{
			name: "launchd oversized definition", parse: launchdDefinitionArguments,
			content: `<plist><dict><key>ProgramArguments</key><array><string>` + strings.Repeat("x", 1<<20) + `</string></array></dict></plist>`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.parse([]byte(test.content)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("arguments = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestServiceCommandResolvesConfigAndRuntime(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, serviceName, hubURL, wantName string
	}{
		{name: "board", wantName: "detent"},
		{name: "runner without service name", hubURL: "https://hub.example.test", wantName: runnerServiceName},
		{name: "runner with service name", serviceName: runnerServiceName, hubURL: "https://hub.example.test", wantName: runnerServiceName},
		{name: "custom board", serviceName: "detent.board", wantName: "detent.board"},
		{name: "custom runner", serviceName: "detent.runner-two", hubURL: "https://hub.example.test", wantName: "detent.runner-two"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "global.yaml")
			cfg, err := globalconfig.DefaultAt(path)
			if err != nil {
				t.Fatal(err)
			}
			port := 4100
			cfg.Port = &port
			cfg.ServiceName = test.serviceName
			cfg.Client.URL = test.hubURL
			cfg.Update.AutoApplyEnabled = true
			if err := globalconfig.Write(path, cfg); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"register"} {
				var captured servicepkg.Config
				runner := &serviceRunnerStub{status: servicepkg.Status{ServiceManager: servicepkg.ManagerManual, State: servicepkg.StateStopped}, startResults: []servicepkg.StartResult{{Action: servicepkg.ActionInstalled}}}
				opts := defaultOptions()
				opts.version = "dev"
				opts.lookupEnv = func(string) string { return "" }
				opts.serviceInjected = true
				opts.service = func(cfg servicepkg.Config) (ServiceRunner, error) {
					captured = cfg
					return runner, nil
				}
				if got := configuredServiceName(path, opts); got != test.wantName {
					t.Fatalf("configured service = %s, want %s", got, test.wantName)
				}
				var cmd *cobra.Command
				switch command {
				case "register":
					cmd = &cobra.Command{Use: "register", RunE: func(cmd *cobra.Command, _ []string) error { return runnerServiceStarterFor(opts)(cmd, path) }}
				}
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(&bytes.Buffer{})
				if err := cmd.ExecuteContext(t.Context()); err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				if captured.ConfigPath != path || captured.AutoUpdate != "apply enabled" || captured.DashboardURL != "http://localhost:4100" || captured.Install.Source != update.InstallSourceDevelopment {
					t.Fatalf("%s config = %#v", command, captured)
				}
				if want := []string{"--config", path, "--headless"}; !reflect.DeepEqual(captured.Arguments, want) {
					t.Fatalf("arguments = %#v, want %#v", captured.Arguments, want)
				}
				for _, goos := range []string{"darwin", "linux"} {
					platform := captured
					platform.GOOS = goos
					platform.HomeDir = t.TempDir()
					platform.UserSystemdPath = filepath.Join(platform.HomeDir, servicepkg.SystemdUnit(test.wantName))
					platform.SystemUnitPaths = []string{filepath.Join(platform.HomeDir, "system", "missing.service")}
					platform.UserUnitPaths = []string{platform.UserSystemdPath}
					platform.InspectManual = func(string) (servicepkg.Inspection, error) { return servicepkg.Inspection{}, nil }
					platform.RunCommand = func(context.Context, string, ...string) (string, error) {
						return "Could not find service", errors.New("exit status 113")
					}
					controller, err := servicepkg.New(platform)
					if err != nil {
						t.Fatal(err)
					}
					result, err := controller.Start(t.Context(), servicepkg.StartOptions{})
					if err != nil {
						t.Fatal(err)
					}
					want := servicepkg.SystemdUnit(test.wantName)
					filename := want
					if goos == "darwin" {
						want = servicepkg.LaunchdLabel(test.wantName)
						filename = want + ".plist"
					}
					if result.Manager.Unit != want || filepath.Base(result.Definition.Path) != filename {
						t.Fatalf("%s %s service = %+v, want %s", command, goos, result, want)
					}
				}
			}
		})
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
	onStart      func()
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
	if err == nil && s.onStart != nil {
		s.onStart()
	}
	return result, err
}

func (s *serviceRunnerStub) Status(context.Context) (servicepkg.Status, error) {
	return s.status, s.statusErr
}

func TestRunningProcessArguments(t *testing.T) {
	for _, tt := range []struct {
		name, args string
		pid        int
		wantHost   string
		readErr    bool
	}{
		{"expanded environment", "/usr/bin/detent\x00--config\x00/config/global.yaml\x00--host\x00100.111.222.33\x00--port=4303\x00", 42, "100.111.222.33", false},
		{"different instance", "detent\x00--config=/elsewhere/global.yaml\x00--host=100.111.222.33\x00", 42, "", false},
		{"no explicit config", "detent\x00--host=100.111.222.33\x00", 42, "", false},
		{"missing process", "", 42, "", true},
		{"no process", "", 0, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := runningProcessArguments(tt.pid, "/config/global.yaml", func(path string) ([]byte, error) {
				if path != filepath.Join("/proc", "42", "cmdline") {
					t.Fatalf("path=%s", path)
				}
				if tt.readErr {
					return nil, os.ErrNotExist
				}
				return []byte(tt.args), nil
			})
			host, _ := serviceStringFlag(args, "--host")
			if host != tt.wantHost {
				t.Fatalf("host=%q want %q", host, tt.wantHost)
			}
		})
	}
}
