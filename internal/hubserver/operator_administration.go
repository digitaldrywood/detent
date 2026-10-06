package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hubAdministration struct{ service *Service }

func (s *Service) operatorAdministration() *operatoradmin.Executor {
	names := []string{operatortool.OrganizationSession, operatortool.OrganizationList}
	if s.config.Hosted != nil {
		names = append(names, operatortool.CredentialList, operatortool.CredentialCreate, operatortool.CredentialRevoke)
		names = append(names, operatortool.OrganizationProjectRank, operatortool.OrganizationProjectRankUpdate)
		names = append(names, operatortool.MembershipList, operatortool.InvitationSend, operatortool.InvitationRevoke, operatortool.MemberRemove, operatortool.MemberRole, operatortool.MemberGrant)
		if _, ok := s.config.Hosted.Provider.(auth.InvitationAdministration); ok {
			names = append(names, operatortool.InvitationEdit)
			if _, ok := s.config.Hosted.Provider.(auth.InvitationDelivery); ok {
				names = append(names, operatortool.InvitationResend)
			}
		}
		if !s.hostedShared() {
			names = append(names, operatortool.SessionLogout, operatortool.OrganizationSwitch, operatortool.OrganizationCreate, operatortool.SupportStart)
			if _, ok := s.config.Hosted.Provider.(auth.InvitationAdministration); ok {
				names = append(names, operatortool.InvitationAccept)
			}
		}
	} else {
		names = append(names, operatortool.OrganizationSwitch, operatortool.OrganizationCreate, operatortool.CredentialList, operatortool.CredentialCreate, operatortool.CredentialRotate, operatortool.CredentialRevoke, operatortool.CredentialGrant)
	}
	return operatoradmin.New(hubAdministration{s}, names...)
}

// Resolve through the original authenticated binding again. The effective role
// supplied by the entry is still capped by the local member role.
func (a hubAdministration) credential(ctx context.Context) (apiCredential, error) {
	c := operatortool.CurrentConnection(ctx)
	authority, err := c.Resolve(ctx)
	if err != nil || authority.Identity != c.Identity {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	s := a.service
	if resolve, ok := ctx.Value(hubOperatorResolverKey{}).(func(context.Context) (apiCredential, error)); ok {
		return resolve(ctx)
	}
	if c.Identity.SessionID != "" {
		session, err := s.storedWebSession(ctx, c.Identity.SessionID, s.config.now())
		if err != nil || session.Identity == nil {
			return apiCredential{}, operatortool.ErrAccessDenied
		}
		if s.hostedShared() {
			credential, _, err := s.hostedSharedCredential(ctx, session, c.Identity.SessionID, authority.Account.Role)
			return credential, err
		}
		if authority.Account.Role == "account" {
			return s.hostedAccountCredential(ctx, c.Identity.SessionID)
		}
		credential, _, err := s.hostedSessionCredential(ctx, session, c.Identity.SessionID)
		if err == nil {
			credential.HostedRole = lesserHostedRole(credential.HostedRole, authority.Account.Role)
		}
		return credential, err
	}
	var credential apiCredential
	err = s.database.db.QueryRowContext(ctx, "SELECT id,name,token_hash,scope,native_only FROM api_tokens WHERE id=? AND token_hash=? AND revoked_at IS NULL", c.Identity.PrincipalID, c.Identity.CredentialID).Scan(&credential.ID, &credential.Name, &credential.Hash, &credential.Scope, &credential.NativeOnly)
	return credential, err
}

func (a hubAdministration) Authorize(ctx context.Context, name string, in operatoradmin.Input, resource string) error {
	credential, err := a.credential(ctx)
	if err != nil {
		return operatortool.ErrAccessDenied
	}
	s := a.service
	if name == operatortool.SessionLogout {
		if s.config.Hosted == nil || s.hostedShared() || credential.Hosted == nil || credential.SessionHash == "" {
			return operatortool.ErrAccessDenied
		}
		return nil
	}
	if name == operatortool.OrganizationSession {
		return nil
	}
	if name == operatortool.OrganizationList {
		if s.config.Hosted == nil && !credential.NativeOnly && credential.Scope != apiScopeAdmin {
			return operatortool.ErrAccessDenied
		}
		return nil
	}
	if s.config.Hosted == nil && name == operatortool.OrganizationSwitch {
		if in.OrganizationID == "" {
			return nil
		}
		return s.authorizeConversationOrganization(ctx, nativeScope{organization: tracker.OrganizationID(in.OrganizationID), credential: credential})
	}

	if s.config.Hosted == nil {
		if name == operatortool.CredentialCreate && (len(in.Scopes) > 0 && len(in.Scopes) == 1 && in.Scopes[0] == "read" || in.ExpiresIn != "") || name == operatortool.CredentialRotate && in.Grace != "" {
			return operatortool.ErrInvalidArguments
		}
		if credential.Scope != apiScopeAdmin || credential.NativeOnly || credential.Runner.RunnerID != "" {
			return operatortool.ErrAccessDenied
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationWide: true}); err != nil {
			return err
		}
		// This deployment has API administration but no authenticated browser
		// approver. Material MCP operations cannot borrow bearer-token authority
		// as human confirmation.
		if name != operatortool.CredentialList {
			return operatoradmin.ErrUnavailable
		}
		for _, project := range in.ProjectIDs {
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: project}); err != nil {
				return err
			}
		}

		if in.CredentialID != "" || resource != "" && (name == operatortool.CredentialCreate || name == operatortool.CredentialRotate) {
			id := in.CredentialID
			if resource != "" && (name == operatortool.CredentialCreate || name == operatortool.CredentialRotate) {
				id = resource
			}
			var count int
			if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM api_tokens WHERE id=? AND NOT EXISTS(SELECT 1 FROM runner_identities WHERE token_id=api_tokens.id)", id).Scan(&count); err != nil || count != 1 {
				return operatortool.ErrAccessDenied
			}
			if resource != "" && (name == operatortool.CredentialCreate || name == operatortool.CredentialRotate) {
				if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM api_tokens WHERE id=? AND revoked_at IS NULL", id).Scan(&count); err != nil || count != 1 {
					return operatortool.ErrAccessDenied
				}
			}
		}
		return nil
	}
	if credential.Hosted == nil {
		return operatortool.ErrAccessDenied
	}
	if name == operatortool.CredentialList || name == operatortool.CredentialCreate || name == operatortool.CredentialRevoke {
		credential, err = s.hostedKeyCredentialFor(ctx, credential)
		if err != nil {
			return err
		}
		if name == operatortool.CredentialCreate && len(in.Scopes) > 0 {
			request, err := hostedToolKeyRequest(in)
			if err != nil {
				return err
			}
			if err := s.authorizeHostedKeyRequest(ctx, credential, request); err != nil {
				return operatortool.ErrAccessDenied
			}
		}
		id := in.CredentialID
		if name == operatortool.CredentialCreate {
			id = resource
		}
		if id != "" {
			key, err := s.hostedAPIKeyFor(ctx, credential, id)
			if err != nil {
				return operatortool.ErrAccessDenied
			}
			if name == operatortool.CredentialCreate {
				if key.Expiry == nil {
					return operatortool.ErrAccessDenied
				}
				expiry, err := parseTimeValue(*key.Expiry)
				projects := append([]string(nil), in.ProjectIDs...)
				sort.Strings(projects)
				projects = slices.Compact(projects)
				if len(in.Scopes) != 1 || err != nil || key.Revoked || !expiry.After(s.config.now()) || key.Scope != apikey.Scope(in.Scopes[0]) || len(key.Projects) != len(projects) {
					return operatortool.ErrAccessDenied
				}
				for i, project := range projects {
					if key.Projects[i] != project {
						return operatortool.ErrAccessDenied
					}
				}
			}
		}
		return nil
	}
	if name == operatortool.SupportStart {
		session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
		if err != nil || credential.Hosted.SupportActor != "" || !hostedEmailListed(s.config.Hosted.SupportActors, session.Email) || in.OrganizationID != "" && in.OrganizationID != s.config.Hosted.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		return nil
	}
	if name == operatortool.OrganizationCreate {
		session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
		if err != nil || credential.Hosted.Subject != s.config.Hosted.BootstrapSubject || credential.Hosted.SupportActor != "" || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
			return operatortool.ErrAccessDenied
		}
		var count int
		if resource != "" {
			if resource != s.config.Hosted.OrganizationID {
				return operatortool.ErrAccessDenied
			}
			return nil
		}
		if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_members").Scan(&count); err != nil || count != 0 && in.RequestID == "" {
			return operatortool.ErrAccessDenied
		}
		return nil
	}

	if name == operatortool.MembershipList {
		return nil
	}
	if name == operatortool.InvitationAccept && in.InvitationID != "" {
		session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
		if err != nil || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
			return operatortool.ErrAccessDenied
		}
		invitation, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, in.InvitationID)
		providerID, providerErr := s.hostedProviderOrganization(ctx)
		if err != nil || providerErr != nil || providerID == "" || invitation.OrganizationID != providerID || auth.InvitationProblem(invitation, credential.Hosted.Subject, session.Email, s.config.now()) != "" {
			return operatortool.ErrAccessDenied
		}
		var count int
		if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_invitations WHERE id=? AND organization_id=? AND (accepted_user_id='' OR accepted_user_id=?)", in.InvitationID, s.config.Hosted.OrganizationID, credential.Hosted.Subject).Scan(&count); err != nil || count != 1 {
			return operatortool.ErrAccessDenied
		}
	}
	if name == operatortool.OrganizationSwitch || name == operatortool.InvitationAccept {
		if s.hostedShared() || credential.Hosted.SupportActor != "" || credential.HostedKeyScope != "" {
			return operatortool.ErrAccessDenied
		}
		if name == operatortool.OrganizationSwitch && in.OrganizationID != "" {
			_, err := s.hostedSwitchFor(ctx, credential.Hosted, in.OrganizationID)
			return err
		}
		return nil
	}
	if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return operatortool.ErrAccessDenied
	}
	if in.Role == "owner" && credential.HostedRole != "owner" {
		return operatortool.ErrAccessDenied
	}
	if name == operatortool.InvitationSend && in.Email != "" {
		if err := s.validateHostedInvitationGrants(ctx, s.database.db, credential, strings.ToLower(strings.TrimSpace(in.Email)), in.Grants); err != nil {
			return err
		}
	}
	if (name == operatortool.InvitationEdit || name == operatortool.InvitationResend) && in.InvitationID != "" {
		view, err := s.pendingHostedInvitationFor(ctx, s.database.db, credential, in.InvitationID)
		if err != nil {
			return err
		}
		if name == operatortool.InvitationEdit {
			return s.validateHostedInvitationEdit(ctx, s.database.db, credential, view, in.Grants)
		}
		if err := s.validateHostedInvitationGrants(ctx, s.database.db, credential, view.Email, view.Grants); err != nil {
			return err
		}
	}
	if in.MemberID != "" {
		if name == operatortool.MemberRole || name == operatortool.MemberRemove {
			if _, err := s.hostedManagedMemberFor(ctx, credential, in.MemberID, name == operatortool.MemberRemove || in.Role != "owner"); err != nil {
				return operatortool.ErrAccessDenied
			}
		} else if _, err := s.hostedMemberByID(ctx, credential, in.MemberID); err != nil {
			return operatortool.ErrAccessDenied
		}
	}
	invitation := in.InvitationID
	if name == operatortool.InvitationSend {
		invitation = resource
	}
	if invitation != "" {
		var count int
		if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_invitations WHERE id=? AND organization_id=? AND accepted_user_id=''", invitation, s.config.Hosted.OrganizationID).Scan(&count); err != nil || count != 1 {
			return operatortool.ErrAccessDenied
		}
	}
	return nil
}

func (a hubAdministration) Read(ctx context.Context, name string, in operatoradmin.Input) (any, error) {
	s := a.service
	credential, err := a.credential(ctx)
	if err != nil {
		return nil, err
	}
	switch name {
	case operatortool.OrganizationProjectRank:
		return readOrganizationProjectRank(ctx, s.database.db, tracker.OrganizationID(s.config.Hosted.OrganizationID))
	case operatortool.OrganizationSession:
		setup := hostedAccountSetup{}
		if credential.Hosted != nil && credential.SessionHash != "" {
			session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
			if err != nil {
				return nil, err
			}
			setup, err = s.hostedAccountSetupFor(ctx, session)
			if err != nil {
				return nil, err
			}
			if credential.HostedRole != "account" {
				setup.CanCreate = credential.HostedRole == "owner" || credential.HostedRole == "admin"
			}
		}
		return struct {
			hostedAccountSetup
			Principal    string `json:"principal_id"`
			Organization string `json:"organization_id"`
			Role         string `json:"role"`
			SupportActor string `json:"support_actor,omitempty"`
			Destination  string `json:"destination"`
			Reconnect    bool   `json:"reconnect"`
		}{setup, credential.ID, operatortool.ConnectionIdentity(ctx).OrganizationID, credential.HostedRole, func() string {
			if credential.Hosted != nil {
				return credential.Hosted.SupportActor
			}
			return ""
		}(), s.operatorSessionDestination(credential), true}, nil
	case operatortool.MembershipList:
		members, err := s.hostedMembersFor(ctx, credential)
		if err != nil {
			return nil, err
		}
		return struct {
			Members     any `json:"members"`
			Invitations any `json:"invitations"`
		}{operatoradmin.Page(members.Members, in), operatoradmin.Page(members.Invitations, in)}, nil
	case operatortool.OrganizationList:
		if credential.Hosted != nil {
			choices, err := s.hostedOrganizationChoicesFor(ctx, credential)
			if err != nil {
				return nil, err
			}
			return operatoradmin.Page(choices, in), nil
		}
		rows, err := s.database.db.QueryContext(ctx, "SELECT id,name,local FROM organizations WHERE id=? OR ?=0 ORDER BY id", operatortool.ConnectionIdentity(ctx).OrganizationID, credential.NativeOnly)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var choices []nativeOrganization
		for rows.Next() {
			var row nativeOrganization
			if err := rows.Scan(&row.ID, &row.Name, &row.Local); err != nil {
				return nil, err
			}
			choices = append(choices, row)
		}
		return operatoradmin.Page(choices, in), rows.Err()
	case operatortool.CredentialList:
		if s.config.Hosted != nil {
			keys, err := s.hostedAPIKeysFor(ctx, credential)
			return operatoradmin.Page(keys, in), err
		}
		return s.tokenMetadata(ctx, in)
	default:
		return nil, operatoradmin.ErrUnavailable
	}
}

func (a hubAdministration) Preview(ctx context.Context, name string, in operatoradmin.Input) (operatoradmin.Preview, error) {
	s := a.service
	credential, err := a.credential(ctx)
	if err != nil {
		return operatoradmin.Preview{}, err
	}
	preview := operatoradmin.Preview{Summary: name, Current: in}
	switch name {
	case operatortool.OrganizationProjectRankUpdate:
		current, err := readOrganizationProjectRank(ctx, s.database.db, tracker.OrganizationID(s.config.Hosted.OrganizationID))
		if err != nil {
			return preview, err
		}
		preview.ResourceID = s.config.Hosted.OrganizationID
		preview.Current = struct {
			Rank  organizationProjectRank `json:"rank"`
			Input operatoradmin.Input     `json:"input"`
		}{current, in}
	case operatortool.InvitationEdit, operatortool.InvitationResend:
		view, err := s.pendingHostedInvitationFor(ctx, s.database.db, credential, in.InvitationID)
		if err != nil {
			return preview, err
		}
		preview.ResourceID = view.ID
		preview.Current = struct {
			Invitation hostedInvitationView `json:"invitation"`
			Input      operatoradmin.Input  `json:"input"`
		}{view, in}
	case operatortool.MemberRemove, operatortool.MemberRole, operatortool.MemberGrant:
		member, err := s.hostedMemberByID(ctx, credential, in.MemberID)
		if err != nil {
			return preview, err
		}
		view, err := s.hostedMemberResponse(ctx, member)
		if err != nil {
			return preview, err
		}
		preview.ResourceID, preview.Current = member.ID, struct {
			Member hostedMemberView    `json:"member"`
			Input  operatoradmin.Input `json:"input"`
		}{view, in}
	case operatortool.InvitationRevoke:
		var email, role string
		err := s.database.db.QueryRowContext(ctx, "SELECT email,role FROM hosted_invitations WHERE id=? AND organization_id=? AND accepted_user_id=''", in.InvitationID, s.config.Hosted.OrganizationID).Scan(&email, &role)
		if err != nil {
			return preview, err
		}
		preview.ResourceID = in.InvitationID
		preview.Current = struct{ Email, Role string }{email, role}
	case operatortool.OrganizationSwitch:
		if s.config.Hosted == nil {
			var org nativeOrganization
			err := s.database.db.QueryRowContext(ctx, "SELECT id,name,local FROM organizations WHERE id=?", in.OrganizationID).Scan(&org.ID, &org.Name, &org.Local)
			preview.ResourceID = in.OrganizationID
			preview.Current = org
			return preview, err
		}
		next, err := s.hostedSwitchFor(ctx, credential.Hosted, in.OrganizationID)
		if err != nil {
			return preview, err
		}
		preview.ResourceID = in.OrganizationID
		preview.Current = hostedNextResponse{Next: next}
	case operatortool.InvitationAccept:
		// The ID identifies the provider invitation, not an identity exchange token.
		var id, email, role string
		err := s.database.db.QueryRowContext(ctx, "SELECT i.id,i.email,i.role FROM hosted_invitations i WHERE i.id=? AND i.organization_id=? AND (i.accepted_user_id='' OR i.accepted_user_id=?)", in.InvitationID, s.config.Hosted.OrganizationID, credential.Hosted.Subject).Scan(&id, &email, &role)
		if err != nil {
			return preview, err
		}
		preview.ResourceID = id
		preview.Current = struct{ Email, Role string }{email, role}
	case operatortool.CredentialRotate, operatortool.CredentialRevoke, operatortool.CredentialGrant:
		if s.config.Hosted != nil {
			key, err := s.hostedAPIKeyFor(ctx, credential, in.CredentialID)
			preview.ResourceID, preview.Current = in.CredentialID, key
			return preview, err
		}
		view, err := s.tokenMetadataByID(ctx, in.CredentialID)
		if err != nil {
			return preview, err
		}
		preview.ResourceID = in.CredentialID
		preview.Current = struct {
			Credential tokenResponse       `json:"credential"`
			Input      operatoradmin.Input `json:"input"`
		}{view, in}
	}
	return preview, nil
}

func (a hubAdministration) Execute(ctx context.Context, name string, in operatoradmin.Input, m mutation.Metadata) (operatoradmin.Output, error) {
	s := a.service
	if err := a.Authorize(ctx, name, in, ""); err != nil {
		return operatoradmin.Output{}, err
	}
	credential, err := a.credential(ctx)
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if name == operatortool.SessionLogout {
		if err := a.Authorize(ctx, name, in, ""); err != nil {
			return operatoradmin.Output{}, err
		}
		outcome, err := s.logoutHostedFor(ctx, credential.SessionHash, credential.Hosted)
		return operatoradmin.Output{URL: "https://detent.build", Reconnect: true, SignOut: &outcome}, err
	}
	if s.config.Hosted != nil && (name == operatortool.OrganizationCreate || name == operatortool.SupportStart || name == operatortool.InvitationAccept && credential.HostedRole == "account") {
		return s.executeHostedAccountFor(ctx, credential, name, in, m)
	}

	if name == operatortool.InvitationSend {
		view, err := s.inviteHostedMemberFor(ctx, credential, in.Email, in.Role, in.RequestID, in.Grants)
		if err != nil {
			return operatoradmin.Output{}, err
		}
		raw, err := json.Marshal(view)
		return operatoradmin.Output{ResourceID: view.ID, Data: raw}, err
	}
	command := hostedCommand{organization: operatortool.ConnectionIdentity(ctx).OrganizationID, actor: credential.ID, operation: name, key: in.RequestID, input: in}
	claimed, replay, err := s.claimHostedOperation(ctx, command)
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if !claimed {
		var output operatoradmin.Output
		if err := json.Unmarshal(replay, &output); err != nil {
			return operatoradmin.Output{}, err
		}
		if err := a.AuthorizeOutput(ctx, name, in, output); err != nil {
			return operatoradmin.Output{}, err
		}
		return output, nil
	}
	output := operatoradmin.Output{ResourceID: m.ResourceID}
	var data any
	switch name {
	case operatortool.OrganizationProjectRankUpdate:
		ids := make([]tracker.ProjectID, len(in.ProjectIDs))
		for i, id := range in.ProjectIDs {
			ids[i] = tracker.ProjectID(id)
		}
		data, err = s.updateOrganizationProjectRankCommand(ctx, credential, projectRankChange{ExpectedRevision: in.ExpectedRevision, ProjectIDs: ids})
	case operatortool.MemberRemove:
		err = s.removeHostedMemberFor(ctx, credential, in.MemberID)
	case operatortool.MemberRole:
		data, err = s.changeHostedRoleFor(ctx, credential, in.MemberID, in.Role)
	case operatortool.MemberGrant:
		data, err = s.changeHostedGrantFor(ctx, credential, in.MemberID, in.ProjectID, in.Write, in.Runner, in.Revoke)
	case operatortool.InvitationRevoke:
		err = s.revokeHostedInvitationFor(ctx, in.InvitationID)
	case operatortool.InvitationEdit:
		err = s.editHostedInvitationFor(ctx, credential, in.InvitationID, in.Grants)
	case operatortool.InvitationResend:
		err = s.resendHostedInvitationFor(ctx, credential, in.InvitationID)
	case operatortool.OrganizationSwitch:
		if s.config.Hosted == nil {
			output.URL = "/api/v2/organizations/" + in.OrganizationID + "/mcp"
			output.Reconnect = true
			break
		}
		output.URL, err = s.hostedSwitchFor(ctx, credential.Hosted, in.OrganizationID)
		output.Reconnect = true
	case operatortool.InvitationAccept:
		session, sessionErr := s.storedWebSession(ctx, credential.SessionHash, s.config.now())
		if sessionErr != nil {
			return output, sessionErr
		}
		err = s.acceptHostedInvitationIDFor(ctx, auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, in.InvitationID)
		output.URL = s.config.Hosted.PublicURL + "/auth/oidc/start"
		output.Reconnect = true
	case operatortool.SupportStart:
		output.URL = s.config.Hosted.PublicURL + "/support"
		output.Reconnect = true
		data = struct {
			InteractiveRequired bool `json:"interactive_required"`
		}{true}
	case operatortool.OrganizationCreate:
		if s.config.Hosted != nil {
			output.ResourceID, err = s.createHostedOrganizationFor(ctx, credential, in.Name)
			output.URL = s.config.Hosted.PublicURL + "/auth/oidc/start"
			output.Reconnect = true
		} else {
			data, err = s.createNativeOrganizationFor(ctx, in.Name)
		}
	case operatortool.CredentialCreate:
		if s.config.Hosted != nil {
			request, requestErr := hostedToolKeyRequest(in)
			if requestErr != nil {
				return operatoradmin.Output{}, requestErr
			}
			data, err = s.createHostedAPIKeyFor(ctx, credential, request)
			break
		}
		var token tokenResponse
		token, err = s.createAPITokenFor(ctx, tokenRequest{Name: in.Name, Scope: nativeToolScope(in.Scopes)})
		if err == nil {
			for _, project := range in.ProjectIDs {
				if err = s.grantNativeTokenFor(ctx, token.ID, operatortool.ConnectionIdentity(ctx).OrganizationID, project); err != nil {
					break
				}
			}
		}
		data = token
	case operatortool.CredentialRotate:
		data, err = s.rotateAPITokenFor(ctx, in.CredentialID)
	case operatortool.CredentialRevoke:
		if s.config.Hosted != nil {
			err = s.revokeHostedAPIKeyFor(ctx, credential, in.CredentialID)
		} else {
			err = s.revokeAPITokenFor(ctx, in.CredentialID)
		}
	case operatortool.CredentialGrant:
		for _, id := range in.ProjectIDs {
			if err = s.grantNativeTokenFor(ctx, in.CredentialID, operatortool.ConnectionIdentity(ctx).OrganizationID, id); err != nil {
				break
			}
		}
	default:
		err = operatoradmin.ErrUnavailable
	}
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if token, ok := data.(tokenResponse); ok {
		output.ResourceID = token.ID
		if s.config.Hosted != nil {
			data = token
		} else {
			view, readErr := s.tokenMetadataByID(ctx, token.ID)
			if readErr != nil {
				return operatoradmin.Output{}, readErr
			}
			view.Token = token.Token
			data = view
		}
	}
	if org, ok := data.(nativeOrganization); ok {
		output.ResourceID = string(org.ID)
		output.URL = "/api/v2/organizations/" + string(org.ID) + "/mcp"
		output.Reconnect = true
	}
	if data != nil {
		output.Data, err = json.Marshal(data)
		if err != nil {
			return operatoradmin.Output{}, err
		}
	}
	receipt := output
	if name == operatortool.CredentialCreate || name == operatortool.CredentialRotate {
		if token, ok := data.(tokenResponse); ok {
			token.Token = ""
			receipt.Data, err = json.Marshal(token)
			if err != nil {
				return operatoradmin.Output{}, err
			}
		} else {
			receipt.Data = nil
		}
	}
	if _, err := s.completeHostedOperation(ctx, command, receipt); err != nil {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	return output, nil
}

func (a hubAdministration) Audit(ctx context.Context, m mutation.Metadata, outcome string) {
	m.RetryIdentity, m.InputHash = "", ""
	a.service.config.Logger.InfoContext(ctx, "operator mutation", "audit", m, "outcome", outcome)
}

func nativeToolScope(scopes []string) apiScope {
	if apikey.HasScope(scopes, apikey.ScopeAdmin) {
		return apiScopeAdmin
	}
	return apiScopeOperator
}

func hostedToolKeyRequest(in operatoradmin.Input) (hostedKeyRequest, error) {
	if len(in.Scopes) != 1 {
		return hostedKeyRequest{}, operatortool.ErrInvalidArguments
	}
	days, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(in.ExpiresIn), "d"))
	if err != nil || days < 1 || days > 90 {
		return hostedKeyRequest{}, operatortool.ErrInvalidArguments
	}
	return hostedKeyRequest{Name: in.Name, Scope: apikey.Scope(in.Scopes[0]), Days: days, Projects: in.ProjectIDs, ProjectAccess: hostedProjectAccess(in.ProjectAccess)}, nil
}

func (s *Service) hostedSwitchFor(ctx context.Context, identity *auth.HostedIdentity, organization string) (string, error) {
	if identity == nil || identity.SupportActor != "" {
		return "", operatortool.ErrAccessDenied
	}
	for _, destination := range s.config.Hosted.Directory {
		if destination.OrganizationID != organization {
			continue
		}
		memberships, err := s.config.Hosted.Provider.Memberships(ctx, identity.Subject, destination.WorkOSOrganizationID)
		if err != nil {
			return "", err
		}
		for _, member := range memberships {
			if member.Status == "active" && member.UserID == identity.Subject && member.OrganizationID == destination.WorkOSOrganizationID {
				return destination.PublicURL + "/auth/oidc/start", nil
			}
		}
	}
	return "", operatortool.ErrAccessDenied
}

// Metadata deliberately omits token hashes and runner credentials.
func (s *Service) tokenMetadata(ctx context.Context, in operatoradmin.Input) (any, error) {
	rows, err := s.database.db.QueryContext(ctx, "SELECT id FROM api_tokens WHERE NOT EXISTS(SELECT 1 FROM runner_identities WHERE token_id=api_tokens.id) ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var views []tokenResponse
	for _, id := range ids {
		view, err := s.tokenMetadataByID(ctx, id)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return operatoradmin.Page(views, in), nil
}

func (s *Service) tokenMetadataByID(ctx context.Context, id string) (tokenResponse, error) {
	var view tokenResponse
	var created string
	var revoked sql.NullString
	err := s.database.db.QueryRowContext(ctx, "SELECT id,name,scope,token_fingerprint,created_at,native_only,revoked_at,CASE WHEN hosted_user_id IS NOT NULL THEN operator_project_access ELSE '' END FROM api_tokens WHERE id=?", id).Scan(&view.ID, &view.Name, &view.Scope, &view.Fingerprint, &created, &view.NativeOnly, &revoked, &view.ProjectAccess)
	if err != nil {
		return view, err
	}
	view.CreatedAt, err = parseTimeValue(created)
	if err != nil {
		return view, err
	}
	if revoked.Valid {
		at, err := parseTimeValue(revoked.String)
		if err != nil {
			return view, err
		}
		view.RevokedAt = &at
	}
	rows, err := s.database.db.QueryContext(ctx, "SELECT organization_id,project_id FROM token_grants WHERE token_id=? ORDER BY organization_id,project_id", id)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	view.Grants = []tokenGrantResponse{}
	view.Projects = []string{}
	for rows.Next() {
		var grant tokenGrantResponse
		if err := rows.Scan(&grant.OrganizationID, &grant.ProjectID); err != nil {
			return view, err
		}
		view.Grants = append(view.Grants, grant)
		view.Projects = append(view.Projects, grant.ProjectID)
	}
	return view, rows.Err()
}

func (s *Service) hostedOrganizationChoicesFor(ctx context.Context, credential apiCredential) ([]nativeOrganization, error) {
	choices := []nativeOrganization{{ID: tracker.OrganizationID(s.config.Hosted.OrganizationID), Name: s.config.Hosted.OrganizationID}}
	if s.hostedShared() || credential.Hosted.SupportActor != "" || credential.HostedKeyScope != "" {
		return choices, nil
	}
	for _, destination := range s.config.Hosted.Directory {
		if destination.OrganizationID == s.config.Hosted.OrganizationID {
			continue
		}
		if _, err := s.hostedSwitchFor(ctx, credential.Hosted, destination.OrganizationID); err == nil {
			choices = append(choices, nativeOrganization{ID: tracker.OrganizationID(destination.OrganizationID), Name: destination.OrganizationID})
		}
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].ID < choices[j].ID })
	return choices, nil
}

func (a hubAdministration) AuthorizeOutput(ctx context.Context, name string, in operatoradmin.Input, output operatoradmin.Output) error {
	if name == operatortool.InvitationSend {
		if output.ResourceID == "" {
			return operatortool.ErrAccessDenied
		}
		return a.Authorize(ctx, name, in, output.ResourceID)
	}
	if (name != operatortool.CredentialCreate && name != operatortool.CredentialRotate) || len(output.Data) == 0 {
		return nil
	}
	if err := a.Authorize(ctx, name, in, output.ResourceID); err != nil {
		return err
	}
	var delivered tokenResponse
	if json.Unmarshal(output.Data, &delivered) != nil {
		return operatortool.ErrAccessDenied
	}
	current, err := a.service.tokenMetadataByID(ctx, output.ResourceID)
	if err != nil || current.Fingerprint != delivered.Fingerprint {
		return operatortool.ErrAccessDenied
	}
	return nil
}

func (s *Service) operatorSessionDestination(credential apiCredential) string {
	if s.config.Hosted == nil {
		return s.operatorDashboardURL()
	}
	if credential.HostedRole == "account" {
		return s.config.Hosted.PublicURL + "/organization"
	}
	return s.config.Hosted.PublicURL + "/"
}
