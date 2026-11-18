package operatortool

// OperatorChatCatalog is the daemon's existing provider conversation. Native
// project conversations use WorkspaceCatalog; no browser cookie/session ID is input.
func OperatorChatCatalog() []Definition {
	definitions := []Definition{
		definition("get_operator_chat", "Read this connection's bounded operator chat history; never selects another browser session.", `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":200}},"additionalProperties":false}`),
		commandDefinition("post_operator_chat", "Send a message to the configured dashboard chat provider. Operator actions use the named command tools with current scoped authority.", `"message":{"type":"string","minLength":1,"maxLength":8192}`, `"message"`, false),
	}
	for i := range definitions {
		definitions[i].Meta = ToolMetadata{Toolset: "conversations_workspaces"}
	}
	return definitions
}
