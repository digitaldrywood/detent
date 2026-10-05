package operatortool

import (
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
)

const (
	OrganizationSession = "organization_session"
	SessionLogout       = "session_logout"
	OrganizationList    = "organization_list"
	OrganizationSwitch  = "organization_switch"
	OrganizationCreate  = "organization_create"
	OrganizationDelete  = "organization_delete"
	ProvisioningPage    = "provisioning_page"
	ResumeProvisioning  = "resume_provisioning"
	InvitationAccept    = "invitation_accept"
	MembershipList      = "membership_list"
	InvitationSend      = "invitation_send"
	InvitationEdit      = "invitation_edit"
	InvitationResend    = "invitation_resend"
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
	projects := `{"type":"array","maxItems":200,"items":` + id + `}`
	grants := `{"type":"array","maxItems":200,"items":{"type":"object","properties":{"project_id":` + id + `,"write":{"type":"boolean"},"runner":{"type":"boolean"}},"required":["project_id"],"additionalProperties":false}}`
	scopes := `{"type":"array","minItems":1,"maxItems":3,"items":{"type":"string","enum":["read","write","admin"]}}`
	return []Definition{
		adminDefinition(SessionLogout, "End the current account session and provider sessions with current authority. Later calls and retries are denied; sign in and reconnect for new access.", "", "", false, true),
		adminDefinition(OrganizationSession, "Read fresh organization/session authority without credentials.", "", "", true, false),
		adminDefinition(OrganizationList, "List owned or joined organization contexts with fresh destinations.", `"offset":{"type":"integer","minimum":0,"maximum":100000},"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false),
		adminDefinition(MembershipList, "Read memberships, invitations and project grants visible to the current principal.", `"offset":{"type":"integer","minimum":0,"maximum":100000},"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false),
		adminDefinition(CredentialList, "Read credential metadata and grants; never returns credential hashes or tokens.", `"offset":{"type":"integer","minimum":0,"maximum":100000},"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false),
		adminDefinition(OrganizationSwitch, "Select an owned organization and return its fresh authenticated connection destination. Reconnect there; existing grants are never transferred.", `"organization_id":`+id, `"organization_id"`, false, false),
		adminDefinition(OrganizationCreate, "Create an organization through the deployment's existing provisioning command.", `"name":{"type":"string","minLength":1,"maxLength":120}`, `"name"`, false, false),
		adminDefinition(ProvisioningPage, "Read safe provisioning status for an allocation created by the current account, with a fresh destination when ready.", `"organization_id":`+id, `"organization_id"`, true, false),
		adminDefinition(ResumeProvisioning, "Resume a creator-owned allocation through the existing provisioning command with current authority; existing retry and capacity rules apply.", `"organization_id":`+id, `"organization_id"`, false, false),
		adminDefinition(OrganizationDelete, "Delete the current owned organization after an exact operator preview.", `"organization_id":`+id+`,"confirm_name":{"type":"string","minLength":1,"maxLength":120}`, `"organization_id","confirm_name"`, false, true),
		adminDefinition(InvitationAccept, "Accept an invitation for the authenticated account and return a fresh login destination.", `"invitation_id":`+id, `"invitation_id"`, false, false),
		adminDefinition(InvitationSend, "Invite a member with the exact email, role and project grants with current authority. Omitted grants give no project access.", `"email":{"type":"string","minLength":1,"maxLength":254},"role":`+role+`,"grants":`+grants, `"email","role"`, false, false),
		adminDefinition(InvitationEdit, "Replace the exact pending invitation project grants with current authority. An empty grants list removes all project access.", `"invitation_id":`+id+`,"grants":`+grants, `"invitation_id","grants"`, false, true),
		adminDefinition(InvitationResend, "Resend the exact pending organization invitation with current authority.", `"invitation_id":`+id, `"invitation_id"`, false, false),
		adminDefinition(InvitationRevoke, "Withdraw an existing pending organization invitation.", `"invitation_id":`+id, `"invitation_id"`, false, true),
		adminDefinition(MemberRemove, "Remove an organization member, preserving existing owner rules.", `"member_id":`+id, `"member_id"`, false, true),
		adminDefinition(MemberRole, "Change a member role, preserving existing ownership and last-owner rules.", `"member_id":`+id+`,"role":`+role, `"member_id","role"`, false, true),
		adminDefinition(MemberGrant, "Set or remove the exact member project grant.", `"member_id":`+id+`,"project_id":`+id+`,"write":{"type":"boolean"},"runner":{"type":"boolean"},"revoke":{"type":"boolean"}`, `"member_id","project_id"`, false, true),
		adminDefinition(CredentialCreate, "Create a credential with exact scopes, project grants and expiry. Hosted keys require one scope and an expiry of 1–90 whole days (for example 30d). Project access defaults to all current and future authorized projects; choose selected with 1–200 project IDs to restrict access. Existing requests supplying project IDs remain selected. The credential is delivered only to this authenticated connection; durable retries omit it.", `"name":{"type":"string","minLength":1,"maxLength":200},"scopes":`+scopes+`,"project_access":{"type":"string","enum":["all","selected"]},"project_ids":`+projects+`,"expires_in":{"type":"string","maxLength":32}`, `"name","scopes"`, false, false),
		adminDefinition(CredentialRotate, "Rotate an owned credential; the exact target and grace period require current administration authority.", `"credential_id":`+id+`,"grace":{"type":"string","maxLength":32}`, `"credential_id"`, false, true),
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
	if name == SessionLogout {
		group = "connection"
	} else if strings.HasPrefix(name, "credential_") {
		group = "credentials"
	}
	return Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), Annotations: Annotations{ReadOnly: readOnly, Destructive: destructive, Idempotent: true, OpenWorld: true}, Meta: ToolMetadata{Toolset: group}}
}

func AdministrationScope(name string) apikey.Scope {
	switch name {
	case OrganizationSession, OrganizationList, ProvisioningPage, MembershipList, CredentialList:
		return apikey.ScopeRead
	case OrganizationSwitch, InvitationAccept, SessionLogout:
		return apikey.ScopeRead
	default:
		return apikey.ScopeAdmin
	}
}

func AdministrationRequirement(name string) Requirement {
	requirement := Requirement{Scope: AdministrationScope(name)}
	if strings.HasPrefix(name, "credential_") {
		requirement.ResourceKind = "credentials"
	}
	return requirement
}

func IsAdministration(name string) bool {
	for _, d := range AdministrationCatalog() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// SignOutResult contains only the outcome of ending access. Provider errors and
// session identifiers never leave the application command.
type SignOutResult struct {
	SignedOut         bool `json:"signed_out"`
	ProviderConfirmed bool `json:"provider_sign_out_confirmed"`
	AuditRecorded     bool `json:"audit_recorded"`
}

func (r SignOutResult) Message() string {
	if !r.ProviderConfirmed {
		return "Signed out of Detent. Provider sign-out could not be confirmed; retry sign-out from your identity provider."
	}
	if !r.AuditRecorded {
		return "Signed out of Detent. Sign-out audit could not be recorded. Sign in and open a fresh connection to continue."
	}
	return "Signed out of Detent. Sign in and open a fresh connection to continue."
}
