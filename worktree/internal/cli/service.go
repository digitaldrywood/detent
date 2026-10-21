package cli

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
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
	servicePath := opts.lookupEnv("PATH")
	if !opts.serviceInjected && (runtime.GOOS == "linux" || runtime.GOOS == "darwin") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve service home: %w", err)
		}
		servicePath, err = providerServicePath(cfg, servicePath, home)
		if err != nil {
			return nil, err
		}
	}
	runner, err := factory(servicepkg.Config{
		Path:         servicePath,
		Name:         serviceNameForConfig(cfg),
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
	if cfg.Client.URL != "" && cfg.Client.IdentityFile != "" {
		return newRunnerHeartbeatService(runner, cfg)
	}
	return runner, nil
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
		Name:       configuredServiceName(configPath, opts),
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
	// Read the actual expanded command line before falling back to unit files,
	// which may contain environment substitutions or have changed since startup.
	if runtime.GOOS == "linux" {
		if arguments := runningProcessArguments(status.PID, configPath, os.ReadFile); len(arguments) > 0 {
			return arguments
		}
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

func runningProcessArguments(pid int, configPath string, readFile func(string) ([]byte, error)) []string {
	if pid <= 0 {
		return nil
	}
	content, err := readFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil
	}
	return serviceArgumentsForConfig(strings.Split(strings.TrimRight(string(content), "\x00"), "\x00"), configPath)
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

// configuredServiceName selects the config's service, or the board default
// when the config cannot be read.
func configuredServiceName(configPath string, opts options) string {
	read := opts.readOrDefault
	if read == nil {
		read = func(path string) (globalconfig.Config, error) { return globalconfig.ReadOrDefault(path) }
	}
	cfg, err := read(configPath)
	if err != nil {
		return ""
	}
	return serviceNameForConfig(cfg)
}

func serviceNameForConfig(cfg globalconfig.Config) string {
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}
	if cfg.Client.Configured() {
		return runnerServiceName
	}
	return servicepkg.DefaultName
}
