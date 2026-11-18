package agentoverride

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/markdownfence"
)

type Override struct {
	Model         string
	Effort        string
	Code          RoleOverride
	Rework        RoleOverride
	Merge         RoleOverride
	Plan          RoleOverride
	Routine       RoleOverride
	Validator     RoleOverride
	SecurityAudit RoleOverride
}

type RoleOverride struct {
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

type RoleEffort struct {
	Role      string
	Field     string
	Effort    string
	Inherited bool
}

type blockYAML struct {
	Schema        int          `yaml:"schema"`
	Model         string       `yaml:"model"`
	Effort        string       `yaml:"effort"`
	Code          RoleOverride `yaml:"code"`
	Rework        RoleOverride `yaml:"rework"`
	Merge         RoleOverride `yaml:"merge"`
	Plan          RoleOverride `yaml:"plan"`
	Routine       RoleOverride `yaml:"routine"`
	Validator     RoleOverride `yaml:"validator"`
	SecurityAudit RoleOverride `yaml:"security_audit"`
}

func FromIssueBody(body string) (Override, bool, error) {
	content, ok := lastBlock(body)
	if !ok {
		return Override{}, false, nil
	}

	var raw blockYAML
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Override{}, true, fmt.Errorf("parse detent-agent YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Override{}, true, errors.New("parse detent-agent YAML: multiple YAML documents are not supported")
	} else if !errors.Is(err, io.EOF) {
		return Override{}, true, errors.New("parse detent-agent YAML: multiple YAML documents are not supported")
	}
	if raw.Schema != 1 {
		return Override{}, true, errors.New("detent-agent schema must be 1")
	}

	return Override{
		Model:         strings.TrimSpace(raw.Model),
		Effort:        strings.TrimSpace(raw.Effort),
		Code:          normalizeRoleOverride(raw.Code),
		Rework:        normalizeRoleOverride(raw.Rework),
		Merge:         normalizeRoleOverride(raw.Merge),
		Plan:          normalizeRoleOverride(raw.Plan),
		Routine:       normalizeRoleOverride(raw.Routine),
		Validator:     normalizeRoleOverride(raw.Validator),
		SecurityAudit: normalizeRoleOverride(raw.SecurityAudit),
	}, true, nil
}

func normalizeRoleOverride(override RoleOverride) RoleOverride {
	override.Model = strings.TrimSpace(override.Model)
	override.Effort = strings.TrimSpace(override.Effort)
	return override
}

func (o Override) EffortForRole(role string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "code":
		return o.Code.Effort, "code.effort"
	case "rework":
		if o.Rework.Effort != "" {
			return o.Rework.Effort, "rework.effort"
		}
		return o.Code.Effort, "code.effort"
	case "merge":
		return o.Merge.Effort, "merge.effort"
	case "plan":
		if o.Plan.Effort != "" {
			return o.Plan.Effort, "plan.effort"
		}
	case "routine":
		if o.Routine.Effort != "" {
			return o.Routine.Effort, "routine.effort"
		}
	case "validator":
		if o.Validator.Effort != "" {
			return o.Validator.Effort, "validator.effort"
		}
	case "security_audit":
		if o.SecurityAudit.Effort != "" {
			return o.SecurityAudit.Effort, "security_audit.effort"
		}
	default:
		return "", ""
	}
	return "", ""
}

func (o Override) ModelForRole(role string) (string, string) {
	var model string
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "code":
		model = o.Code.Model
	case "rework":
		model = o.Rework.Model
		if model == "" && o.Code.Model != "" {
			return o.Code.Model, "code.model"
		}
	case "merge":
		model = o.Merge.Model
	case "plan":
		model = o.Plan.Model
	case "routine":
		model = o.Routine.Model
	case "validator":
		model = o.Validator.Model
	case "security_audit":
		model = o.SecurityAudit.Model
	}
	if model != "" {
		return model, role + ".model"
	}
	return o.Model, "model"
}

func (o Override) RoleEfforts() []RoleEffort {
	rework := RoleEffort{Role: "rework", Field: "rework.effort", Effort: o.Rework.Effort}
	if rework.Effort == "" && o.Code.Effort != "" {
		rework.Field = "code.effort"
		rework.Effort = o.Code.Effort
		rework.Inherited = true
	}
	return []RoleEffort{
		{Role: "code", Field: "code.effort", Effort: o.Code.Effort},
		rework,
		{Role: "merge", Field: "merge.effort", Effort: o.Merge.Effort},
	}
}

func lastBlock(body string) (string, bool) {
	var last string
	found := false
	var fence markdownfence.Fence
	capture := false
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		previous := fence
		marker := fence.Consume(line)
		if previous == "" && fence != "" {
			fields := strings.Fields(strings.TrimSpace(line)[len(fence):])
			capture = len(fields) > 0 && fields[0] == "detent-agent"
			lines = lines[:0]
			continue
		}
		if marker && fence == "" {
			if capture {
				last = strings.Join(lines, "\n")
				found = true
			}
			capture = false
			continue
		}
		if capture {
			lines = append(lines, line)
		}
	}
	return last, found
}
