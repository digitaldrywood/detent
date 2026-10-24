package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

func registeredRunnerServiceName(configPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve runner service home: %w", err)
	}
	return runnerServiceNameForPath(configPath, home), nil
}

func runnerServiceNameForPath(configPath, home string) string {
	configPath = filepath.Clean(configPath)
	if configPath == filepath.Join(home, ".config", "detent-runner", "global.yaml") {
		return runnerServiceName
	}
	digest := sha256.Sum256([]byte(configPath))
	return fmt.Sprintf("%s-%x", runnerServiceName, digest[:16])
}
