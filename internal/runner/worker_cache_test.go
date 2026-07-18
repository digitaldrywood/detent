package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestConfigureWorkerCache(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	tests := []struct {
		name             string
		strategy         string
		workspace        string
		tempDir          string
		wantRoot         string
		wantWritableRoot bool
	}{
		{
			name:      "isolated uses turn scratch",
			strategy:  config.WorkspaceCacheIsolated,
			workspace: filepath.Join(workspaceRoot, "issue-1"),
			tempDir:   filepath.Join(workspaceRoot, "issue-1", ".detent", "tmp"),
			wantRoot:  filepath.Join(workspaceRoot, "issue-1", ".detent", "tmp"),
		},
		{
			name:             "shared uses stable project root",
			strategy:         config.WorkspaceCacheShared,
			workspace:        filepath.Join(workspaceRoot, "issue-2"),
			tempDir:          filepath.Join(workspaceRoot, "issue-2", ".detent", "tmp"),
			wantRoot:         sharedWorkerCacheRoot(filepath.Join(workspaceRoot, "issue-1"), "parable"),
			wantWritableRoot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := AgentTurnRequest{
				Workspace:     tt.workspace,
				TempDir:       tt.tempDir,
				cacheStrategy: tt.strategy,
				projectID:     "parable",
				Environment: procgroup.Environment{
					Variables:    map[string]string{"EXISTING": "value", "GOCACHE": "/stale"},
					PathPrefixes: []string{"/existing/bin"},
					PathSuffixes: []string{"/fallback/bin"},
				},
			}
			if err := configureWorkerCache(&request); err != nil {
				t.Fatalf("configureWorkerCache() error = %v", err)
			}

			wantVariables := map[string]string{
				"EXISTING":            "value",
				"GOCACHE":             filepath.Join(tt.wantRoot, "go-build"),
				"GOMODCACHE":          filepath.Join(tt.wantRoot, "go-mod"),
				"GOBIN":               filepath.Join(tt.wantRoot, "go-bin"),
				"GOLANGCI_LINT_CACHE": filepath.Join(tt.wantRoot, "golangci-lint"),
			}
			if !reflect.DeepEqual(request.Environment.Variables, wantVariables) {
				t.Fatalf("Environment.Variables = %#v, want %#v", request.Environment.Variables, wantVariables)
			}
			wantPathPrefixes := []string{"/existing/bin"}
			if !reflect.DeepEqual(request.Environment.PathPrefixes, wantPathPrefixes) {
				t.Fatalf("Environment.PathPrefixes = %#v, want %#v", request.Environment.PathPrefixes, wantPathPrefixes)
			}
			wantPathSuffixes := []string{"/fallback/bin", filepath.Join(tt.wantRoot, "go-bin")}
			if !reflect.DeepEqual(request.Environment.PathSuffixes, wantPathSuffixes) {
				t.Fatalf("Environment.PathSuffixes = %#v, want %#v", request.Environment.PathSuffixes, wantPathSuffixes)
			}
			for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOBIN", "GOLANGCI_LINT_CACHE"} {
				path := wantVariables[name]
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("Stat(%s) error = %v", path, err)
				}
				if !info.IsDir() {
					t.Fatalf("%s is not a directory", path)
				}
			}
			gotWritableRoot := len(request.ExtraWritableRoots) == 1 && request.ExtraWritableRoots[0] == tt.wantRoot
			if gotWritableRoot != tt.wantWritableRoot {
				t.Fatalf("ExtraWritableRoots = %#v, want shared root = %t", request.ExtraWritableRoots, tt.wantWritableRoot)
			}
		})
	}
}

func TestSharedWorkerCacheRootSeparatesProjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	firstWorkspace := filepath.Join(root, "project-issue-1")
	secondWorkspace := filepath.Join(root, "project-issue-2")
	first := sharedWorkerCacheRoot(firstWorkspace, "owner/project")
	second := sharedWorkerCacheRoot(secondWorkspace, "owner/project")
	other := sharedWorkerCacheRoot(secondWorkspace, "owner_project")
	if first != second {
		t.Fatalf("same project roots differ: %q != %q", first, second)
	}
	if first == other {
		t.Fatalf("distinct project roots collide: %q", first)
	}
}

func TestRunAgentBackendTurnCleansScratchAfterCacheFailure(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	backend := &cacheTestAgentBackend{}
	_, err, cleanupErr := runAgentBackendTurn(context.Background(), backend, AgentTurnRequest{
		Workspace:     workspacePath,
		cacheStrategy: "unsupported",
	}, nil)
	if err == nil {
		t.Fatal("runAgentBackendTurn() error = nil, want cache preparation error")
	}
	if cleanupErr != nil {
		t.Fatalf("runAgentBackendTurn() cleanup error = %v", cleanupErr)
	}
	if backend.called {
		t.Fatal("backend called after cache preparation failure")
	}
	if _, statErr := os.Stat(filepath.Join(workspacePath, ".detent", "tmp")); !os.IsNotExist(statErr) {
		t.Fatalf("worker scratch remains after cache preparation failure: %v", statErr)
	}
}

type cacheTestAgentBackend struct {
	called bool
}

func (b *cacheTestAgentBackend) RunTurn(context.Context, AgentTurnRequest, AgentUpdateHandler) (AgentTurnResult, error) {
	b.called = true
	return AgentTurnResult{}, nil
}
