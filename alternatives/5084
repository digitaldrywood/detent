package testenv

import (
	"fmt"
	"os"
	"strings"
)

func ClearGitEnvironment() error {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			if err := os.Unsetenv(key); err != nil {
				return fmt.Errorf("clear %s: %w", key, err)
			}
		}
	}
	return nil
}
