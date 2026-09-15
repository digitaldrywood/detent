package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/claudecode"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

var nativeCacheVariables = []string{"GOCACHE", "GOMODCACHE", "GOBIN", "GOLANGCI_LINT_CACHE", "GOFLAGS", "npm_config_cache", "PNPM_HOME", "YARN_CACHE_FOLDER", "CARGO_HOME", "CARGO_TARGET_DIR", "PIP_CACHE_DIR", "UV_CACHE_DIR", "PLAYWRIGHT_BROWSERS_PATH", "DOCKER_CONFIG"}

// Exercise real backend process construction; the helper exits before speaking
// either provider protocol, after recording only the invariant's environment.
func TestINV12BackendToolchainEnvironment(t *testing.T) {
	for _, backendKind := range []string{"codex", "claude_code"} {
		for _, mode := range []string{"absent", "host", "explicit"} {
			t.Run(backendKind+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				observed := filepath.Join(root, "observed.json")
				factory := func(ctx context.Context) *exec.Cmd {
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeCacheEnvironmentHelper$", "--")
					for _, entry := range os.Environ() {
						key, _, _ := strings.Cut(entry, "=")
						remove := key == "DETENT_CACHE_ENV_HELPER"
						for _, name := range nativeCacheVariables {
							if key == name {
								remove = true
							}
						}
						if !remove {
							cmd.Env = append(cmd.Env, entry)
						}
					}
					cmd.Env = append(cmd.Env, "DETENT_CACHE_ENV_HELPER="+observed)
					if mode == "host" {
						for _, name := range nativeCacheVariables {
							cmd.Env = append(cmd.Env, name+"=operator-value")
						}
					}
					return cmd
				}
				var backend runner.AgentBackend
				if backendKind == "codex" {
					transport, err := codex.NewLocalTransportFactory(factory)
					if err != nil {
						t.Fatal(err)
					}
					server, err := codex.NewAppServer(transport)
					if err != nil {
						t.Fatal(err)
					}
					backend, err = codex.NewAgentBackend(server, codex.Options{ThreadSandbox: "danger-full-access"})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var err error
					backend, err = claudecode.NewAgentBackend(claudecode.Options{CommandFactory: factory})
					if err != nil {
						t.Fatal(err)
					}
				}
				variables := map[string]string{}
				if mode == "explicit" {
					for _, name := range nativeCacheVariables {
						variables[name] = "operator-value"
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				_, err := backend.RunTurn(ctx, runner.AgentTurnRequest{Workspace: root, TempDir: root, Environment: procgroup.Environment{Variables: variables}}, nil)
				if err == nil {
					t.Fatal("helper unexpectedly completed provider protocol")
				}
				data, err := os.ReadFile(observed)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]string
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				for _, name := range nativeCacheVariables {
					value, present := got[name]
					if mode == "absent" {
						if present {
							t.Errorf("Detent set %s=%q", name, value)
						}
					} else if !present || value != "operator-value" {
						t.Errorf("operator %s changed: %q, present=%t", name, value, present)
					}
				}
				for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
					if got[name] != root {
						t.Errorf("%s=%q, want scratch %q", name, got[name], root)
					}
				}
			})
		}
	}
}

func TestNativeCacheEnvironmentHelper(t *testing.T) {
	output := os.Getenv("DETENT_CACHE_ENV_HELPER")
	if output == "" {
		return
	}
	values := map[string]string{}
	for _, name := range append(append([]string(nil), nativeCacheVariables...), "TMPDIR", "TMP", "TEMP") {
		if value, present := os.LookupEnv(name); present {
			values[name] = value
		}
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
