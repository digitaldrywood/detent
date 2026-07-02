package codex

import (
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
)

func TestTurnSandboxPolicyForWorkspaceMergesExtraWritableRoots(t *testing.T) {
	t.Parallel()

	policy := map[string]any{
		"networkAccess": true,
		"writableRoots": []string{"/existing"},
		"writable_roots": []any{
			"/legacy",
			42,
		},
	}

	got, ok := turnSandboxPolicyForWorkspace("workspace-write", policy, []string{"/extra", "/existing", " "}).(map[string]any)
	if !ok {
		t.Fatalf("turnSandboxPolicyForWorkspace() = %T, want map", got)
	}

	if got["type"] != "workspaceWrite" {
		t.Fatalf("policy type = %#v, want workspaceWrite", got["type"])
	}
	if got["networkAccess"] != true {
		t.Fatalf("policy networkAccess = %#v, want true", got["networkAccess"])
	}
	if roots, ok := got["writableRoots"].([]string); !ok || !reflect.DeepEqual(roots, []string{"/existing", "/legacy", "/extra"}) {
		t.Fatalf("policy writableRoots = %#v, want merged roots", got["writableRoots"])
	}
	if _, ok := got["writable_roots"]; ok {
		t.Fatalf("policy writable_roots = %#v, want absent", got["writable_roots"])
	}
	if _, ok := policy["type"]; ok {
		t.Fatalf("original policy type = %#v, want absent", policy["type"])
	}
}

func TestWorkspaceWriteSandboxPolicyMapSkipsExplicitNonWorkspacePolicy(t *testing.T) {
	t.Parallel()

	policy := map[string]any{
		"type":          "dangerFullAccess",
		"networkAccess": true,
	}

	got, ok := workspaceWriteSandboxPolicyMap("workspace-write", policy)
	if ok {
		t.Fatalf("workspaceWriteSandboxPolicyMap() = %#v, true; want false for explicit non-workspace policy", got)
	}
	if policy["type"] != "dangerFullAccess" {
		t.Fatalf("policy type = %#v, want dangerFullAccess", policy["type"])
	}
	if _, ok := policy["writableRoots"]; ok {
		t.Fatalf("policy writableRoots = %#v, want absent", policy["writableRoots"])
	}
}

func TestOptionsFromConfigClonesApprovalPolicy(t *testing.T) {
	t.Parallel()

	rawPolicy := map[string]any{
		"reject": map[string]any{
			"rules": true,
		},
	}
	options := OptionsFromConfig(config.Codex{
		ApprovalPolicy: config.MapValue(rawPolicy),
		ThreadSandbox:  "workspace-write",
		TurnSandboxPolicy: map[string]any{
			"type": "workspaceWrite",
		},
	})
	rawPolicy["reject"].(map[string]any)["rules"] = false

	policy, ok := options.ApprovalPolicy.(map[string]any)
	if !ok {
		t.Fatalf("ApprovalPolicy = %T, want map", options.ApprovalPolicy)
	}
	reject, ok := policy["reject"].(map[string]any)
	if !ok {
		t.Fatalf("ApprovalPolicy.reject = %T, want map", policy["reject"])
	}
	if reject["rules"] != true {
		t.Fatalf("ApprovalPolicy.reject.rules = %#v, want cloned true", reject["rules"])
	}
	if options.ThreadSandbox != "workspace-write" {
		t.Fatalf("ThreadSandbox = %q, want workspace-write", options.ThreadSandbox)
	}
}
