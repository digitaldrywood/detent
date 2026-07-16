package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/update"
)

type ServiceRunner interface {
	Start(context.Context, servicepkg.StartOptions) (servicepkg.StartResult, error)
	Status(context.Context) (servicepkg.Status, error)
}

type ServiceFactory func(servicepkg.Config) (ServiceRunner, error)

func defaultServiceFactory(cfg servicepkg.Config) (ServiceRunner, error) {
	return servicepkg.New(cfg)
}

func newStartCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var assumeYes bool
	var restart bool

	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start Detent as a background service",
		Example:      "detent start",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			runner, err := serviceRunnerForCommand(cmd, configPath, host, port, opts)
			if err != nil {
				return err
			}
			result, err := runner.Start(cmd.Context(), servicepkg.StartOptions{Install: assumeYes, Restart: restart})
			if err != nil {
				return err
			}
			if result.Action == servicepkg.ActionNeedsInstall {
				if out.IsJSON() {
					if writeErr := out.WriteJSON(result); writeErr != nil {
						return writeErr
					}
					return NewValidationError("service installation confirmation required", "Run detent start --yes to install and start the service.", nil)
				}
				confirmed, err := confirmServiceInstall(cmd, result)
				if err != nil {
					return err
				}
				if !confirmed {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "Service installation canceled.")
					return err
				}
				result, err = runner.Start(cmd.Context(), servicepkg.StartOptions{Install: true, Restart: restart})
				if err != nil {
					return err
				}
			}
			return out.Write(func(out io.Writer) error {
				return writeStartText(out, result)
			}, result)
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "install a missing user service without prompting")
	cmd.Flags().BoolVar(&restart, "restart", false, "restart Detent when the managed service is already running")
	return cmd
}

func newStatusCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show Detent installation and service status",
		Example:      "detent status",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			runner, err := serviceRunnerForCommand(cmd, configPath, host, port, opts)
			if err != nil {
				return err
			}
			status, err := runner.Status(cmd.Context())
			if err != nil {
				return err
			}
			if err := out.Write(func(out io.Writer) error {
				return writeServiceStatusText(out, status)
			}, status); err != nil {
				return err
			}
			if status.Expected && !status.Running() {
				return servicepkg.ErrNotRunning
			}
			return nil
		},
	}
}

func serviceRunnerForCommand(cmd *cobra.Command, configPath *string, host *string, port *int, opts options) (ServiceRunner, error) {
	resolution, err := resolveConfigPathResolution(pointerString(configPath), opts)
	if err != nil {
		return nil, err
	}
	cfg, err := opts.readOrDefault(resolution.Path)
	if err != nil {
		return nil, err
	}
	portFlag := runtimeIntFlag{Value: pointerInt(port, -1), Set: flagChanged(cmd, "port")}
	resolvedPort, err := resolveRuntimePort(cmd.Context(), runtimeInput{
		Config:     &cfg,
		ConfigPath: resolution,
		Workflow:   firstGlobalWorkflowPath(cfg),
		Flags:      runtimeFlags{Port: portFlag},
	}, runtimeDepsFromOptions(opts))
	if err != nil {
		return nil, err
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve current executable: %w", err)
	}
	arguments := []string{"--config", resolution.Path}
	if flagChanged(cmd, "host") {
		arguments = append(arguments, "--host", pointerString(host))
	}
	if portFlag.Set {
		arguments = append(arguments, "--port", strconv.Itoa(portFlag.Value))
	}
	arguments = append(arguments, "--headless")
	install := update.DetectInstallSource(update.DetectionOptions{
		CurrentVersion: opts.version,
		ExecutablePath: executable,
		GOOS:           runtime.GOOS,
	})
	binaryPath := serviceBinaryPath(executable, os.Args[0], exec.LookPath, filepath.EvalSymlinks)
	if resolvedPort.Source == "PORT" && !portFlag.Set {
		arguments = append(arguments[:len(arguments)-1], "--port", strconv.Itoa(resolvedPort.Value), "--headless")
	}
	factory := opts.service
	if factory == nil {
		factory = defaultServiceFactory
	}
	return factory(servicepkg.Config{
		GOOS:         runtime.GOOS,
		BinaryPath:   binaryPath,
		ConfigPath:   resolution.Path,
		Arguments:    arguments,
		LockPath:     filepath.Join(filepath.Dir(resolution.Path), "detent.db.lock"),
		Version:      opts.version,
		AutoUpdate:   serviceAutoUpdate(cfg.Update.AutoCheckEnabled, cfg.Update.AutoApplyEnabled),
		DashboardURL: "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(resolvedPort.Value)),
		Install:      install,
	})
}

func serviceBinaryPath(executable string, invocation string, lookPath func(string) (string, error), evalSymlinks func(string) (string, error)) string {
	invocation = strings.TrimSpace(invocation)
	if invocation == "" || lookPath == nil || evalSymlinks == nil {
		return executable
	}
	candidate := invocation
	if !filepath.IsAbs(candidate) && !strings.ContainsAny(candidate, `/\`) {
		resolved, err := lookPath(candidate)
		if err != nil {
			return executable
		}
		candidate = resolved
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return executable
	}
	realCandidate, err := evalSymlinks(candidate)
	if err != nil {
		return executable
	}
	realExecutable, err := evalSymlinks(executable)
	if err != nil || filepath.Clean(realCandidate) != filepath.Clean(realExecutable) {
		return executable
	}
	return candidate
}

func confirmServiceInstall(cmd *cobra.Command, result servicepkg.StartResult) (bool, error) {
	path := "the platform default location"
	if result.Definition != nil && strings.TrimSpace(result.Definition.Path) != "" {
		path = result.Definition.Path
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Install the %s user service at %s? [y/N] ", result.Manager.Name, path); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read service installation confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func writeStartText(out io.Writer, result servicepkg.StartResult) error {
	switch result.Action {
	case servicepkg.ActionInstalled:
		if result.Definition != nil {
			if _, err := fmt.Fprintf(out, "Wrote %s:\n%s", result.Definition.Path, result.Definition.Content); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(out, "Started %s via %s.\n", result.Manager.Unit, formatServiceManager(result.Manager))
		return err
	case servicepkg.ActionRestarted:
		_, err := fmt.Fprintf(out, "Restarted %s via %s.\n", result.Manager.Unit, formatServiceManager(result.Manager))
		return err
	case servicepkg.ActionAlreadyActive:
		if result.Manager.Name == servicepkg.ManagerManual {
			_, err := fmt.Fprintf(out, "Detent is already running in foreground/manual mode%s. Stop it before installing a managed service.\n", formatPID(result.PID))
			return err
		}
		_, err := fmt.Fprintf(out, "Detent is already %s via %s%s. Run detent start --restart to restart it.\n", result.State, formatServiceManager(result.Manager), formatPID(result.PID))
		return err
	default:
		_, err := fmt.Fprintf(out, "Started %s via %s.\n", result.Manager.Unit, formatServiceManager(result.Manager))
		return err
	}
}

func writeServiceStatusText(out io.Writer, status servicepkg.Status) error {
	lines := []string{
		"Install method: " + string(status.InstallMethod),
		"Service manager: " + formatServiceManager(servicepkg.ManagerInfo{Name: status.ServiceManager, Scope: status.ServiceScope}),
		"Service: " + status.Service,
		"State: " + string(status.State),
	}
	if status.PID > 0 {
		lines = append(lines, "PID: "+strconv.Itoa(status.PID))
	}
	if status.UptimeSeconds > 0 {
		lines = append(lines, "Uptime: "+(time.Duration(status.UptimeSeconds)*time.Second).String())
	} else {
		lines = append(lines, "Uptime: unavailable")
	}
	lines = append(lines,
		"Version: "+status.Version,
		"Auto-update: "+status.AutoUpdate,
		"Dashboard: "+status.DashboardURL,
		"Config: "+status.ConfigPath,
	)
	if status.DefinitionPath != "" {
		lines = append(lines, "Definition: "+status.DefinitionPath)
	}
	_, err := fmt.Fprintln(out, strings.Join(lines, "\n"))
	return err
}

func formatServiceManager(info servicepkg.ManagerInfo) string {
	if info.Scope == "" || info.Name == servicepkg.ManagerManual {
		return string(info.Name)
	}
	return fmt.Sprintf("%s (%s)", info.Name, info.Scope)
}

func formatPID(pid int) string {
	if pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" (PID %d)", pid)
}

func serviceAutoUpdate(autoCheck bool, autoApply bool) string {
	switch {
	case autoApply:
		return "apply enabled"
	case autoCheck:
		return "check enabled"
	default:
		return "disabled"
	}
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func pointerInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
