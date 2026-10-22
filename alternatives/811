package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func testRunnerServiceName(string) (string, error) {
	return runnerServiceName, nil
}

func TestHubRunnerRegisterDistinctServices(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			hubs := []*registerHub{newRegisterHub(t, nil), newRegisterHub(t, nil)}
			paths := []string{filepath.Join(root, ".config", "detent-runner", "global.yaml"), filepath.Join(root, "local", "global.yaml")}
			selected := [2]string{runnerServiceNameForPath(paths[0], root), runnerServiceNameForPath(paths[1], root)}
			if selected[0] != "detent.runner" || selected[1] == selected[0] || !strings.HasPrefix(selected[1], "detent.runner-") {
				t.Fatalf("service names before either installation = %v", selected)
			}
			var names [2]string
			for _, test := range []struct {
				name string
				hub  int
			}{
				{name: "second Hub", hub: 1},
				{name: "first install"},
				{name: "first Hub re-registration"},
				{name: "second Hub re-registration", hub: 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					path := paths[test.hub]
					starter := func(cmd *cobra.Command, configPath string) error {
						cfg, err := readRunnerSetupConfig(configPath)
						if err != nil {
							return err
						}
						name := serviceNameForConfig(cfg)
						if name != selected[test.hub] {
							t.Fatalf("installation changed selected name %q to %q", selected[test.hub], name)
						}
						if name == servicepkg.DefaultName {
							t.Fatal("runner selected the board service")
						}
						if test.hub == 0 && name != "detent.runner" {
							t.Fatalf("first runner = %q, want detent.runner", name)
						}
						if test.hub == 1 && (!strings.HasPrefix(name, "detent.runner-") || name == names[0]) {
							t.Fatalf("second runner = %q, first = %q", name, names[0])
						}
						if names[test.hub] != "" && names[test.hub] != name {
							t.Fatalf("re-registration changed %q to %q", names[test.hub], name)
						}
						names[test.hub] = name
						opts := defaultOptions()
						if got := configuredServiceName(configPath, opts); got != name {
							t.Fatalf("update service discovery = %q, want %q", got, name)
						}
						serviceHome := t.TempDir()
						controller, err := servicepkg.New(servicepkg.Config{
							Name: name, GOOS: goos, HomeDir: serviceHome, UID: "501",
							BinaryPath: "/opt/detent", ConfigPath: configPath, Arguments: []string{"--config", configPath, "--headless"},
							UserSystemdPath: filepath.Join(serviceHome, name+".service"),
							SystemUnitPaths: []string{filepath.Join(serviceHome, "missing")},
							InspectManual:   func(string) (servicepkg.Inspection, error) { return servicepkg.Inspection{}, nil },
							RunCommand: func(context.Context, string, ...string) (string, error) {
								return "Could not find service", errors.New("exit status 113")
							},
						})
						if err != nil {
							return err
						}
						result, err := controller.Start(cmd.Context(), servicepkg.StartOptions{})
						if err != nil {
							return err
						}
						definition := result.Definition
						wantUnit := name + ".service"
						wantFilename := wantUnit
						arguments := systemdDefinitionArguments([]byte(definition.Content))
						if goos == "darwin" {
							wantUnit = "com.digitaldrywood." + name
							wantFilename = wantUnit + ".plist"
							arguments = launchdDefinitionArguments([]byte(definition.Content))
						}
						if result.Manager.Unit != wantUnit || filepath.Base(definition.Path) != wantFilename || len(serviceArgumentsForConfig(arguments, configPath)) == 0 {
							t.Fatalf("service = %+v, arguments = %v", result.Manager, arguments)
						}
						if definition.StandardOutPath != filepath.Join(filepath.Dir(configPath), "logs", "service.out.log") || definition.StandardErrorPath != filepath.Join(filepath.Dir(configPath), "logs", "service.err.log") {
							t.Fatalf("service logs = %+v", definition)
						}
						return nil
					}
					command := newHubRunnerRegisterCommandWithServiceName("test", func(string) string { return "" }, starter, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil }, func(context.Context) error { return nil }, func(path string) (string, error) {
						return runnerServiceNameForPath(path, root), nil
					})
					output, err := executeRegister(t, command, "--url", hubs[test.hub].server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", path, "--workspace-root", filepath.Join(filepath.Dir(path), "work"), "--service")
					if err != nil {
						t.Fatalf("register: %v\n%s", err, output)
					}
					var registration runnerRegistration
					if err := json.Unmarshal([]byte(output), &registration); err != nil || registration.Service != names[test.hub] || !registration.ServiceRun {
						t.Fatalf("registration = %+v, %v", registration, err)
					}
				})
			}
		})
	}
}

func TestHubRunnerRegisterPreservesServiceName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, serviceName, wantName string
		wantError                   bool
	}{
		{name: "legacy runner", wantName: "detent.runner"},
		{name: "default runner", serviceName: "detent.runner", wantName: "detent.runner"},
		{name: "custom runner", serviceName: "detent.runner-custom", wantName: "detent.runner-custom"},
		{name: "board service refused", serviceName: "detent", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, nil)
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			paths := runnerPaths{config: filepath.Join(root, "global.yaml"), identity: filepath.Join(root, "identity.json"), workspaces: filepath.Join(root, "work")}
			base := hub.server.URL + "/organizations/org_example"
			cfg := runnerConfig(base, "org_example", "Build host", 2, paths)
			cfg.ServiceName = test.serviceName
			body, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.config, body, 0o600); err != nil {
				t.Fatal(err)
			}
			started := false
			starter := func(_ *cobra.Command, path string) error {
				started = true
				loaded, err := readRunnerSetupConfig(path)
				if err != nil {
					return err
				}
				if got := serviceNameForConfig(loaded); got != test.wantName {
					t.Fatalf("service = %q, want %q", got, test.wantName)
				}
				return nil
			}
			command := newHubRunnerRegisterCommandWithServiceName("test", func(string) string { return "" }, starter, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil }, func(context.Context) error { return nil }, func(string) (string, error) {
				t.Fatal("existing config requested a new service name")
				return "", nil
			})
			output, err := executeRegister(t, command, "--url", base, "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", paths.config, "--workspace-root", paths.workspaces, "--service")
			if (err != nil) != test.wantError || started == test.wantError {
				t.Fatalf("started = %v, error = %v\n%s", started, err, output)
			}
			if got := mustRead(t, paths.config); got != string(body) {
				t.Fatal("registration rewrote the existing config")
			}
		})
	}
}
