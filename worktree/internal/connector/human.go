package connector

import (
	"context"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

type HumanTask struct {
	Schema             int    `json:"schema" yaml:"schema"`
	Key                string `json:"key" yaml:"key"`
	Action             string `json:"action" yaml:"action"`
	Owner              string `json:"owner" yaml:"owner"`
	CompletionCriteria string `json:"completion_criteria" yaml:"completion_criteria"`
	ApprovalConstraint string `json:"approval_constraint" yaml:"approval_constraint"`
	CompletionEvidence string `json:"completion_evidence,omitempty" yaml:"completion_evidence,omitempty"`
}

type HumanPrerequisiteRequest struct {
	Title              string    `json:"title"`
	Task               HumanTask `json:"task"`
	ExistingIdentifier string    `json:"existing_identifier,omitempty"`
}

type HumanPrerequisiteResult struct {
	Issue   Issue `json:"issue"`
	Created bool  `json:"created"`
}

type HumanPrerequisiteWriter interface {
	EnsureHumanPrerequisite(context.Context, string, HumanPrerequisiteRequest) (HumanPrerequisiteResult, error)
}

func ParseHumanTask(body string) (HumanTask, bool, error) {
	var content strings.Builder
	fence := ""
	exampleFence := ""
	found, closed := false, false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if exampleFence != "" {
			if trimmed == exampleFence {
				exampleFence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```detent-human") || strings.HasPrefix(trimmed, "~~~detent-human") {
			if found {
				return HumanTask{}, true, errors.New("multiple detent-human blocks")
			}
			found = true
			fence = trimmed[:3]
			if trimmed != fence+"detent-human" {
				return HumanTask{}, true, errors.New("invalid detent-human fence")
			}
			continue
		}
		if (!found || closed) && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			end := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
			exampleFence = trimmed[:end]
			continue
		}
		if found && !closed {
			if trimmed == fence {
				closed = true
				continue
			}
			content.WriteString(line)
			content.WriteByte('\n')
		}
	}
	if !found {
		return HumanTask{}, false, nil
	}
	if !closed {
		return HumanTask{}, true, errors.New("unterminated detent-human block")
	}
	var task HumanTask
	decoder := yaml.NewDecoder(strings.NewReader(content.String()))
	decoder.KnownFields(true)
	if err := decoder.Decode(&task); err != nil {
		return HumanTask{}, true, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return HumanTask{}, true, errors.New("detent-human requires one YAML document")
	}
	return task, true, task.Validate()
}

func (task HumanTask) Validate() error {
	if task.Schema != 1 {
		return errors.New("detent-human schema must be 1")
	}
	for _, value := range []string{task.Key, task.Action, task.Owner, task.CompletionCriteria, task.ApprovalConstraint} {
		if strings.TrimSpace(value) == "" {
			return errors.New("detent-human requires key, action, owner, completion_criteria, and approval_constraint")
		}
	}
	return nil
}

func HumanOwned(issue Issue) bool {
	_, found, err := ParseHumanTask(issue.Description)
	return found || err != nil || issueHasLabel(issue, "human-owned")
}

func HumanPrerequisiteReady(issue Issue) bool {
	task, found, err := ParseHumanTask(issue.Description)
	return issue.Closed && found && err == nil && strings.TrimSpace(task.CompletionEvidence) != ""
}

func NonExecutableReason(issue Issue) string {
	if HumanOwned(issue) {
		return "human_owned"
	}
	if issueHasLabel(issue, "epic") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(issue.Title)), "epic:") {
		return "tracking_epic"
	}
	return ""
}

func issueHasLabel(issue Issue, want string) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}
