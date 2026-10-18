package runner

import (
	"context"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestINV12WorkerInheritsToolchainEnvironment(t *testing.T) {
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOBIN", "GOLANGCI_LINT_CACHE", "GOFLAGS", "npm_config_cache", "PNPM_HOME", "YARN_CACHE_FOLDER", "CARGO_HOME", "CARGO_TARGET_DIR", "PIP_CACHE_DIR", "UV_CACHE_DIR", "PLAYWRIGHT_BROWSERS_PATH", "DOCKER_CONFIG"} {
		t.Run(name, func(t *testing.T) {
			for _, override := range []bool{false, true} {
				variables := map[string]string{}
				if override {
					variables[name] = "/operator/cache"
				}
				backend := &cacheTestAgentBackend{}
				_, err, cleanupErr := runAgentBackendTurn(context.Background(), backend, AgentTurnRequest{
					Workspace: t.TempDir(), Environment: procgroup.Environment{Variables: variables},
				}, nil)
				if err != nil || cleanupErr != nil {
					t.Fatalf("turn: %v, cleanup: %v", err, cleanupErr)
				}
				got, exists := backend.request.Environment.Variables[name]
				if exists != override || got != variables[name] {
					t.Fatalf("%s = %q, exists %t", name, got, exists)
				}
				if !reflect.DeepEqual(backend.request.Environment.PathSuffixes, []string(nil)) {
					t.Fatal("worker modified PATH suffixes")
				}
				if backend.request.TempDir == "" {
					t.Fatal("worker lost per-attempt scratch")
				}
			}
		})
	}
}

type cacheTestAgentBackend struct{ request AgentTurnRequest }

func (b *cacheTestAgentBackend) RunTurn(_ context.Context, request AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.request = request
	return AgentTurnResult{}, nil
}
