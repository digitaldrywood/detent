package operatortool

import (
	"encoding/json"
	"strings"
)

const (
	EditItem               = "edit_item"
	AddComment             = "add_comment"
	EditComment            = "edit_comment"
	DeleteComment          = "delete_comment"
	SetDependency          = "set_dependency"
	ArchiveItem            = "archive_item"
	RestoreItem            = "restore_item"
	RemoveItem             = "remove_item"
	ListComments           = "list_comments"
	WorkflowTimeline       = "workflow_timeline"
	AcknowledgeParks       = "acknowledge_parks"
	DisposeSecurityFinding = "dispose_security_finding"
	OrderItem              = "order_item"
	SetQueuePriority       = "set_queue_priority"
)

// WorkArguments are application inputs only. Connection authority, actor,
// approval and native worker fencing cannot be supplied by a tool caller.
type WorkArguments struct {
	ProjectID        string    `json:"project_id"`
	Identifier       string    `json:"identifier"`
	ExpectedRevision int64     `json:"expected_revision,omitempty"`
	Title            *string   `json:"title,omitempty"`
	Body             *string   `json:"body,omitempty"`
	Labels           *[]string `json:"labels,omitempty"`
	Priority         *int      `json:"priority,omitempty"`
	Target           string    `json:"target,omitempty"`
	CommentID        string    `json:"comment_id,omitempty"`
	Related          string    `json:"related,omitempty"`
	Operation        string    `json:"operation,omitempty"`
	Cursor           string    `json:"cursor,omitempty"`
	Limit            int       `json:"limit,omitempty"`
	QueuePriority    string    `json:"queue_priority,omitempty"`
	Scope            string    `json:"queue_scope,omitempty"`
	State            string    `json:"state,omitempty"`
	Rank             string    `json:"rank,omitempty"`
	Repository       string    `json:"repository,omitempty"`
	PullRequest      int       `json:"pull_request,omitempty"`
	BaseSHA          string    `json:"base_sha,omitempty"`
	HeadSHA          string    `json:"head_sha,omitempty"`
	FindingID        string    `json:"finding_id,omitempty"`
	Evidence         string    `json:"evidence,omitempty"`
}

func WorkCatalog() []Definition {
	selector := `"identifier":{"type":"string","minLength":1,"maxLength":256}`
	revision := `,"expected_revision":{"type":"integer","minimum":1}`
	body := `,"body":{"type":"string","maxLength":32768}`
	comment := `,"comment_id":{"type":"string","minLength":1,"maxLength":256}`
	definitions := []Definition{
		commandDefinition(SetQueuePriority, "Set a compatibility work-item queue priority using the existing dashboard command.", selector+`,"queue_scope":{"type":"string","minLength":1,"maxLength":256},"state":{"type":"string","minLength":1,"maxLength":256},"queue_priority":{"type":"string","enum":["urgent","high","normal","low","none"]}`, `"identifier","queue_scope","state","queue_priority"`, false),
		commandDefinition(EditItem, "Edit native work-item content, labels and priority with an expected revision. Clearing content requires operator approval. GitHub-backed editing is unavailable when the dashboard does not offer it.", selector+revision+body+`,"title":{"type":"string","minLength":1,"maxLength":500},"labels":{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":200}},"priority":{"type":"integer","minimum":0,"maximum":3}`, `"identifier","expected_revision"`, true),
		commandDefinition(AddComment, "Add an issue or supported pull-request comment through the dashboard application command. Embed upload_attachment reference in body to attach a file.", selector+body+`,"target":{"type":"string","enum":["issue","pr"]},"repository":{"type":"string","minLength":1,"maxLength":256},"pull_request":{"type":"integer","minimum":1}`, `"identifier","body"`, false),
		commandDefinition(EditComment, "Edit a permitted comment; native comments require their expected revision.", selector+revision+body+comment, `"identifier","comment_id","body"`, false),
		commandDefinition(DeleteComment, "Delete a permitted local comment after real operator approval. Native comment deletion is unavailable.", selector+comment, `"identifier","comment_id"`, true),
		commandDefinition(SetDependency, "Add or remove a dashboard-supported dependency. Native requests require the issue's expected revision.", selector+revision+`,"related":{"type":"string","minLength":1,"maxLength":256},"operation":{"type":"string","enum":["add","remove"]}`, `"identifier","related","operation"`, false),
		commandDefinition(ArchiveItem, "Archive native work after operator approval. Active work must be finished or stopped.", selector+revision, `"identifier","expected_revision"`, true),
		commandDefinition(RestoreItem, "Restore archived native work with an expected revision.", selector+revision, `"identifier","expected_revision"`, false),
		commandDefinition(RemoveItem, "Remove board membership through the orchestrator after operator approval; tracker capability restrictions remain.", selector, `"identifier"`, true),
		commandDefinition(AcknowledgeParks, "Acknowledge the item's existing park summary through the dashboard command.", selector, `"identifier"`, false),
		commandDefinition(DisposeSecurityFinding, "Record a false-positive disposition for the exact trusted security finding after operator approval.", selector+`,"repository":{"type":"string","minLength":1,"maxLength":256},"pull_request":{"type":"integer","minimum":1},"base_sha":{"type":"string","minLength":1,"maxLength":256},"head_sha":{"type":"string","minLength":1,"maxLength":256},"finding_id":{"type":"string","minLength":1,"maxLength":256},"evidence":{"type":"string","minLength":1,"maxLength":4096}`, `"identifier","repository","pull_request","base_sha","head_sha","finding_id","evidence"`, true),
		commandDefinition(OrderItem, "Set queue ordering where the deployment dashboard offers it.", selector+`,"queue_scope":{"type":"string","minLength":1,"maxLength":256},"state":{"type":"string","minLength":1,"maxLength":256},"rank":{"type":"string","minLength":1,"maxLength":256}`, `"identifier","queue_scope","state","rank"`, false),
	}
	for _, name := range []string{ListComments, WorkflowTimeline} {
		definitions = append(definitions, definition(name, "Read bounded issue discussion or workflow history through the dashboard application read.", `{"type":"object","required":["project_id","identifier"],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},`+selector+`,"cursor":{"type":"string","maxLength":2048},"limit":{"type":"integer","minimum":1,"maximum":200}},"additionalProperties":false}`))
	}
	for i := range definitions {
		if definitions[i].Name == ListComments {
			definitions[i].Annotations.OpenWorld = true
		}
	}
	return definitions
}

func IsWorkTool(name string) bool {
	for _, definition := range WorkCatalog() {
		if definition.Name == name {
			return true
		}
	}
	return false
}

// DecodeWorkArguments enforces the exact tool's fields and bounds even when
// discovery is bypassed. JSON schema remains documentation, never authority.
func DecodeWorkArguments(name string, raw json.RawMessage) (WorkArguments, error) {
	var request WorkArguments
	if err := DecodeArguments(raw, &request); err != nil {
		return request, err
	}
	var definition Definition
	for _, candidate := range WorkCatalog() {
		if candidate.Name == name {
			definition = candidate
			break
		}
	}
	if definition.Name == "" {
		return request, ErrUnknownTool
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(definition.InputSchema, &schema) != nil || json.Unmarshal(raw, &fields) != nil {
		return request, ErrInvalidArguments
	}
	// The shared submission layer removes the business key before proposals.
	for _, required := range schema.Required {
		if required != "request_id" && fields[required] == nil {
			return request, ErrInvalidArguments
		}
	}
	for key, value := range fields {
		if schema.Properties[key] == nil || string(value) == "null" {
			return request, ErrInvalidArguments
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			limit := 256
			if key == "title" {
				limit = 500
			}
			if key == "body" {
				limit = 32768
			}
			if key == "evidence" {
				limit = 4096
			}
			if key == "cursor" {
				limit = 2048
			}
			if len(text) > limit || key != "body" && key != "cursor" && strings.TrimSpace(text) == "" {
				return request, ErrInvalidArguments
			}
		}
	}
	if strings.TrimSpace(request.ProjectID) == "" || strings.TrimSpace(request.Identifier) == "" || request.ExpectedRevision < 0 || request.Limit < 0 || request.Limit > 200 || request.PullRequest < 0 {
		return request, ErrInvalidArguments
	}
	if fields["expected_revision"] != nil && request.ExpectedRevision <= 0 || fields["limit"] != nil && request.Limit <= 0 || fields["pull_request"] != nil && request.PullRequest <= 0 {
		return request, ErrInvalidArguments
	}
	if request.Priority != nil && (*request.Priority < 0 || *request.Priority > 3) {
		return request, ErrInvalidArguments
	}
	if request.Labels != nil {
		if len(*request.Labels) > 64 {
			return request, ErrInvalidArguments
		}
		for _, label := range *request.Labels {
			if strings.TrimSpace(label) == "" || len(label) > 200 {
				return request, ErrInvalidArguments
			}
		}
	}
	if name == EditItem && request.Title == nil && request.Body == nil && request.Labels == nil && request.Priority == nil || name == SetDependency && request.Operation != "add" && request.Operation != "remove" {
		return request, ErrInvalidArguments
	}
	if (name == AddComment || name == EditComment) && (request.Body == nil || strings.TrimSpace(*request.Body) == "") {
		return request, ErrInvalidArguments
	}
	if name == AddComment && (request.Target != "" && request.Target != "issue" && request.Target != "pr" || request.Target == "pr" && (request.Repository == "" || request.PullRequest <= 0) || request.Target != "pr" && (request.Repository != "" || request.PullRequest != 0)) {
		return request, ErrInvalidArguments
	}
	if name == SetQueuePriority {
		switch request.QueuePriority {
		case "urgent", "high", "normal", "low", "none":
		default:
			return request, ErrInvalidArguments
		}
	}
	return request, nil
}
