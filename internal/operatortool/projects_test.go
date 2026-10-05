package operatortool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Catch bounds or authority fields reaching the shared application/preview path.
func TestProjectArgumentBounds(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		target    func() any
		invalid   bool
	}{
		{"bounded read", `{"project_id":"p","limit":200}`, func() any { return &ProjectReadRequest{} }, false},
		{"large page", `{"limit":201}`, func() any { return &ProjectReadRequest{} }, true},
		{"negative page", `{"limit":-1}`, func() any { return &ProjectReadRequest{} }, true},
		{"long project", `{"project_id":"` + strings.Repeat("p", 257) + `"}`, func() any { return &ProjectReadRequest{} }, true},
		{"nested secret", `{"input":{"token":"secret"}}`, func() any { return &ProjectRequest[struct{}]{} }, true},
		{"forged authority", `{"input":{},"yolo":true}`, func() any { return &ProjectRequest[struct{}]{} }, true},
		{"null input", `{"input":null}`, func() any { return &ProjectRequest[struct{}]{} }, true},
		{"negative issue", `{"input":{"issue_number":-1}}`, func() any { return &ProjectRequest[ImportStartInput]{} }, true},
		{"too many states", `{"input":{"states":[` + strings.TrimSuffix(strings.Repeat(`{},`, 201), ",") + `]}}`, func() any { return &ProjectRequest[ProjectCreateInput]{} }, true},
		{"too many intake numbers", `{"input":{"numbers":[` + strings.TrimSuffix(strings.Repeat(`1,`, 201), ",") + `]}}`, func() any { return &ProjectRequest[GitHubBatchInput]{} }, true},
		{"behavior object", `{"input":{"policy":{"configuration":{"behavior":{"Budget":{"PerIssueMaxUSD":0.25},"ActiveStates":null,"AllowLocalBinding":null},"prompt":"work"}}}}`, func() any { return &ProjectRequest[PolicyApprovalInput]{} }, false},
		{"behavior array", `{"input":{"policy":{"configuration":{"behavior":[],"prompt":"work"}}}}`, func() any { return &ProjectRequest[PolicyApprovalInput]{} }, true},
		{"behavior string", `{"input":{"policy":{"configuration":{"behavior":"opaque","prompt":"work"}}}}`, func() any { return &ProjectRequest[PolicyApprovalInput]{} }, true},
		{"null behavior", `{"input":{"policy":{"configuration":{"behavior":null,"prompt":"work"}}}}`, func() any { return &ProjectRequest[PolicyApprovalInput]{} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeProjectArguments(json.RawMessage(tt.raw), tt.target())
			if (err != nil) != tt.invalid || err != nil && !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("decode=%v want invalid=%v", err, tt.invalid)
			}
		})
	}
	t.Run("policy discovery", func(t *testing.T) {
		for _, definition := range ProjectCatalog() {
			if definition.Name != "approve_project_policy" {
				continue
			}
			var schema map[string]any
			if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			properties := func(schema map[string]any, name string) map[string]any {
				t.Helper()
				return schema["properties"].(map[string]any)[name].(map[string]any)
			}
			input := properties(schema, "input")
			descriptor := properties(input, "policy")
			configuration := properties(descriptor, "configuration")
			if properties(configuration, "behavior")["type"] != "object" {
				t.Fatal("policy behavior must be advertised as a JSON object")
			}
			for _, name := range []string{"prompt", "shared_prompt", "agents_prompt"} {
				if properties(configuration, name)["maxLength"] != float64(MaxArgumentBytes) {
					t.Fatalf("policy %s discovery rejects bounded prompts", name)
				}
			}
			if properties(properties(descriptor, "workflow"), "source")["maxLength"] != float64(1024) {
				t.Fatal("workflow source discovery differs from descriptor validation")
			}
			return
		}
		t.Fatal("policy approval is missing from discovery")
	})
}
