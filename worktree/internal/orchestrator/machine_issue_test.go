package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	state               string
	createErr, stateErr error
	states              int
}

func (s *machineIssueTracker) CreateIntakeIssue(_ context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	s.draft = draft
	return intake.Issue{ID: "issue", Identifier: "example/repo#1", Reused: s.reused, State: s.state}, s.createErr
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
		state                                  string
		reused, createFail, stateFail, success bool
		states                                 int
		cfg                                    Config
		labels                                 []string
		priority                               *int
	}{
		{name: "host routes Todo", state: "Todo", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, success: true},
		{name: "create", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, success: true, states: 1},
		{name: "prose priority stays unset", arguments: `{"title":"Disk","body":"Priority: High.","fingerprint":"disk"}`, success: true, states: 1},
		{name: "urgent", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":1}`, success: true, states: 1, priority: new(1)},
		{name: "high", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":2}`, success: true, states: 1, priority: new(2)},
		{name: "normal", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":3}`, success: true, states: 1, priority: new(3)},
		{name: "low", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":4}`, success: true, states: 1, priority: new(4)},
		{name: "reuse high", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":2}`, reused: true, success: true, priority: new(2)},
		{name: "priority zero", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":0}`},
		{name: "priority negative", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":-1}`},
		{name: "priority above low", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":5}`},
		{name: "priority fractional", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":2.5}`},
		{name: "priority name", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":"High"}`},
		{name: "priority boolean", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":true}`},
		{name: "priority object", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":{}}`},
		{name: "priority array", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":[2]}`},
		{name: "priority null", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":null}`},
		{name: "lane argument", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","priority":2,"state":"Todo"}`},
		{name: "empty labels", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":[]}`, success: true, states: 1},
		{name: "metadata", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":["bug","phase:design"]}`, success: true, states: 1, labels: []string{"bug", "phase:design"}},
		{name: "default lanes", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":[" DETENT:BACKLOG ","detent:todo","detent:metadata","phase:design"]}`, success: true, states: 1, cfg: Config{LaneSignalStates: []string{"Todo"}}, labels: []string{"detent:metadata", "phase:design"}},
		{name: "mapped custom lanes", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":[" Workflow:READY-FOR-BUILD ","workflow:todo","workflow:backlog","workflow:untriaged","workflow:metadata","detent:todo","bug"]}`, success: true, states: 1, cfg: Config{TrackerStatusLabelPrefix: " workflow: ", TrackerStateMap: map[string]string{" todo ": "Ready for Build", "Backlog": "Untriaged"}, LaneSignalStates: []string{"Todo"}}, labels: []string{"workflow:metadata", "detent:todo", "bug"}},
		{name: "supplied provenance", arguments: "{\"title\":\"Disk\",\"body\":\"Evidence\\n\\n```detent-origin\\norigin_kind: audit\\ninstance_identity: supplied\\nsource_ref: supplied\\nfingerprint: other\\n```\",\"fingerprint\":\"disk\"}", success: true, states: 1},
		{name: "reuse", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":["bug"]}`, reused: true, success: true, labels: []string{"bug"}},
		{name: "create failure", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, createFail: true},
		{name: "state failure", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk"}`, stateFail: true, states: 1},
		{name: "missing", arguments: `{}`},
		{name: "unknown field", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","extra":1}`},
		{name: "trailing", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk"} {}`},
		{name: "labels string", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":"bug"}`},
		{name: "labels object", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":{}}`},
		{name: "labels number", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":[1]}`},
		{name: "labels null", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":null}`},
		{name: "label null", arguments: `{"title":"Disk","body":"Evidence","fingerprint":"disk","labels":[null]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracker := &machineIssueTracker{Connector: memory.New(memory.Config{}), reused: tt.reused, state: tt.state}
			if tt.createFail {
				tracker.createErr = errors.New("create failed")
			}
			if tt.stateFail {
				tracker.stateErr = errors.New("state failed")
			}
			o := &Orchestrator{connector: tracker, cfg: tt.cfg}
			request := RunRequest{WorkAttemptID: 5187}
			o.attachMachineIssueTool(&request)
			result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "file_machine_issue", Arguments: json.RawMessage(tt.arguments)})
			if err != nil || result.Success != tt.success || tracker.states != tt.states {
				t.Fatalf("result = %+v, error = %v, states = %d", result, err, tracker.states)
			}
			if !tt.success && !tt.createFail && !tt.stateFail && tracker.draft.Title != "" {
				t.Fatalf("malformed request published draft = %+v", tracker.draft)
			}
			if tt.success {
				if (tracker.draft.Priority == nil) != (tt.priority == nil) || tt.priority != nil && *tracker.draft.Priority != *tt.priority {
					t.Fatalf("priority = %v, want %v", tracker.draft.Priority, tt.priority)
				}
				if !slices.Equal(tracker.draft.Labels, tt.labels) {
					t.Fatalf("labels = %v, want %v", tracker.draft.Labels, tt.labels)
				}
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
	var schema struct {
		AdditionalProperties bool
		Required             []string
		Properties           map[string]struct {
			Type    string
			Minimum int
			Maximum int
		}
	}
	if err := json.Unmarshal(request.AgentTools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	priority := schema.Properties["priority"]
	if schema.AdditionalProperties || slices.Contains(schema.Required, "priority") || priority.Type != "integer" || priority.Minimum != 1 || priority.Maximum != 4 {
		t.Fatalf("worker schema = %+v", schema)
	}
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
