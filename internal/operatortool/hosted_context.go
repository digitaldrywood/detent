package operatortool

const (
	AppBootstrapPayload = "app_bootstrap_payload"
	AppUpdates          = "app_updates"
	HostedEvents        = "hosted_events"
)

func HostedContextCatalog() []Definition {
	bootstrap := definition(AppBootstrapPayload, "Read current hosted organization, actor, readable projects, features, preferences, plan and build context without browser secrets. Complete result or safe unavailable when bounds are exceeded.", `{"type":"object","properties":{},"additionalProperties":false}`)
	bootstrap.Meta = ToolMetadata{Toolset: "connection"}
	updates := definition(AppUpdates, "Read hub/client builds and current runner online/update context. Requires non-viewer authority and runner management on every project. Complete result or safe unavailable when bounds are exceeded.", `{"type":"object","properties":{},"additionalProperties":false}`)
	updates.Meta = ToolMetadata{Toolset: "connection"}
	events := definition(HostedEvents, "Observe the current project activity sequence and optional workspace revision once. Cursor compares observations; no heartbeat, event replay, streaming or background polling. Fresh project authority is required on every call.", `{"type":"object","required":["project_id"],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"workspace_id":{"type":"string","minLength":1,"maxLength":256},"cursor":{"type":"string","minLength":1,"maxLength":2048}},"additionalProperties":false}`)
	events.Meta = ToolMetadata{Toolset: "work_reads"}
	definitions := []Definition{bootstrap, updates, events}
	for i := range definitions {
		definitions[i].Annotations.OpenWorld = true
	}
	return definitions
}

func IsHostedContext(name string) bool {
	return name == AppBootstrapPayload || name == AppUpdates || name == HostedEvents
}
