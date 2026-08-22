package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
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

type statusServiceRunner struct {
	ServiceRunner
	fallbackURL string
}

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
	dashboardPort := resolvedPort
	if cmd.Name() == "status" {
		dashboardPort = resolveConfiguredRuntimePort(cmd.Context(), runtimeInput{
			Config:     &cfg,
			ConfigPath: resolution,
			Workflow:   firstGlobalWorkflowPath(cfg),
		}, runtimeDepsFromOptions(opts))
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
	dashboardURL := "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(dashboardPort.Value))
	runner, err := factory(servicepkg.Config{
		GOOS:         runtime.GOOS,
		BinaryPath:   binaryPath,
		ConfigPath:   resolution.Path,
		Arguments:    arguments,
		LockPath:     filepath.Join(filepath.Dir(resolution.Path), "detent.db.lock"),
		Version:      opts.version,
		AutoUpdate:   serviceAutoUpdate(cfg.Update.AutoCheckEnabled, cfg.Update.AutoApplyEnabled),
		DashboardURL: dashboardURL,
		Install:      install,
	})
	if err != nil {
		return nil, err
	}
	if cmd.Name() == "status" {
		return statusServiceRunner{ServiceRunner: runner, fallbackURL: dashboardURL}, nil
	}
	return runner, nil
}

func (r statusServiceRunner) Status(ctx context.Context) (servicepkg.Status, error) {
	status, err := r.ServiceRunner.Status(ctx)
	if err != nil {
		return servicepkg.Status{}, err
	}
	status.DashboardURL = r.fallbackURL
	if port, ok := installedServicePort(status.ServiceManager, status.DefinitionPath); ok {
		status.DashboardURL = "http://" + net.JoinHostPort(dashboardHost, strconv.Itoa(port))
	}
	return status, nil
}

func installedServicePort(manager servicepkg.ManagerName, path string) (int, bool) {
	if strings.TrimSpace(path) == "" {
		return 0, false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var arguments []string
	switch manager {
	case servicepkg.ManagerSystemd:
		arguments = systemdDefinitionArguments(content)
	case servicepkg.ManagerLaunchd:
		arguments = launchdDefinitionArguments(content)
	default:
		return 0, false
	}
	for index, argument := range arguments {
		if argument == "--port" && index+1 < len(arguments) {
			return validServicePort(arguments[index+1])
		}
		if raw, ok := strings.CutPrefix(argument, "--port="); ok {
			return validServicePort(raw)
		}
	}
	return 0, false
}

func runningServiceArguments(ctx context.Context, configPath string, opts options) []string {
	factory := opts.service
	if factory == nil {
		factory = defaultServiceFactory
	}
	runCommand := opts.runCommand
	if runCommand == nil {
		runCommand = defaultCommandRunner
	}
	runner, err := factory(servicepkg.Config{
		GOOS:       runtime.GOOS,
		ConfigPath: configPath,
		LockPath:   filepath.Join(filepath.Dir(configPath), "detent.db.lock"),
		RunCommand: servicepkg.CommandRunner(runCommand),
	})
	if err != nil {
		return nil
	}
	status, err := runner.Status(ctx)
	if err != nil || !status.Running() {
		return nil
	}
	switch status.ServiceManager {
	case servicepkg.ManagerSystemd:
		arguments := []string{"cat", status.Service}
		if status.ServiceScope == "user" {
			arguments = append([]string{"--user"}, arguments...)
		}
		if content, runErr := runCommand(ctx, "systemctl", arguments...); runErr == nil {
			if parsed := systemdDefinitionArguments([]byte(content)); len(parsed) > 0 {
				return serviceArgumentsForConfig(parsed, configPath)
			}
		}
	case servicepkg.ManagerLaunchd:
		if content, readErr := os.ReadFile(status.DefinitionPath); readErr == nil {
			return serviceArgumentsForConfig(launchdDefinitionArguments(content), configPath)
		}
	}
	if content, readErr := os.ReadFile(status.DefinitionPath); readErr == nil {
		return serviceArgumentsForConfig(systemdDefinitionArguments(content), configPath)
	}
	return nil
}

func serviceArgumentsForConfig(arguments []string, configPath string) []string {
	serviceConfig, ok := serviceStringFlag(arguments, "--config")
	if !ok {
		return nil
	}
	serviceConfig, serviceErr := filepath.Abs(serviceConfig)
	configPath, configErr := filepath.Abs(configPath)
	if serviceErr != nil || configErr != nil || filepath.Clean(serviceConfig) != filepath.Clean(configPath) {
		return nil
	}
	return arguments
}

func validServicePort(raw string) (int, bool) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	return port, err == nil && port >= 0
}

func systemdDefinitionArguments(content []byte) []string {
	var arguments []string
	for line := range strings.SplitSeq(string(content), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		if parsed := splitDefinitionArguments(value); len(parsed) > 0 {
			for index := range parsed {
				parsed[index] = strings.ReplaceAll(parsed[index], "%%", "%")
			}
			arguments = parsed
		}
	}
	return arguments
}

func splitDefinitionArguments(value string) []string {
	var arguments []string
	for value = strings.TrimSpace(value); value != ""; value = strings.TrimSpace(value) {
		if value[0] == '"' {
			end := quotedArgumentEnd(value)
			if end < 0 {
				return nil
			}
			argument, err := strconv.Unquote(value[:end+1])
			if err != nil {
				return nil
			}
			arguments = append(arguments, argument)
			value = value[end+1:]
		} else {
			index := strings.IndexAny(value, " \t")
			if index < 0 {
				return append(arguments, value)
			}
			arguments = append(arguments, value[:index])
			value = value[index:]
		}
	}
	return arguments
}

func quotedArgumentEnd(value string) int {
	escaped := false
	for index := 1; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case value[index] == '\\':
			escaped = true
		case value[index] == '"':
			return index
		}
	}
	return -1
}

func launchdDefinitionArguments(content []byte) []string {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	wantArguments := false
	inArguments := false
	var arguments []string
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "key":
				var key string
				if err := decoder.DecodeElement(&key, &element); err != nil {
					return nil
				}
				wantArguments = strings.TrimSpace(key) == "ProgramArguments"
			case "array":
				if wantArguments {
					inArguments = true
					wantArguments = false
				}
			case "string":
				if inArguments {
					var argument string
					if err := decoder.DecodeElement(&argument, &element); err != nil {
						return nil
					}
					arguments = append(arguments, argument)
				}
			}
		case xml.EndElement:
			if inArguments && element.Name.Local == "array" {
				return arguments
			}
		}
	}
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
