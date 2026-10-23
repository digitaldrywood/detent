package operatortool

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	MoveItem       = "move_item"
	SetPriority    = "set_priority"
	StopRun        = "stop_run"
	FileIssue      = "file_issue"
	ActionResult   = "action_result"
	ConnectionInfo = "connection_info"

	workRevisionProperty = `"expected_revision":{"type":"string","minLength":1,"maxLength":19,"pattern":"^[1-9][0-9]*$"}`
)

// CommandCatalog is separate from the five preserved shared read definitions.
// A deployment advertises commands only when its application adapter exists.
// request_id is an explicit business idempotency key, reused across reconnects;
// it is independent of JSON-RPC request IDs and is never an approval token.
func CommandCatalog() []Definition {
	return append(append(append([]Definition{
		commandDefinition(MoveItem, "Request an item's configured workflow transition through the application owner. Read get_native_project for native states and allowed transitions; native requests require expected_revision. Optional reason_detail is plain text recorded on the transition, for example why an item moved to Blocked and what a human must do. Authorized transitions execute directly, including terminal states.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"target_state":{"type":"string","minLength":1,"maxLength":256},"reason_detail":{"type":"string","maxLength":`+strconv.Itoa(workpad.MaxFinalSummaryBytes)+`},`+workRevisionProperty, `"identifier","target_state"`, true),
		commandDefinition(SetPriority, "Set an item's configured priority directly through the dashboard command.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"priority":{"type":"string","minLength":1,"maxLength":256},`+workRevisionProperty, `"identifier","priority"`, false),
		stopRunDefinition(),
		fileIssueDefinition(),
		definition(ActionResult, "Read the outcome of this connection's exact action. This never approves an action.", `{"type":"object","required":["action_id"],"properties":{"action_id":{"type":"string","minLength":1,"maxLength":256}},"additionalProperties":false}`),
		definition(ConnectionInfo, "Read this connection's authenticated identity and organization.", `{"type":"object","properties":{},"additionalProperties":false}`),
	}, WorkCatalog()...), BillingCatalog()...), FleetCatalog()...)
}

type FileIssueArguments struct {
	ChallengeToken string                 `json:"challenge_token,omitempty"`
	NotDuplicateOf []int                  `json:"not_duplicate_of,omitempty"`
	ProjectID      string                 `json:"project_id"`
	GitHubIssueURL string                 `json:"github_issue_url,omitempty"`
	Type           string                 `json:"type,omitempty"`
	ReportContext  *tracker.ReportContext `json:"report_context,omitempty"`
	Title          string                 `json:"title,omitempty"`
	Description    string                 `json:"description,omitempty"`
	State          string                 `json:"state,omitempty"`
	Labels         []string               `json:"labels,omitempty"`
	Priority       *int                   `json:"priority,omitempty"`
}

func fileIssueDefinition() Definition {
	return Definition{Name: FileIssue, Description: "Create an issue through the dashboard command. Native filing may return possible_duplicates and a challenge_token without creating an issue. Review the returned candidates, using supported reads such as work_item and work_comments when needed, and explicitly decide whether they concern the same problem. If filing is already authorized and the evidence clearly establishes a distinct problem, repeat file_issue without another human question with the same request_id and content, the challenge_token, and not_duplicate_of containing the challenged issue numbers to link as related. For the same problem, use the existing issue and call add_comment with occurrence details and any screenshot attachment references within the caller's authorization; do not create a duplicate. Ask the human only when overlap is genuinely ambiguous, a product decision is needed, or the proposed action exceeds authorization. Do not automatically dismiss candidates. Challenge tokens expire after one hour; after expiry, begin a new filing with a new request_id. A completed request replay returns its original result. Native descriptions may be empty for non-Reporters. Type is bug or change_request and is required for Reporters, who file into Triage. Reporter descriptions require nonempty sections. Bug template: ## Steps to reproduce, ## Expected, ## Actual, ## Where (screen or URL), ## Environment (browser/OS and app build; use available context). Change request template: ## Current behavior, ## Desired behavior, ## Why. Call work_config for effective reporter_templates; a project WORKFLOW Reporter Contract may override headings. Engineering issue sections are required when promoted from Triage into executable work. Native projects may link a permitted github_issue_url without title or description. Priority ranks 1–4 map to native priorities 0–3. Embed upload_attachment reference in description to attach a file. When filing a bug from a browser, fill report_context with the page url, app_build, browser, os, viewport and captured_at the tester can see; browser and app_build satisfy the bug Environment section. When the tester has a network HAR export (.har), upload it with detent attach or the REST attachment upload API, since upload_attachment accepts only about 44 KB; use upload_attachment only for small console log exports (.log). Embed each attachment reference in the description.", InputSchema: json.RawMessage(`{"type":"object","required":["project_id","request_id"],"anyOf":[{"required":["title"],"properties":{"title":{"minLength":1}}},{"required":["github_issue_url"]}],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"request_id":{"type":"string","minLength":1,"maxLength":128},"github_issue_url":{"type":"string","minLength":1,"maxLength":2048},"type":{"type":"string","enum":["bug","change_request"]},"report_context":{"type":"object","properties":{"url":{"type":"string","maxLength":2048},"app_build":{"type":"string","maxLength":256},"browser":{"type":"string","maxLength":256},"os":{"type":"string","maxLength":256},"viewport":{"type":"string","maxLength":256},"captured_at":{"type":"string","maxLength":256}},"additionalProperties":false},"title":{"type":"string","maxLength":500},"description":{"type":"string","maxLength":32768},"state":{"type":"string","maxLength":256},"labels":{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":200}},"priority":{"type":"integer","minimum":1,"maximum":4},"challenge_token":{"type":"string","minLength":1,"maxLength":128},"not_duplicate_of":{"type":"array","minItems":1,"maxItems":5,"uniqueItems":true,"items":{"type":"integer","minimum":1}}},"additionalProperties":false}`), Annotations: Annotations{Idempotent: true, OpenWorld: true}, Meta: toolset(FileIssue)}
}

func DecodeFileIssue(raw json.RawMessage) (FileIssueArguments, error) {
	var request FileIssueArguments
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil && fields["report_context"] != nil {
		if _, err := tracker.DecodeReportContext(fields["report_context"]); err != nil {
			return request, invalidArgument("report_context", "accepts only url, app_build, browser, os, viewport and captured_at as bounded single-line strings")
		}
	}
	if DecodeArguments(raw, &request) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return request, ErrInvalidArguments
	}
	if request.ReportContext != nil {
		request.ReportContext = request.ReportContext.Normalized()
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
	if fields["type"] != nil && request.Type != "bug" && request.Type != "change_request" {
		return request, invalidArgument("type", "must be bug or change_request")
	}
	if request.GitHubIssueURL != "" {
		if _, _, _, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL); err != nil {
			return request, ErrInvalidArguments
		}
	}
	if len(request.ChallengeToken) > 128 || len(request.NotDuplicateOf) > 5 || (request.ChallengeToken == "") != (len(request.NotDuplicateOf) == 0) {
		return request, ErrInvalidArguments
	}
	for _, number := range request.NotDuplicateOf {
		if number <= 0 {
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
	ReasonDetail     string `json:"reason_detail,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,string"`
}

type SetPriorityArguments struct {
	ExpectedRevision int64  `json:"expected_revision,string"`
	ProjectID        string `json:"project_id"`
	Identifier       string `json:"identifier"`
	Priority         string `json:"priority"`
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
	request.ReasonDetail = strings.TrimSpace(request.ReasonDetail)
	if _, ok := workpad.LastStatusBlock(request.ReasonDetail); ok {
		return request, invalidArgument("reason_detail", "must be plain text without a status block")
	}
	return request, nil
}

type NativeStopRunArguments struct {
	ProjectID        string               `json:"project_id"`
	RequestID        string               `json:"request_id,omitempty"`
	Identifier       string               `json:"identifier"`
	ExpectedRevision tracker.Revision     `json:"expected_revision,string"`
	LeaseID          tracker.LeaseID      `json:"lease_id"`
	FencingToken     tracker.FencingToken `json:"fencing_token"`
	Destination      string               `json:"destination"`
	Priority         *int                 `json:"priority,omitempty"`
	Reason           string               `json:"reason,omitempty"`
}

func stopRunDefinition() Definition {
	return commandDefinition(StopRun, "Stop the exact active run and route its item with current write authority. Native Hub calls require expected_revision, lease_id and fencing_token from the current attempt. Native stops preserve retained source and return an action_id readable with action_result; the receipt identifies the released lease and resulting item state.", `"identifier":{"type":"string","minLength":1,"maxLength":256},`+workRevisionProperty+`,"lease_id":{"type":"string","minLength":1,"maxLength":256},"fencing_token":{"type":"integer","minimum":1},"destination":{"type":"string","enum":["Blocked","Backlog","Cancelled","Todo"]},"priority":{"type":"integer","minimum":1,"maximum":4},"reason":{"type":"string","maxLength":280}`, `"identifier","destination"`, true)
}

func DecodeNativeStopRun(raw json.RawMessage) (NativeStopRunArguments, error) {
	var request NativeStopRunArguments
	var schema argumentSchema
	if err := json.Unmarshal(stopRunDefinition().InputSchema, &schema); err != nil {
		return request, err
	}
	schema.Required = append(schema.Required, "expected_revision", "lease_id", "fencing_token")
	if err := validateArgumentSchema(raw, schema); err != nil {
		return request, err
	}
	if err := DecodeArguments(raw, &request); err != nil {
		return request, err
	}
	if request.ExpectedRevision <= 0 || request.FencingToken <= 0 {
		return request, ErrInvalidArguments
	}
	for _, value := range []string{request.ProjectID, request.RequestID, request.Identifier, string(request.LeaseID)} {
		if strings.TrimSpace(value) == "" {
			return request, ErrInvalidArguments
		}
	}
	return request, nil
}
