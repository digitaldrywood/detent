package operatortool

import (
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	MoveItem       = "move_item"
	SetPriority    = "set_priority"
	StopRun        = "stop_run"
	FileIssue      = "file_issue"
	ActionResult   = "action_result"
	ConnectionInfo = "connection_info"
)

// CommandCatalog is separate from the five preserved shared read definitions.
// A deployment advertises commands only when its application adapter exists.
// request_id is an explicit business idempotency key, reused across reconnects;
// it is independent of JSON-RPC request IDs and is never an approval token.
func CommandCatalog() []Definition {
	return append(append(append([]Definition{
		commandDefinition(MoveItem, "Request an item's configured workflow transition through the application owner. Read get_native_project for native states and allowed transitions; native requests require expected_revision. Authorized transitions execute directly, including terminal states.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"target_state":{"type":"string","minLength":1,"maxLength":256},"expected_revision":{"type":"integer","minimum":1}`, `"identifier","target_state"`, true),
		commandDefinition(SetPriority, "Set an item's configured priority directly through the dashboard command.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"priority":{"type":"string","minLength":1,"maxLength":256},"expected_revision":{"type":"integer","minimum":1}`, `"identifier","priority"`, false),
		commandDefinition(StopRun, "Stop the exact active run and route its item. Executes directly with current write authority.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"destination":{"type":"string","enum":["Blocked","Backlog","Cancelled","Todo"]},"priority":{"type":"integer","minimum":1,"maximum":4},"reason":{"type":"string","maxLength":280}`, `"identifier","destination"`, true),
		fileIssueDefinition(),
		definition(ActionResult, "Read the outcome of this connection's exact action. This never approves an action.", `{"type":"object","required":["action_id"],"properties":{"action_id":{"type":"string","minLength":1,"maxLength":256}},"additionalProperties":false}`),
		definition(ConnectionInfo, "Read this connection's authenticated identity and organization.", `{"type":"object","properties":{},"additionalProperties":false}`),
	}, WorkCatalog()...), BillingCatalog()...), FleetCatalog()...)
}

type FileIssueArguments struct {
	ProjectID      string   `json:"project_id"`
	GitHubIssueURL string   `json:"github_issue_url,omitempty"`
	Title          string   `json:"title,omitempty"`
	Description    string   `json:"description,omitempty"`
	State          string   `json:"state,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	Priority       *int     `json:"priority,omitempty"`
}

func fileIssueDefinition() Definition {
	return Definition{Name: FileIssue, Description: "Create an issue through the dashboard command. Native descriptions may be empty. Native projects may link a permitted github_issue_url without title or description. Priority ranks 1–4 map to native priorities 0–3. Embed upload_attachment reference in description to attach a file.", InputSchema: json.RawMessage(`{"type":"object","required":["project_id","request_id"],"anyOf":[{"required":["title"],"properties":{"title":{"minLength":1}}},{"required":["github_issue_url"]}],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"request_id":{"type":"string","minLength":1,"maxLength":128},"github_issue_url":{"type":"string","minLength":1,"maxLength":2048},"title":{"type":"string","maxLength":500},"description":{"type":"string","maxLength":32768},"state":{"type":"string","maxLength":256},"labels":{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":200}},"priority":{"type":"integer","minimum":1,"maximum":4}},"additionalProperties":false}`), Annotations: Annotations{Idempotent: true, OpenWorld: true}, Meta: toolset(FileIssue)}
}

func DecodeFileIssue(raw json.RawMessage) (FileIssueArguments, error) {
	var request FileIssueArguments
	var fields map[string]json.RawMessage
	if DecodeArguments(raw, &request) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return request, ErrInvalidArguments
	}
	for _, value := range fields {
		if string(value) == "null" {
			return request, ErrInvalidArguments
		}
	}
	if strings.TrimSpace(request.ProjectID) == "" || len(request.ProjectID) > 256 || len(request.GitHubIssueURL) > 2048 || len(request.Title) > 500 || len(request.Description) > 32768 || len(request.State) > 256 || len(request.Labels) > 64 {
		return request, ErrInvalidArguments
	}
	if strings.TrimSpace(request.GitHubIssueURL) == "" && strings.TrimSpace(request.Title) == "" {
		return request, ErrInvalidArguments
	}
	if fields["github_issue_url"] != nil && strings.TrimSpace(request.GitHubIssueURL) == "" || request.Priority != nil && (*request.Priority < 1 || *request.Priority > 4) {
		return request, ErrInvalidArguments
	}
	if request.GitHubIssueURL != "" {
		if _, _, _, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL); err != nil {
			return request, ErrInvalidArguments
		}
	}
	for _, label := range request.Labels {
		if strings.TrimSpace(label) == "" || len(label) > 200 {
			return request, ErrInvalidArguments
		}
	}
	return request, nil
}

func commandDefinition(name, description, properties, required string, destructive bool) Definition {
	schema := `{"type":"object","required":["project_id","request_id",` + required + `],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"request_id":{"type":"string","description":"Business idempotency key; reuse for the same operation across reconnects. Independent of JSON-RPC id.","minLength":1,"maxLength":128},` + properties + `},"additionalProperties":false}`
	return Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), Annotations: Annotations{Destructive: destructive, Idempotent: true, OpenWorld: true}, Meta: toolset(name)}
}

type MoveItemArguments struct {
	ProjectID        string `json:"project_id"`
	RequestID        string `json:"request_id,omitempty"`
	Identifier       string `json:"identifier"`
	TargetState      string `json:"target_state"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func DecodeNativeMoveItem(raw json.RawMessage) (MoveItemArguments, error) {
	var request MoveItemArguments
	definition, _ := Lookup(MoveItem)
	var schema argumentSchema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return request, err
	}
	schema.Required = append(schema.Required, "expected_revision")
	if err := validateArgumentSchema(raw, schema); err != nil {
		return request, err
	}
	if err := DecodeArguments(raw, &request); err != nil {
		return request, err
	}
	for field, value := range map[string]string{"project_id": request.ProjectID, "identifier": request.Identifier, "target_state": request.TargetState, "request_id": request.RequestID} {
		if strings.TrimSpace(value) == "" {
			return request, invalidArgument(field, "must not be blank")
		}
	}
	return request, nil
}
