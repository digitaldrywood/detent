package pathsafe

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var windowsAbsPathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func WorkspaceRelative(workspacePath string, relativePath string) (string, error) {
	workspace := strings.TrimSpace(workspacePath)
	if workspace == "" {
		return "", errors.New("workspace path is required")
	}

	if !IsWorkspaceRelative(relativePath) {
		return "", errors.New("path must be a relative path inside the workspace")
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("absolute workspace path: %w", err)
	}

	path := strings.TrimSpace(relativePath)
	return filepath.Join(absWorkspace, filepath.Clean(path)), nil
}

func IsWorkspaceRelative(relativePath string) bool {
	path := strings.TrimSpace(relativePath)
	return path != "" &&
		!strings.HasPrefix(path, "~") &&
		!filepath.IsAbs(path) &&
		!strings.HasPrefix(path, "/") &&
		!strings.HasPrefix(path, `\`) &&
		!windowsAbsPathPattern.MatchString(path) &&
		!escapesWorkspace(path)
}

func escapesWorkspace(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}
