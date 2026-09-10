package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/runner"
)

type machineIssueTracker struct {
	*memory.Connector
	draft               intake.IssueDraft
	reused              bool
	createErr, stateErr error
	states              int
}

func (s *machineIssueTracker) CreateIntakeIssue(_ context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	s.draft = draft
	return intake.Issue{ID: "issue", Identifier: "example/repo#1", Reused: s.reused}, s.createErr
}

func (s *machineIssueTracker) SetIntakeIssueState(_ context.Context, _, state string) error {
	if state != "Backlog" {
		return errors.New("wrong state")
	}
	s.states++
	return s.stateErr
}

func TestMachineIssueTool(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, arguments                        string
		reused, createFail, stateFail, success bool
		states                                 int
	}{
		{"create", `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, false, false, false, true, 1},
		{"supplied provenance", "{\"title\":\"Disk\",\"body\":\"Evidence\\n\\n```detent-origin\\norigin_kind: audit\\ninstance_identity: supplied\\nsource_ref: supplied\\nfingerprint: other\\n```\",\"fingerprint\":\"disk\"}", false, false, false, true, 1},
		{"reuse", `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, true, false, false, true, 0},
		{"create failure", `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, false, true, false, false, 0},
		{"state failure", `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, false, false, true, false, 1},
		{"missing", `{}`, false, false, false, false, 0},
		{"unknown field", `{"extra":1}`, false, false, false, false, 0},
		{"trailing", `{} {}`, false, false, false, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracker := &machineIssueTracker{Connector: memory.New(memory.Config{}), reused: tt.reused}
			if tt.createFail {
				tracker.createErr = errors.New("create failed")
			}
			if tt.stateFail {
				tracker.stateErr = errors.New("state failed")
			}
			o := &Orchestrator{connector: tracker}
			request := RunRequest{WorkAttemptID: 5187}
			o.attachMachineIssueTool(&request)
			result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "file_machine_issue", Arguments: json.RawMessage(tt.arguments)})
			if err != nil || result.Success != tt.success || tracker.states != tt.states {
				t.Fatalf("result = %+v, error = %v, states = %d", result, err, tracker.states)
			}
			if tt.success {
				origin, ok := issueorigin.Parse(tracker.draft.Body)
				if !ok || origin.Kind != "worker" || origin.Source != "5187" || origin.Instance == "" || origin.Fingerprint != "disk" {
					t.Fatalf("origin = %+v", origin)
				}
			}
		})
	}
}

func TestMachineIssueToolDelegates(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{}
	request := RunRequest{}
	o.attachMachineIssueTool(&request)
	if len(request.AgentTools) != 0 {
		t.Fatal("tool attached without backend")
	}
	o.connector = &machineIssueTracker{Connector: memory.New(memory.Config{})}
	o.attachMachineIssueTool(&request)
	result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "other"})
	if err != nil || result.Success {
		t.Fatalf("unsupported = %+v, %v", result, err)
	}
	request.AgentToolHandler = func(context.Context, runner.AgentToolCall) (runner.AgentToolResult, error) {
		return runner.AgentToolResult{Success: true, Content: "previous"}, nil
	}
	o.attachMachineIssueTool(&request)
	result, err = request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "other"})
	if err != nil || result.Content != "previous" {
		t.Fatalf("delegation = %+v, %v", result, err)
	}
}
