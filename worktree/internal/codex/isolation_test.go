package codex

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestIsolationSettings(t *testing.T) {
	for _, test := range []struct {
		name         string
		tier         string
		want         string
		valid        bool
		localBinding bool
	}{
		{"sandbox", "sandbox", "workspace-write", true, false},
		{"sandbox local binding", "sandbox", "workspace-write", true, true},
		{"trusted", "native-trusted", "danger-full-access", true, false},
		{"unknown", "container", "", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := isolation.Policy{AllowLocalBinding: test.localBinding, Tier: test.tier, WritableRoots: []string{"/worktree", "/runtime"}, HostServices: []string{"unix:/var/run/example.sock"}}
			options, settings, err := IsolationSettings(policy)
			if test.tier == isolation.Sandbox && !isolation.SandboxAvailable() {
				if !errors.Is(err, isolation.ErrSandboxUnavailable) {
					t.Fatalf("error = %v, want sandbox unavailable", err)
				}
				return
			}
			if (err == nil) != test.valid {
				t.Fatalf("error = %v", err)
			}
			if !test.valid {
				return
			}
			if options.ThreadSandbox != test.want || options.ApprovalPolicy != "never" {
				t.Fatalf("options = %#v", options)
			}
			if test.tier == "sandbox" {
				p := options.TurnSandboxPolicy.(map[string]any)
				if !reflect.DeepEqual(p["writableRoots"], policy.WritableRoots) || p["excludeSlashTmp"] != true || p["excludeTmpdirEnvVar"] != true {
					t.Fatalf("policy = %#v", p)
				}
				n := settings["permissions"].(map[string]any)[options.PermissionProfile].(map[string]any)["network"].(map[string]any)
				if n["enabled"] != true || n["mode"] != "limited" || n["dangerously_allow_all_unix_sockets"] != false || n["allow_local_binding"] != test.localBinding {
					t.Fatalf("network = %#v", n)
				}
				if !reflect.DeepEqual(n["unix_sockets"], map[string]any{"/var/run/example.sock": "allow"}) {
					t.Fatalf("sockets = %#v", n["unix_sockets"])
				}
			}
		})
	}
}

func TestBackendAppliesRunnerIsolation(t *testing.T) {
	for _, test := range []struct {
		name, tier string
		restricted bool
	}{{"sandbox", isolation.Sandbox, false}, {"trusted", isolation.NativeTrusted, false}, {"restricted", isolation.Sandbox, true}} {
		t.Run(test.name, func(t *testing.T) {
			tier := test.tier
			transport := newFakeAppServerTransport([]Message{responseMessage(t, 1, `{"userAgent":"codex-cli/0.159.2"}`), responseMessage(t, 2, `{"thread":{"id":"thread"}}`), responseMessage(t, 3, `{"turn":{"id":"turn"}}`), notificationMessage(t, "turn/completed", `{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)})
			server, err := NewAppServer(&workerTempCapturingTransportFactory{transport: transport}, WithReadTimeout(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			backend, err := NewAgentBackend(server, Options{ThreadSandbox: "danger-full-access", IsolationPolicy: func() (isolation.Policy, error) { return isolation.Policy{Tier: isolation.NativeTrusted}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.RunTurn(isolation.WithPolicy(t.Context(), isolation.Policy{Tier: tier}), runner.AgentTurnRequest{ReadOnly: test.restricted, Workspace: "/worktree", TempDir: "/runtime", AllowLocalBinding: true, ExtraNetworkDomains: []string{"fonts.googleapis.com", "fonts.gstatic.com"}, Model: "test-model", ExtraWritableRoots: []string{"/detent-state"}}, nil)
			if test.tier == isolation.Sandbox && !isolation.SandboxAvailable() {
				if !errors.Is(err, isolation.ErrSandboxUnavailable) {
					t.Fatalf("error = %v, want sandbox unavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sent := transport.sentMessages()
			var params struct {
				Config      map[string]any `json:"config"`
				Sandbox     string         `json:"sandbox"`
				Permissions string         `json:"permissions"`
			}
			if err := json.Unmarshal(sent[2].Params, &params); err != nil {
				t.Fatal(err)
			}
			if tier == isolation.Sandbox {
				if params.Config["default_permissions"] != params.Permissions {
					t.Fatalf("retained permission profile = %v, want %q", params.Config["default_permissions"], params.Permissions)
				}
				if params.Permissions != "detent-runner" || params.Sandbox != "" || params.Config["features.network_proxy"] != !test.restricted {
					t.Fatalf("thread = %#v", params)
				}
				paths, err := toolcache.Resolve(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				roots := []any{"/worktree", "/runtime", "/detent-state"}
				if !test.restricted {
					roots = append(roots, paths.Build, paths.Modules)
				}
				assertJSONContains(t, sent[3].Params, "runtimeWorkspaceRoots", roots)
				profile := params.Config["permissions"].(map[string]any)["detent-runner"].(map[string]any)
				network := profile["network"].(map[string]any)
				if network["enabled"] != !test.restricted {
					t.Fatalf("network = %#v", network)
				}
				if !test.restricted {
					domains := network["domains"].(map[string]any)
					if domains["fonts.googleapis.com"] != "allow" || domains["fonts.gstatic.com"] != "allow" || domains["example.com"] != nil {
						t.Fatalf("project domain grants = %#v", domains)
					}
				}
				if !test.restricted && network["allow_local_binding"] != true {
					t.Fatalf("local binding grant missing: %#v", network)
				}
				if test.restricted && profile["filesystem"].(map[string]any)[":workspace_roots"] != "read" {
					t.Fatal("restricted turn can write")
				}
				assertJSONContains(t, sent[3].Params, "permissions", "detent-runner")
				assertJSONOmits(t, sent[3].Params, "sandboxPolicy")
			} else if params.Sandbox != "danger-full-access" {
				t.Fatalf("sandbox = %q", params.Sandbox)
			}
		})
	}
}

func TestBackendRejectsMissingIsolationPolicy(t *testing.T) {
	server, err := NewAppServer(&workerTempCapturingTransportFactory{})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("routing unavailable")
	backend, err := NewAgentBackend(server, Options{IsolationPolicy: func() (isolation.Policy, error) { return isolation.Policy{}, want }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.RunTurn(t.Context(), runner.AgentTurnRequest{}, nil); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
