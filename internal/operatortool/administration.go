package operatortool

import (
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
)

const (
	OrganizationSession = "organization_session"
	OrganizationList    = "organization_list"
	OrganizationSwitch  = "organization_switch"
	OrganizationCreate  = "organization_create"
	OrganizationDelete  = "organization_delete"
	InvitationAccept    = "invitation_accept"
	MembershipList      = "membership_list"
	InvitationSend      = "invitation_send"
	InvitationRevoke    = "invitation_revoke"
	MemberRemove        = "member_remove"
	MemberRole          = "member_role"
	MemberGrant         = "member_grant"
	CredentialList      = "credential_list"
	CredentialCreate    = "credential_create"
	CredentialRotate    = "credential_rotate"
	CredentialRevoke    = "credential_revoke"
	CredentialGrant     = "credential_grant"
	SupportStart        = "support_start"
)

// AdministrationCatalog describes application commands, never arbitrary paths
// or a model-selected identity/mode. Adapters advertise only installed services.
func AdministrationCatalog() []Definition {
	id := `{"type":"string","minLength":1,"maxLength":256}`
	role := `{"type":"string","enum":["owner","admin","member","viewer"]}`
	projects := `{"type":"array","maxItems":64,"items":` + id + `}`
	scopes := `{"type":"array","minItems":1,"maxItems":3,"items":{"type":"string","enum":["read","write","admin"]}}`
	return []Definition{
		adminDefinition(OrganizationSession, "Read fresh organization/session authority without credentials.", "", "", true, false),
		adminDefinition(OrganizationList, "List owned or joined organization contexts with fresh destinations.", `"offset":{"type":"integer","minimum":0,"maximum":100000},"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false),
		adminDefinition(MembershipList, "Read memberships, invitations and project grants visible to the current principal.", `"offset":{"type":"integer","minimum":0,"maximum":100000},"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false),
		adminDefinition(CredentialList, "Read credential metadata and grants; never returns credential hashes or tokens.", `"offset":{"type":"integer","minimum":0,"maximum":100000},"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false),
		adminDefinition(OrganizationSwitch, "Select an owned organization and return its fresh authenticated connection destination. Reconnect there; existing grants are never transferred.", `"organization_id":`+id, `"organization_id"`, false, false),
		adminDefinition(OrganizationCreate, "Create an organization through the deployment's existing provisioning command.", `"name":{"type":"string","minLength":1,"maxLength":120}`, `"name"`, false, false),
		adminDefinition(OrganizationDelete, "Delete the current owned organization after an exact operator preview.", `"organization_id":`+id+`,"confirm_name":{"type":"string","minLength":1,"maxLength":120}`, `"organization_id","confirm_name"`, false, true),
		adminDefinition(InvitationAccept, "Accept an invitation for the authenticated account and return a fresh login destination.", `"invitation_id":`+id, `"invitation_id"`, false, false),
		adminDefinition(InvitationSend, "Invite a member with the exact email and role after operator approval.", `"email":{"type":"string","minLength":1,"maxLength":254},"role":`+role, `"email","role"`, false, false),
		adminDefinition(InvitationRevoke, "Withdraw an existing pending organization invitation.", `"invitation_id":`+id, `"invitation_id"`, false, true),
		adminDefinition(MemberRemove, "Remove an organization member, preserving existing owner rules.", `"member_id":`+id, `"member_id"`, false, true),
		adminDefinition(MemberRole, "Change a member role, preserving existing ownership and last-owner rules.", `"member_id":`+id+`,"role":`+role, `"member_id","role"`, false, true),
		adminDefinition(MemberGrant, "Set or remove the exact member project grant.", `"member_id":`+id+`,"project_id":`+id+`,"write":{"type":"boolean"},"runner":{"type":"boolean"},"revoke":{"type":"boolean"}`, `"member_id","project_id"`, false, true),
		adminDefinition(CredentialCreate, "Create a credential with exact scopes, project grants and expiry. The credential is delivered only to this authenticated connection; durable retries omit it.", `"name":{"type":"string","minLength":1,"maxLength":120},"scopes":`+scopes+`,"project_ids":`+projects+`,"expires_in":{"type":"string","maxLength":32}`, `"name","scopes"`, false, false),
		adminDefinition(CredentialRotate, "Rotate an owned credential; the exact target and grace period require operator approval.", `"credential_id":`+id+`,"grace":{"type":"string","maxLength":32}`, `"credential_id"`, false, true),
		adminDefinition(CredentialRevoke, "Revoke an existing owned credential.", `"credential_id":`+id, `"credential_id"`, false, true),
		adminDefinition(CredentialGrant, "Add native credential organization/project grants without expanding platform powers.", `"credential_id":`+id+`,"project_ids":{"type":"array","minItems":1,"maxItems":64,"items":`+id+`}`, `"credential_id","project_ids"`, false, true),
		adminDefinition(SupportStart, "Enter existing support access only with the deployment's existing staff/support authority.", `"organization_id":`+id+`,"reason":{"type":"string","minLength":1,"maxLength":280}`, `"organization_id","reason"`, false, false),
	}
}

func adminDefinition(name, description, properties, required string, readOnly, destructive bool) Definition {
	if !readOnly {
		if required != "" {
			required += ","
		}
		required += `"request_id"`
		if properties != "" {
			properties += ","
		}
		properties += `"request_id":{"type":"string","minLength":1,"maxLength":128}`
	}
	schema := `{"type":"object","properties":{` + properties + `},"additionalProperties":false`
	if required != "" {
		schema += `,"required":[` + required + `]`
	}
	schema += `}`
	group := "organization"
	if strings.HasPrefix(name, "credential_") {
		group = "credentials"
	}
	return Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), Annotations: Annotations{ReadOnly: readOnly, Destructive: destructive, Idempotent: true, OpenWorld: true}, Meta: ToolMetadata{Toolset: group}}
}

func AdministrationScope(name string) apikey.Scope {
	switch name {
	case OrganizationSession, OrganizationList, MembershipList, CredentialList:
		return apikey.ScopeRead
	case OrganizationSwitch, InvitationAccept:
		return apikey.ScopeRead
	default:
		return apikey.ScopeAdmin
	}
}

func IsAdministration(name string) bool {
	for _, d := range AdministrationCatalog() {
		if d.Name == name {
			return true
		}
	}
	return false
}
