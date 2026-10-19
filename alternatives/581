package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/devruntime"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/testutil/fakehubscheduling"
)

const (
	devRuntimeDemoHost       = "0.0.0.0"
	devRuntimeExampleCommand = "detent dev-runtime --port 0"
)

func newDevRuntimeCommand(host *string, port *int, opts options) *cobra.Command {
	var home string
	var dbPath string
	var trackerMode string
	var fixturePath string
	var demo string
	var demoClock string
	var demoProjectID string
	var allowLiveDB bool
	var allowProductionPort bool
	var authEmail string
	var authSMTP string
	var authFrom string
	var authOIDCIssuer string
	var authOIDCClientID string
	var authOIDCClientSecret string

	cmd := &cobra.Command{
		Use:     "dev-runtime",
		Short:   "Run an isolated mock Detent runtime for dogfood-safe e2e tests",
		Example: devRuntimeExampleCommand,
		Args:    NoArgs,
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

			authConfig, err := devRuntimeAuthConfig(authEmail, authSMTP, authFrom, authOIDCIssuer, authOIDCClientID, authOIDCClientSecret)
			if err != nil {
				return WrapValidation(err)
			}
			runtime, err := devruntime.Build(devruntime.Config{
				Home:                home,
				DBPath:              dbPath,
				TrackerMode:         trackerMode,
				FixturePath:         fixturePath,
				Demo:                demo,
				DemoClock:           demoClock,
				DemoProjectID:       demoProjectID,
				Port:                runtimePort,
				AllowLiveDatabase:   allowLiveDB,
				AllowProductionPort: allowProductionPort,
				Auth:                authConfig,
			})
			if err != nil {
				return devRuntimeError(err)
			}

			return opts.boot(cmd.Context(), devRuntimeBootConfig(cmd.Context(), runtime, derefString(host), opts, cmd.OutOrStdout()))
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "isolated runtime home; a temp directory is created when omitted")
	cmd.Flags().StringVar(&dbPath, "db", "", "isolated SQLite path or :memory:; relative paths are resolved inside --home")
	cmd.Flags().StringVar(&trackerMode, "tracker", devruntime.TrackerHubNative, "isolated tracker backend")
	cmd.Flags().StringVar(&fixturePath, "fixture", "", "YAML fixture with mock issues and pull requests")
	cmd.Flags().StringVar(&demo, "demo", "", "built-in isolated demo fixture (kanban, screenshots)")
	cmd.Flags().StringVar(&demoClock, "demo-clock", "frozen", "demo clock mode for screenshots fixtures (frozen, play)")
	cmd.Flags().StringVar(&demoProjectID, "demo-project", "", "generated primary project ID for isolated demos; screenshots stays dogfood")
	cmd.Flags().BoolVar(&allowLiveDB, "allow-live-db", false, "allow --db to point at the operator's live ~/.detent/detent.db")
	cmd.Flags().BoolVar(&allowProductionPort, "allow-production-port", false, "allow binding the isolated runtime to the live dogfood port")
	cmd.Flags().StringVar(&authEmail, "auth-email", "", "allowed email for isolated dashboard auth")
	cmd.Flags().StringVar(&authSMTP, "auth-smtp", "", "SMTP host:port for isolated magic-link auth")
	cmd.Flags().StringVar(&authFrom, "auth-from", "", "sender email for isolated magic-link auth")
	cmd.Flags().StringVar(&authOIDCIssuer, "auth-oidc-issuer", "", "issuer URL for isolated OIDC auth")
	cmd.Flags().StringVar(&authOIDCClientID, "auth-oidc-client-id", "", "client ID for isolated OIDC auth")
	cmd.Flags().StringVar(&authOIDCClientSecret, "auth-oidc-client-secret", "", "client secret for isolated OIDC auth")
	cmd.AddCommand(newDevRuntimeCaptureCommand(opts))
	return cmd
}

func devRuntimeAuthConfig(email string, smtpAddress string, from string, oidcIssuer string, oidcClientID string, oidcClientSecret string) (globalconfig.Auth, error) {
	email = strings.TrimSpace(email)
	smtpAddress = strings.TrimSpace(smtpAddress)
	from = strings.TrimSpace(from)
	oidcIssuer = strings.TrimSpace(oidcIssuer)
	oidcClientID = strings.TrimSpace(oidcClientID)
	magicLinkRequested := smtpAddress != "" || from != ""
	oidcRequested := oidcIssuer != "" || oidcClientID != "" || oidcClientSecret != ""
	if !magicLinkRequested && !oidcRequested && email == "" {
		return globalconfig.Auth{}, nil
	}
	if magicLinkRequested && oidcRequested {
		return globalconfig.Auth{}, errors.New("isolated runtime auth must select either magic-link or oidc settings")
	}
	if oidcRequested {
		if email == "" || oidcIssuer == "" || oidcClientID == "" || oidcClientSecret == "" {
			return globalconfig.Auth{}, errors.New("--auth-email, --auth-oidc-issuer, --auth-oidc-client-id, and --auth-oidc-client-secret must be set together")
		}
		return globalconfig.Auth{
			Mode:          globalconfig.AuthModeOIDC,
			AllowedEmails: []string{email},
			OIDC: globalconfig.OIDC{
				IssuerURL:    oidcIssuer,
				ClientID:     oidcClientID,
				ClientSecret: oidcClientSecret,
			},
		}, nil
	}
	if email == "" || smtpAddress == "" || from == "" {
		return globalconfig.Auth{}, errors.New("--auth-email, --auth-smtp, and --auth-from must be set together")
	}
	host, portText, err := net.SplitHostPort(smtpAddress)
	if err != nil || strings.TrimSpace(host) == "" {
		return globalconfig.Auth{}, errors.New("--auth-smtp must be a host:port address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return globalconfig.Auth{}, errors.New("--auth-smtp port must be between 1 and 65535")
	}
	return globalconfig.Auth{
		Mode:          globalconfig.AuthModeMagicLink,
		AllowedEmails: []string{email},
		SMTP: globalconfig.SMTP{
			Host: host,
			Port: port,
			From: from,
		},
	}, nil
}

func devRuntimeBootConfig(ctx context.Context, runtime devruntime.Runtime, host string, opts options, output io.Writer) BootConfig {
	runtimePort := runtime.Port
	runtimeHost := strings.TrimSpace(host)
	if runtimeHost == "" {
		runtimeHost = defaultWebHost
		if runtime.Demo != "" {
			runtimeHost = devRuntimeDemoHost
		}
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
		Runner:           devRuntimeRunner{},
		ConnectorFactory: devRuntimeConnectorFactory,
		Scheduling: &fakehubscheduling.Source{Factory: func(id string) (connector.Connector, bool) {
			for _, selected := range runtime.Global.Projects {
				if selected.ID != id {
					continue
				}
				workflow, err := project.LoadWorkflowContext(ctx, selected)
				if err != nil {
					return nil, false
				}
				conn, err := devRuntimeConnectorFactory(workflow.Config)
				return conn, err == nil
			}
			return nil, false
		}},
	}
}

func devRuntimeIsolationInfo(runtime devruntime.Runtime) *IsolatedRuntimeInfo {
	manifestPath := ""
	if runtime.Demo == devruntime.DemoScreenshots {
		manifestPath = "/api/v1/demo/scenarios"
	}
	return &IsolatedRuntimeInfo{
		Home:          runtime.Home,
		ConfigPath:    runtime.ConfigPath,
		WorkflowPath:  runtime.WorkflowPath,
		WorkspaceRoot: runtime.WorkspaceRoot,
		DBPath:        runtime.DBPath,
		DBMode:        runtime.DBMode,
		TrackerMode:   runtime.TrackerMode,
		FixturePath:   runtime.FixturePath,
		Demo:          runtime.Demo,
		DemoClock:     runtime.DemoClock,
		ManifestPath:  manifestPath,
	}
}

func devRuntimeConnectorFactory(cfg workflowconfig.Config) (connector.Connector, error) {
	if cfg.Tracker.Kind != workflowconfig.TrackerHubNative {
		return nil, fmt.Errorf("%w: %s", devruntime.ErrUnsupportedTracker, cfg.Tracker.Kind)
	}
	return devRuntimeConnector{Connector: fakehub.New(fakehub.Config{
		Issues:   cfg.Tracker.Issues,
		Stateful: true,
	})}, nil
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
	case errors.Is(err, devruntime.ErrUnsupportedDemo):
		return WrapValidation(hintedError(err, err.Error(), "Use --demo kanban, --demo screenshots, or omit --demo.", "detent dev-runtime --demo screenshots --port 0"))
	case errors.Is(err, devruntime.ErrDemoFixtureConflict):
		return WrapValidation(hintedError(err, err.Error(), "Use either --demo or --fixture, not both.", "detent dev-runtime --demo screenshots --port 0"))
	default:
		return err
	}
}

const devRuntimeAuditPatch = "diff --git a/example.txt b/example.txt\n--- a/example.txt\n+++ b/example.txt\n@@ -1 +1 @@\n-before\n+after\n"

type devRuntimeConnector struct {
	*fakehub.Connector
}

func (c devRuntimeConnector) PullRequestValidationDiff(ctx context.Context, issue connector.Issue) (connector.ValidationDiff, error) {
	current, err := c.HydratePullRequest(ctx, issue)
	if err != nil {
		return connector.ValidationDiff{}, err
	}
	if current.ID != "mock-issue-autopromote" || current.PullRequest == nil || current.PullRequest.Number != 9001 || current.PullRequest.BaseSHA != "isolated-mock-base" || current.PullRequest.HeadSHA != "isolated-mock-head" {
		return connector.ValidationDiff{}, errors.New("isolated mock audit source is unavailable")
	}
	return connector.ValidationDiff{
		Repository: "example-org/example-project", PRNumber: 9001,
		BaseSHA: current.PullRequest.BaseSHA, HeadSHA: current.PullRequest.HeadSHA,
		Files: []string{"example.txt"}, Patch: devRuntimeAuditPatch,
	}, nil
}

type devRuntimeRunner struct {
	orchestrator.FakeRunner
}

func (devRuntimeRunner) Audit(ctx context.Context, request orchestrator.SecurityAuditRequest) (orchestrator.SecurityAuditExecution, error) {
	if err := ctx.Err(); err != nil {
		return orchestrator.SecurityAuditExecution{}, err
	}
	snapshot := request.Snapshot
	if snapshot.IssueID != "mock-issue-autopromote" || snapshot.Repository != "example-org/example-project" || snapshot.PRNumber != 9001 || snapshot.BaseSHA != "isolated-mock-base" || snapshot.HeadSHA != "isolated-mock-head" || snapshot.Diff != devRuntimeAuditPatch || snapshot.DiffTruncated {
		return orchestrator.SecurityAuditExecution{}, errors.New("isolated mock audit source does not match the fixture")
	}
	output := `{"verdict":"pass","summary":"Simulated isolated-runtime audit of the synthetic example fixture; no provider or worker process ran.","findings":[]}`
	result, err := securityaudit.ParseOutput(output)
	if err != nil {
		return orchestrator.SecurityAuditExecution{}, err
	}
	startedAt := time.Now().UTC()
	return orchestrator.SecurityAuditExecution{
		InvocationID: "isolated-mock-" + uuid.NewString(), AuthenticationMode: "isolated_mock",
		WorkerProcess:    procgroup.Identity{PID: 4200, GroupID: 4200, StartedAt: startedAt},
		ProviderThreadID: "isolated-mock-thread", ProviderSessionID: "isolated-mock-session",
		Output: output, Result: result, StartedAt: startedAt, CompletedAt: startedAt,
	}, nil
}
