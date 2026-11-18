package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func ExpandWorkspacePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Clean(home), nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func ResolveProjectPath(project globalconfig.Project, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) || path == "~" || strings.HasPrefix(path, "~/") {
		return ExpandWorkspacePath(path)
	}
	base := strings.TrimSpace(project.Workdir)
	if base == "" {
		return ExpandWorkspacePath(path)
	}
	expandedBase, err := ExpandWorkspacePath(base)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(expandedBase, path)), nil
}
