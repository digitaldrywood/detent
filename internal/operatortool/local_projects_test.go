package operatortool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLocalProjectArgumentBoundary(t *testing.T) {
	valid := `{"project_id":"p","request_id":"retry","expected_config_revision":"` + strings.Repeat("a", 64) + `","expected_policy_id":"policy_test"}`
	for _, scenario := range []struct {
		name, tool, raw string
		valid           bool
	}{
		{"read", LocalProjectConfiguration, `{"project_id":"p"}`, true},
		{"drain", "drain_local_project", valid, true},
		{"resume", "resume_local_project", valid, true},
		{"detach", "detach_local_project", strings.TrimSuffix(valid, "}") + `,"checkpoint":"` + strings.Repeat("b", 64) + `"}`, true},
		{"policy", "apply_local_project_policy", strings.TrimSuffix(valid, "}") + `,"source_revision":"` + strings.Repeat("c", 40) + `","policy_id":"policy_new"}`, true},
		{"missing checkpoint", "detach_local_project", valid, false},
		{"wrong revision", "drain_local_project", strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("z", 64), 1), false},
		{"path editor", "drain_local_project", strings.TrimSuffix(valid, "}") + `,"path":"/private/global.yaml"}`, false},
		{"shell editor", "drain_local_project", strings.TrimSuffix(valid, "}") + `,"command":"sed"}`, false},
		{"forged approval", "drain_local_project", strings.TrimSuffix(valid, "}") + `,"confirm":true}`, false},
		{"oversized selector", LocalProjectConfiguration, `{"project_id":"` + strings.Repeat("p", 257) + `"}`, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := DecodeLocalProjectArguments(scenario.tool, json.RawMessage(scenario.raw), true)
			if (err == nil) != scenario.valid {
				t.Fatalf("valid=%t err=%v", scenario.valid, err)
			}
		})
	}
}
