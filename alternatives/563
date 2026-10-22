package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestAgentBackendToolConnection(t *testing.T) {
	for _, test := range []struct {
		name               string
		readOnly, textOnly bool
		tier               string
	}{{"native", false, false, isolation.NativeTrusted}, {"sandbox", false, false, isolation.Sandbox}, {"sandbox-read-only", true, false, isolation.Sandbox}, {"sandbox-text-only", true, true, isolation.Sandbox}, {"read-only", true, false, isolation.NativeTrusted}, {"text-only", true, true, isolation.NativeTrusted}} {
		t.Run(test.name, func(t *testing.T) {
			if test.tier == isolation.Sandbox && !isolation.SandboxAvailable() {
				t.Skip("sandbox unavailable")
			}
			var configPath, endpoint string
			backend := newTestBackend(t, Options{AllowedTools: []string{"Read"}, IsolationPolicy: func() (isolation.Policy, error) { return isolation.Policy{Tier: test.tier}, nil }, CommandFactoryWithArgs: func(_ context.Context, args []string) *exec.Cmd {
				configs := 0
				var settings struct {
					Sandbox struct {
						Enabled bool `json:"enabled"`
						Network struct {
							AllowedDomains    []string `json:"allowedDomains"`
							AllowLocalBinding bool     `json:"allowLocalBinding"`
							StrictAllowlist   bool     `json:"strictAllowlist"`
						} `json:"network"`
					} `json:"sandbox"`
				}
				for i, arg := range args {
					if arg == "--mcp-config" && i+1 < len(args) {
						configs++
						configPath = args[i+1]
					}
					if arg == "--settings" && i+1 < len(args) {
						if err := json.Unmarshal([]byte(args[i+1]), &settings); err != nil {
							t.Fatal(err)
						}
					}
				}
				if configs != 1 || !slices.Contains(args, "--strict-mcp-config") || !slices.Contains(args, "mcp__detent__attach_evidence") {
					t.Fatal("worker MCP configuration was omitted or overridden")
				}
				if !test.readOnly && !slices.Contains(args, "Read") {
					t.Fatal("worker MCP configuration replaced allowed built-in tools")
				}
				raw, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				var config struct {
					Servers map[string]struct {
						URL string `json:"url"`
					} `json:"mcpServers"`
				}
				if json.Unmarshal(raw, &config) != nil || len(config.Servers) != 1 {
					t.Fatal("invalid worker connection configuration")
				}
				endpoint = config.Servers["detent"].URL
				if test.tier == isolation.Sandbox {
					address, err := url.Parse(endpoint)
					if err != nil {
						t.Fatal(err)
					}
					network := settings.Sandbox.Network
					if !settings.Sandbox.Enabled || !network.StrictAllowlist || network.AllowLocalBinding || !slices.Contains(network.AllowedDomains, address.Host) {
						t.Fatal("sandbox cannot reach its worker MCP endpoint without local binding")
					}
					if len(network.AllowedDomains) != len(isolation.Domains())+1 || slices.Contains(network.AllowedDomains, "127.0.0.1") || slices.Contains(network.AllowedDomains, "localhost") || slices.Contains(network.AllowedDomains, "::1") {
						t.Fatal("worker MCP connection broadened sandbox network access")
					}
				}
				info, err := os.Stat(configPath)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("connection configuration permissions: %v", err)
				}
				client := &http.Client{Timeout: time.Second}
				response, err := client.Get(endpoint)
				if err != nil {
					t.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatal("worker connection accepted an unauthenticated request")
				}
				return nil
			}})
			tools := []runner.AgentTool{{Name: "attach_evidence", InputSchema: json.RawMessage(`{"type":"object"}`)}}
			_, err := backend.RunTurnWithTools(t.Context(), runner.AgentTurnRequest{Workspace: t.TempDir(), TempDir: t.TempDir(), ReadOnly: test.readOnly, TextOnly: test.textOnly, AllowLocalBinding: test.readOnly}, tools, func(context.Context, runner.AgentToolCall) (runner.AgentToolResult, error) {
				t.Error("unexpected dispatch")
				return runner.AgentToolResult{}, nil
			}, nil)
			if !errors.Is(err, ErrNilCommand) {
				t.Fatalf("startup failure: %v", err)
			}
			if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("worker connection configuration survived turn cleanup")
			}
			client := &http.Client{Timeout: time.Second}
			if response, err := client.Get(endpoint); err == nil {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("worker connection survived turn cleanup")
			}
		})
	}
}
