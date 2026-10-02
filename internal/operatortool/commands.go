package operatortool

import "encoding/json"

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
		commandDefinition(MoveItem, "Move an item through the dashboard command. Moves between Todo and Backlog execute directly; material actions return a browser approval preview.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"target_state":{"type":"string","minLength":1,"maxLength":256},"expected_revision":{"type":"integer","minimum":1}`, `"identifier","target_state"`, true),
		commandDefinition(SetPriority, "Set an item's configured priority directly through the dashboard command.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"priority":{"type":"string","minLength":1,"maxLength":256},"expected_revision":{"type":"integer","minimum":1}`, `"identifier","priority"`, false),
		commandDefinition(StopRun, "Stop the exact active run and route its item. Requires real operator approval unless the operator selected YOLO for this connection.", `"identifier":{"type":"string","minLength":1,"maxLength":256},"destination":{"type":"string","enum":["Blocked","Backlog","Cancelled","Todo"]},"priority":{"type":"integer","minimum":1,"maximum":4},"reason":{"type":"string","maxLength":280}`, `"identifier","destination"`, true),
		commandDefinition(FileIssue, "Create an issue using the shared dashboard application command. Embed upload_attachment reference in description to attach a file.", `"title":{"type":"string","minLength":1,"maxLength":256},"description":{"type":"string","minLength":1,"maxLength":32768},"state":{"type":"string","maxLength":256},"labels":{"type":"array","maxItems":64,"items":{"type":"string","maxLength":256}},"priority":{"type":"integer","minimum":1,"maximum":4}`, `"title","description"`, false),
		definition(ActionResult, "Read the outcome of this connection's exact action. This never approves an action.", `{"type":"object","required":["action_id"],"properties":{"action_id":{"type":"string","minLength":1,"maxLength":256}},"additionalProperties":false}`),
		definition(ConnectionInfo, "Read this connection's operator-controlled confirmation mode and dashboard setup URL. Tool input cannot change mode.", `{"type":"object","properties":{},"additionalProperties":false}`),
	}, WorkCatalog()...), BillingCatalog()...), FleetCatalog()...)
}

func commandDefinition(name, description, properties, required string, destructive bool) Definition {
	schema := `{"type":"object","required":["project_id","request_id",` + required + `],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"request_id":{"type":"string","description":"Business idempotency key; reuse for the same operation across reconnects. Independent of JSON-RPC id.","minLength":1,"maxLength":128},` + properties + `},"additionalProperties":false}`
	return Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), Annotations: Annotations{Destructive: destructive, Idempotent: true, OpenWorld: true}, Meta: toolset(name)}
}
