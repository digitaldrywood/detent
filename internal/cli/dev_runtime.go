package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/devruntime"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

const devRuntimeExampleCommand = "detent dev-runtime --port 0"

func newDevRuntimeCommand(host *string, port *int, opts options) *cobra.Command {
	var home string
	var dbPath string
	var trackerMode string
	var fixturePath string
	var allowLiveDB bool
	var allowProductionPort bool

	cmd := &cobra.Command{
		Use:   "dev-runtime",
		Short: "Run an isolated mock Detent runtime for dogfood-safe e2e tests",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := OutputForCommand(cmd); err != nil {
				return err
			}

			runtimePort := derefInt(port, -1)
			if flagChanged(cmd, "port") {
				if runtimePort < 0 {
					return WrapValidation(hintedError(nil, "--port must be greater than or equal to 0", exampleHint(devRuntimeExampleCommand), devRuntimeExampleCommand))
				}
			} else {
				runtimePort = 0
			}

			runtime, err := devruntime.Build(devruntime.Config{
				Home:                home,
				DBPath:              dbPath,
				TrackerMode:         trackerMode,
				FixturePath:         fixturePath,
				Port:                runtimePort,
				AllowLiveDatabase:   allowLiveDB,
				AllowProductionPort: allowProductionPort,
			})
			if err != nil {
				return devRuntimeError(err)
			}

			return opts.boot(cmd.Context(), devRuntimeBootConfig(runtime, derefString(host), opts, cmd.OutOrStdout()))
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "isolated runtime home; a temp directory is created when omitted")
	cmd.Flags().StringVar(&dbPath, "db", "", "isolated SQLite path or :memory:; relative paths are resolved inside --home")
	cmd.Flags().StringVar(&trackerMode, "tracker", devruntime.TrackerMemory, "isolated tracker backend")
	cmd.Flags().StringVar(&fixturePath, "fixture", "", "YAML fixture with mock issues and pull requests")
	cmd.Flags().BoolVar(&allowLiveDB, "allow-live-db", false, "allow --db to point at the operator's live ~/.detent/detent.db")
	cmd.Flags().BoolVar(&allowProductionPort, "allow-production-port", false, "allow binding the isolated runtime to the live dogfood port")
	return cmd
}

func devRuntimeBootConfig(runtime devruntime.Runtime, host string, opts options, output io.Writer) BootConfig {
	runtimePort := runtime.Port
	runtimeHost := strings.TrimSpace(host)
	if runtimeHost == "" {
		runtimeHost = defaultWebHost
	}
	return BootConfig{
		Mode:           BootModeRunning,
		Global:         runtime.Global,
		ConfigPathRule: globalconfig.PathRuleFlag,
		Runtime: RuntimeSettings{
			ConfigPath: RuntimeValue{Value: runtime.ConfigPath, Source: runtimeSourceFlag},
			Env:        RuntimeValue{Value: "dev", Source: runtimeSourceDefault},
			LogLevel:   RuntimeValue{Value: "info", Source: runtimeSourceDefault},
			Port:       RuntimeIntValue{Value: runtimePort, Source: runtimeSourceFlag},
			GitHubToken: RuntimeSecret{
				Required: false,
			},
		},
		WorkflowPath:     runtime.WorkflowPath,
		Host:             runtimeHost,
		Port:             &runtimePort,
		RuntimeDBPath:    runtime.DBPath,
		RuntimeLogPath:   filepath.Join(runtime.Home, "detent.log"),
		Isolated:         devRuntimeIsolationInfo(runtime),
		Version:          opts.version,
		Build:            opts.build,
		Headless:         true,
		StdoutTTY:        false,
		Output:           output,
		Runner:           orchestrator.FakeRunner{},
		ConnectorFactory: devRuntimeConnectorFactory,
	}
}

func devRuntimeIsolationInfo(runtime devruntime.Runtime) *IsolatedRuntimeInfo {
	return &IsolatedRuntimeInfo{
		Home:          runtime.Home,
		ConfigPath:    runtime.ConfigPath,
		WorkflowPath:  runtime.WorkflowPath,
		WorkspaceRoot: runtime.WorkspaceRoot,
		DBPath:        runtime.DBPath,
		DBMode:        runtime.DBMode,
		TrackerMode:   runtime.TrackerMode,
		FixturePath:   runtime.FixturePath,
	}
}

func devRuntimeConnectorFactory(cfg workflowconfig.Config) (connector.Connector, error) {
	if cfg.Tracker.Kind != workflowconfig.TrackerMemory {
		return nil, fmt.Errorf("%w: %s", devruntime.ErrUnsupportedTracker, cfg.Tracker.Kind)
	}
	return memory.New(memory.Config{
		Issues:   cfg.Tracker.Issues,
		Stateful: true,
	}), nil
}

func devRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, devruntime.ErrLivePort):
		return WrapValidation(hintedError(err, err.Error(), exampleHint(devRuntimeExampleCommand), devRuntimeExampleCommand))
	case errors.Is(err, devruntime.ErrLiveDatabase):
		return WrapValidation(hintedError(err, err.Error(), "Use the default :memory: DB or a path under --home for isolated tests.", "detent dev-runtime --db :memory:"))
	default:
		return err
	}
}
