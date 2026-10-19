package operatortool

const (
	AppBootstrapPayload = "app_bootstrap_payload"
	AppUpdates          = "app_updates"
	ReleaseNotes        = "release_notes"
	HostedEvents        = "hosted_events"
)

type ReleaseNotesArguments struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func HostedContextCatalog() []Definition {
	bootstrap := definition(AppBootstrapPayload, "Read current hosted organization, actor, readable projects, features, preferences, plan and build context without browser secrets. Complete result or safe unavailable when bounds are exceeded.", `{"type":"object","properties":{},"additionalProperties":false}`)
	bootstrap.Meta = ToolMetadata{Toolset: "connection"}
	updates := definition(AppUpdates, "Read hub/client builds and current runner online/update context. Requires non-viewer authority and runner management on every project. Complete result or safe unavailable when bounds are exceeded.", `{"type":"object","properties":{},"additionalProperties":false}`)
	updates.Meta = ToolMetadata{Toolset: "connection"}
	events := definition(HostedEvents, "Observe the current project activity sequence and optional workspace revision once. Cursor compares observations; no heartbeat, event replay, streaming or background polling. Fresh project authority is required on every call.", `{"type":"object","required":["project_id"],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"workspace_id":{"type":"string","minLength":1,"maxLength":256},"cursor":{"type":"string","minLength":1,"maxLength":2048}},"additionalProperties":false}`)
	events.Meta = ToolMetadata{Toolset: "work_reads"}
	notes := definition(ReleaseNotes, "Read embedded release notes newest first, including per-user unread state. Cursor paginates older builds. No external service is contacted.", `{"type":"object","properties":{"cursor":{"type":"string","maxLength":512},"limit":{"type":"integer","minimum":0,"maximum":100}},"additionalProperties":false}`)
	notes.Meta = ToolMetadata{Toolset: "connection"}
	definitions := []Definition{bootstrap, updates, events, notes}
	for i := range definitions {
		definitions[i].Annotations.OpenWorld = true
	}
	return definitions
}

func IsHostedContext(name string) bool {
	return name == ReleaseNotes || name == AppBootstrapPayload || name == AppUpdates || name == HostedEvents
}
