package cli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func registeredRunnerServiceName(configPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve runner service home: %w", err)
	}
	var definitions []string
	switch runtime.GOOS {
	case "darwin":
		definitions = []string{filepath.Join(home, "Library", "LaunchAgents", servicepkg.LaunchdLabel(runnerServiceName)+".plist")}
	case "linux":
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		unit := servicepkg.SystemdUnit(runnerServiceName)
		definitions = []string{
			filepath.Join(configHome, "systemd", "user", unit),
			filepath.Join(home, ".local", "share", "systemd", "user", unit),
			filepath.Join("/etc", "systemd", "user", unit),
			filepath.Join("/usr", "lib", "systemd", "user", unit),
			filepath.Join("/etc", "systemd", "system", unit),
			filepath.Join("/usr", "lib", "systemd", "system", unit),
			filepath.Join("/lib", "systemd", "system", unit),
		}
	}
	return runnerServiceNameFromDefinitions(configPath, runtime.GOOS, definitions)
}

func runnerServiceNameFromDefinitions(configPath, goos string, definitions []string) (string, error) {
	for _, path := range definitions {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read installed runner service: %w", err)
		}
		arguments := systemdDefinitionArguments(content)
		if goos == "darwin" {
			arguments = launchdDefinitionArguments(content)
		}
		if len(serviceArgumentsForConfig(arguments, configPath)) == 0 {
			digest := sha256.Sum256([]byte(filepath.Clean(configPath)))
			return fmt.Sprintf("%s-%x", runnerServiceName, digest[:16]), nil
		}
	}
	return runnerServiceName, nil
}
