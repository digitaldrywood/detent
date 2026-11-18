package operatortool

import (
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

const LocalProjectConfiguration = "local_project_configuration"
const RunnerProjectDiagnostics = "runner_project_diagnostics"

func localProjectSchema(name string) string {
	properties := `"runner_id":{"type":"string","minLength":1,"maxLength":256},"project_id":{"type":"string","minLength":1,"maxLength":256}`
	if name == RunnerProjectDiagnostics {
		return `{"type":"object","properties":{` + properties + `,"issue_id":{"type":"string","minLength":1,"maxLength":256},"attempt_id":{"type":"string","minLength":1,"maxLength":256},"cursor":{"type":"string","minLength":1,"maxLength":300},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["project_id"],"additionalProperties":false}`
	}
	if name == LocalProjectConfiguration {
		return `{"type":"object","properties":{` + properties + `},"required":["project_id"],"additionalProperties":false}`
	}
	extra := `,"expected_runner_revision":{"type":"integer","minimum":1,"maximum":2147483647},"request_id":{"type":"string","minLength":1,"maxLength":128},"expected_config_revision":{"type":"string","minLength":64,"maxLength":64,"pattern":"^[0-9a-f]{64}$"},"expected_policy_id":{"type":"string","minLength":1,"maxLength":256}`
	required := `"project_id","request_id","expected_config_revision","expected_policy_id"`
	if name == "apply_local_project_policy" {
		extra += `,"allow_local_binding":{"type":"boolean"},"source_revision":{"type":"string","minLength":40,"maxLength":64,"pattern":"^[0-9a-f]{40}([0-9a-f]{24})?$"},"policy_id":{"type":"string","minLength":1,"maxLength":256}`
		required += `,"source_revision","policy_id"`
	}
	if name == "detach_local_project" {
		extra += `,"checkpoint":{"type":"string","minLength":64,"maxLength":64,"pattern":"^[0-9a-f]{64}$"}`
		required += `,"checkpoint"`
	}
	return `{"type":"object","properties":{` + properties + extra + `},"required":[` + required + `],"additionalProperties":false}`
}

func IsLocalProjectTool(name string) bool {
	for _, definition := range LocalProjectCatalog() {
		if name == definition.Name {
			return true
		}
	}
	return false
}

type LocalProjectArguments struct {
	IssueID                string `json:"issue_id,omitempty"`
	AttemptID              string `json:"attempt_id,omitempty"`
	Cursor                 string `json:"cursor,omitempty"`
	Limit                  int    `json:"limit,omitempty"`
	RunnerID               string `json:"runner_id,omitempty"`
	ExpectedRunnerRevision int64  `json:"expected_runner_revision,omitempty"`
	AllowLocalBinding      *bool  `json:"allow_local_binding,omitempty"`
	RequestID              string `json:"request_id,omitempty"`
	ProjectID              string `json:"project_id"`
	ExpectedConfigRevision string `json:"expected_config_revision,omitempty"`
	ExpectedPolicyID       string `json:"expected_policy_id,omitempty"`
	PolicyID               string `json:"policy_id,omitempty"`
	SourceRevision         string `json:"source_revision,omitempty"`
	Checkpoint             string `json:"checkpoint,omitempty"`
}

func DecodeLocalProjectArguments(name string, raw json.RawMessage, submission bool) (LocalProjectArguments, error) {
	var result LocalProjectArguments
	definition, ok := Lookup(name)
	if !ok || !IsLocalProjectTool(name) || DecodeArguments(raw, &result) != nil {
		return result, ErrInvalidArguments
	}
	var schema fleetSchema
	var value any
	if json.Unmarshal(definition.InputSchema, &schema) != nil || json.Unmarshal(raw, &value) != nil {
		return result, ErrInvalidArguments
	}
	if !submission {
		schema.Required = slices.DeleteFunc(schema.Required, func(key string) bool { return key == "request_id" })
	}
	if !schema.accepts(value) || strings.TrimSpace(result.ProjectID) == "" {
		return result, ErrInvalidArguments
	}
	if !definition.Annotations.ReadOnly {
		if !localHex(result.ExpectedConfigRevision, 64) || strings.TrimSpace(result.ExpectedPolicyID) == "" {
			return result, ErrInvalidArguments
		}
		if submission && strings.TrimSpace(result.RequestID) == "" {
			return result, ErrInvalidArguments
		}
	}
	if name == "apply_local_project_policy" && ((!localHex(result.SourceRevision, 40) && !localHex(result.SourceRevision, 64)) || strings.TrimSpace(result.PolicyID) == "") || name == "detach_local_project" && !localHex(result.Checkpoint, 64) {
		return result, ErrInvalidArguments
	}
	return result, nil
}

func localHex(value string, length int) bool {
	_, err := hex.DecodeString(value)
	return err == nil && len(value) == length && strings.ToLower(value) == value
}
