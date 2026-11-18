//go:build !windows

package global

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRestrictsExistingConfigFilePermissions(t *testing.T) {

	paths := createProjectFiles(t)
	path := filepath.Join(paths.root, "global.yaml")
	writeFile(t, path, "old")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	err := Write(path, Config{
		APIVersion:  APIVersion,
		Kind:        Kind,
		GitHubToken: "ghp_secret",
		Global: Settings{
			MaxConcurrentAgents: 8,
			Scheduling:          SchedulingWeighted,
		},
		Projects: []Project{
			{ID: "detent", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != configFileMode {
		t.Fatalf("mode = %o, want %o", got, configFileMode)
	}
}
