package operatortool

import "encoding/json"

const OAuthGrantList = "list_oauth_grants"
const OAuthGrantRevoke = "revoke_oauth_grant"

type OAuthGrantListArguments struct {
	Cursor string `json:"cursor,omitempty"`
}

type OAuthGrantArguments struct {
	GrantID string `json:"grant_id"`
}

func OAuthGrantCatalog() []Definition {
	return []Definition{
		{Name: OAuthGrantList, Description: "List your connected assistant grants and their workspaces, connection times, last use and revocation times. Follow next_cursor to read the next page.", InputSchema: json.RawMessage(`{"type":"object","properties":{"cursor":{"type":"string","maxLength":128}},"additionalProperties":false}`), Annotations: Annotations{ReadOnly: true, Idempotent: true}, Meta: ToolMetadata{Toolset: "connection"}},
		{Name: OAuthGrantRevoke, Description: "Revoke one of your assistant grants. Its access and refresh tokens stop working immediately.", InputSchema: json.RawMessage(`{"type":"object","properties":{"grant_id":{"type":"string"}},"required":["grant_id"],"additionalProperties":false}`), Annotations: Annotations{Destructive: true, Idempotent: true}, Meta: ToolMetadata{Toolset: "connection"}},
	}
}
