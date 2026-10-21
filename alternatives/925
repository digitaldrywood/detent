package cloudentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type entryAdministration struct{ service *Service }

func (s *Service) registerAdministration() {
	names := []string{operatortool.SessionLogout, operatortool.OrganizationSession, operatortool.OrganizationList, operatortool.OrganizationSwitch, operatortool.SupportStart}
	if _, ok := s.config.Provider.(auth.InvitationAdministration); ok {
		names = append(names, operatortool.InvitationAccept)
	}
	if s.config.Allocation != nil {
		names = append(names, operatortool.OrganizationCreate, operatortool.OrganizationDelete, operatortool.ProvisioningPage, operatortool.ResumeProvisioning)
	}
	s.administration = operatoradmin.New(entryAdministration{s}, names...)
	handler := mcp.NewHTTPHandler(s.administration, "", mcp.HTTPConfig{Principal: func(r *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(r.Context()) }})
	s.echo.Any("/api/cloud/mcp", echo.WrapHandler(handler), s.administrationAuthority)
}

// Account commands remain bound to the original account session. A destination
// URL never changes this connection's organization or copies its project grants.
func (s *Service) administrationAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return c.NoContent(http.StatusForbidden)
		}
		session, err := s.session(c)
		if err != nil {
			return c.NoContent(http.StatusUnauthorized)
		}
		identity := operatortool.Identity{PrincipalID: session.Subject, OrganizationID: "account:" + session.Subject, CredentialID: session.Hash, SessionID: session.Hash}
		connection := operatortool.Connection{Identity: identity, DashboardURL: s.config.PublicURL, Resolve: func(ctx context.Context) (operatortool.Authority, error) {
			current, err := s.currentAdministrationSession(ctx, identity)
			if err != nil {
				return operatortool.Authority{}, err
			}
			return operatortool.Authority{Identity: identity, Account: operatortool.Account{Subject: current.Subject, Email: current.Email, SupportActor: current.Identity.SupportActor}, Check: func(_ context.Context, r operatortool.Requirement) error {
				if r.ProjectID != "" || r.ResourceID != "" || r.ResourceKind != "" {
					return operatortool.ErrAccessDenied
				}
				return nil
			}}, nil
		}}
		c.SetRequest(c.Request().WithContext(operatortool.WithConnection(c.Request().Context(), connection)))
		return next(c)
	}
}

func (s *Service) currentAdministrationSession(ctx context.Context, id operatortool.Identity) (accountSession, error) {
	session, err := s.auth.session(ctx, id.SessionID)
	if err != nil || session.Subject != id.PrincipalID || session.Hash != id.CredentialID {
		return accountSession{}, operatortool.ErrAccessDenied
	}
	current, err := s.config.Provider.CurrentSession(ctx, session.Identity)
	if err != nil || current.Subject != session.Subject || current.SessionID != session.Identity.SessionID || current.SupportActor != session.Identity.SupportActor || !current.ExpiresAt.After(s.config.now()) {
		return accountSession{}, operatortool.ErrAccessDenied
	}
	return session, nil
}

func (a entryAdministration) Authorize(ctx context.Context, name string, in operatoradmin.Input, resource string) error {
	s := a.service
	session, err := s.currentAdministrationSession(ctx, operatortool.ConnectionIdentity(ctx))
	if err != nil {
		return err
	}
	if platformMemberOperation(name) {
		role := s.sessionPlatformRole(ctx, session)
		if role == "" || name != platformMemberList && role != "admin" {
			return operatortool.ErrAccessDenied
		}
		return nil
	}
	switch name {
	case operatortool.ProvisioningPage, operatortool.ResumeProvisioning:
		if s.config.Allocation == nil {
			return operatoradmin.ErrUnavailable
		}
		if in.OrganizationID == "" { // Catalog discovery conveys no resource authority.
			return nil
		}
		if resource != "" && resource != in.OrganizationID {
			return operatortool.ErrAccessDenied
		}
		_, err = s.creatorOrganizationFor(ctx, session, in.OrganizationID)
		return err
	case operatortool.OrganizationSession, operatortool.OrganizationList, operatortool.SessionLogout:
		return nil
	case operatortool.SupportStart:
		if !s.supportActor(ctx, session.Email) || session.Identity.SupportActor != "" {
			return operatortool.ErrAccessDenied
		}
		if in.OrganizationID != "" {
			_, err = s.readyOrganization(ctx, in.OrganizationID)
		}
		if in.Reason != "" && !auth.ValidSupportReason(in.Reason) {
			return operatortool.ErrInvalidArguments
		}
		return err
	case operatortool.OrganizationCreate:
		if s.config.Allocation == nil {
			return operatoradmin.ErrUnavailable
		}
		if session.Identity.SupportActor != "" || !s.signupAllowed(session.Email) {
			return operatortool.ErrAccessDenied
		}
		if resource != "" {
			var owner string
			err = s.registry.store.db.QueryRowContext(ctx, "SELECT creator_subject FROM organizations WHERE id=?", resource).Scan(&owner)
			if err != nil || owner != session.Subject {
				return operatortool.ErrAccessDenied
			}
		}
		return nil
	case operatortool.OrganizationDelete:
		if in.OrganizationID == "" {
			choices, err := s.organizationChoices(ctx, session)
			if err != nil {
				return err
			}
			for _, choice := range choices {
				if choice.Role == "owner" {
					if org, err := s.readyOrganization(ctx, choice.ID); err == nil && org.Managed {
						return nil
					}
				}
			}
			return operatortool.ErrAccessDenied
		}
		_, err = s.ownerOrganizationFor(ctx, session, in.OrganizationID)
		if err != nil {
			return operatortool.ErrAccessDenied
		}
		return nil
	case operatortool.OrganizationSwitch:
		if session.Identity.SupportActor != "" {
			return operatortool.ErrAccessDenied
		}
		if in.OrganizationID == "" {
			return nil
		}
		_, err = s.switchOrganizationFor(ctx, session, in.OrganizationID)
		return err
	case operatortool.InvitationAccept:
		if session.Identity.SupportActor != "" {
			return operatortool.ErrAccessDenied
		}
		if in.InvitationID != "" {
			_, _, err = s.invitedOrganizationFor(ctx, session, in.InvitationID)
		}
		return err
	default:
		return operatoradmin.ErrUnavailable
	}
}

func (s *Service) switchOrganizationFor(ctx context.Context, session accountSession, id string) (organizationChoice, error) {
	choices, err := s.organizationChoices(ctx, session)
	if err != nil {
		return organizationChoice{}, err
	}
	for _, choice := range choices {
		if choice.ID == id {
			return choice, nil
		}
	}
	return organizationChoice{}, operatortool.ErrAccessDenied
}

func (s *Service) invitedOrganizationFor(ctx context.Context, session accountSession, id string) (Organization, auth.Invitation, error) {
	invitation, err := auth.LookupInvitationID(ctx, s.config.Provider, id)
	if err != nil || auth.InvitationProblem(invitation, session.Subject, session.Email, s.config.now()) != "" {
		return Organization{}, auth.Invitation{}, operatortool.ErrAccessDenied
	}
	organization, err := s.registry.ByProvider(ctx, invitation.OrganizationID)
	if err != nil || organization.State != "ready" {
		return Organization{}, auth.Invitation{}, operatoradmin.ErrUnavailable
	}
	return organization, invitation, nil
}

func (a entryAdministration) Read(ctx context.Context, name string, in operatoradmin.Input) (any, error) {
	s := a.service
	session, err := s.currentAdministrationSession(ctx, operatortool.ConnectionIdentity(ctx))
	if err != nil {
		return nil, err
	}
	if name == platformMemberList {
		return s.readPlatformMembers(ctx, session)
	}
	if name == operatortool.ProvisioningPage {
		organization, err := s.creatorOrganizationFor(ctx, session, in.OrganizationID)
		if err != nil {
			return nil, err
		}
		return s.provisioningResult(organization)
	}
	if name == operatortool.OrganizationSession {
		return s.accountContextFor(ctx, session)
	}
	choices, err := s.organizationChoices(ctx, session)
	if err != nil {
		return nil, err
	}
	pending, err := s.pendingOrganizations(ctx, session.Subject)
	if err != nil {
		return nil, err
	}
	canCreate, err := s.canCreate(ctx, session)
	sort.Slice(choices, func(i, j int) bool { return choices[i].ID < choices[j].ID })
	return struct {
		Organizations operatoradmin.PageResult[organizationChoice]                 `json:"organizations"`
		Provisioning  operatoradmin.PageResult[templates.HostedOrganizationChoice] `json:"provisioning"`
		CanCreate     bool                                                         `json:"can_create"`
	}{operatoradmin.Page(choices, in), operatoradmin.Page(pending, in), canCreate}, err
}

func (a entryAdministration) Preview(ctx context.Context, name string, in operatoradmin.Input) (operatoradmin.Preview, error) {
	s := a.service
	session, err := s.currentAdministrationSession(ctx, operatortool.ConnectionIdentity(ctx))
	if err != nil {
		return operatoradmin.Preview{}, err
	}
	p := operatoradmin.Preview{Summary: name, Current: in}
	switch name {
	case operatortool.ResumeProvisioning:
		org, err := s.creatorOrganizationFor(ctx, session, in.OrganizationID)
		if err != nil {
			return p, err
		}
		status, err := s.provisioningResult(org)
		if err != nil {
			return p, err
		}
		p.ResourceID = org.ID
		p.Current = struct {
			Status     provisioningResult `json:"status"`
			Attempts   int                `json:"attempts"`
			Generation int64              `json:"generation"`
		}{status, org.Attempts, org.Generation}
	case operatortool.OrganizationDelete:
		org, err := s.ownerOrganizationFor(ctx, session, in.OrganizationID)
		if err != nil {
			return p, err
		}
		if in.ConfirmName != org.Name {
			return p, operatortool.ErrInvalidArguments
		}
		p.ResourceID = org.ID
		p.Current = struct {
			ID, Name   string
			Generation int64
		}{org.ID, org.Name, org.Generation}
	case operatortool.OrganizationSwitch:
		choice, err := s.switchOrganizationFor(ctx, session, in.OrganizationID)
		if err != nil {
			return p, err
		}
		p.ResourceID = choice.ID
		p.Current = choice
	case operatortool.InvitationAccept:
		org, invitation, err := s.invitedOrganizationFor(ctx, session, in.InvitationID)
		if err != nil {
			return p, err
		}
		p.ResourceID = org.ID
		p.Current = struct{ Organization, Email, Role string }{org.ID, invitation.Email, ""}
	case operatortool.SupportStart:
		org, err := s.readyOrganization(ctx, in.OrganizationID)
		if err != nil {
			return p, err
		}
		p.ResourceID = org.ID
	}
	return p, nil
}

func (a entryAdministration) Execute(ctx context.Context, name string, in operatoradmin.Input, m mutation.Metadata) (operatoradmin.Output, error) {
	s := a.service
	session, err := s.currentAdministrationSession(ctx, operatortool.ConnectionIdentity(ctx))
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if platformMemberOperation(name) {
		return a.executePlatformMember(ctx, name, in, m, session)
	}
	if name == operatortool.SessionLogout {
		outcome, err := s.logoutFor(ctx, session)
		return operatoradmin.Output{URL: "https://detent.build", Reconnect: true, SignOut: &outcome}, err
	}
	// Reuse the entry application's audit ledger and serialization for receipts;
	// no separate persistence owner, retry recovery, or provider retry is added.
	if name == operatortool.OrganizationCreate {
		id, err := s.createOrganizationFor(ctx, session, in.Name, m.RetryIdentity, in.Price)
		return operatoradmin.Output{ResourceID: id, URL: s.config.PublicURL + "/organizations/" + id + "/provisioning", Reconnect: true}, err
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if name == operatortool.ResumeProvisioning {
		if err := a.Authorize(ctx, name, in, m.ResourceID); err != nil {
			return operatoradmin.Output{}, err
		}
	}
	if out, found, err := s.administrationReceipt(ctx, session, m); err != nil || found {
		return out, err
	}
	if err := s.recordAdministrationReceipt(ctx, session, m, "pending", operatoradmin.Output{}); err != nil {
		return operatoradmin.Output{}, err
	}
	out := operatoradmin.Output{ResourceID: m.ResourceID, Reconnect: true}
	switch name {
	case operatortool.ResumeProvisioning:
		var org Organization
		org, err = s.resumeProvisioningFor(ctx, session, in.OrganizationID)
		if err == nil {
			var status provisioningResult
			status, err = s.provisioningResult(org)
			if err == nil {
				out.Data, err = json.Marshal(status)
				out.URL = s.config.PublicURL + "/organizations/" + org.ID + "/provisioning"
				if status.Next != "" {
					out.URL = s.config.PublicURL + status.Next
				}
			}
		}
	case operatortool.OrganizationSwitch:
		var choice organizationChoice
		choice, err = s.switchOrganizationFor(ctx, session, in.OrganizationID)
		out.URL = s.config.PublicURL + choice.URL
	case operatortool.OrganizationDelete:
		var org Organization
		org, err = s.ownerOrganizationFor(ctx, session, in.OrganizationID)
		if err == nil {
			err = s.deleteOrganizationFor(ctx, session, org, in.ConfirmName)
		}
		out.URL = s.config.PublicURL + "/organizations"
	case operatortool.InvitationAccept:
		var org Organization
		org, err = s.joinOrganizationFor(ctx, session, in.InvitationID)
		out.ResourceID = org.ID
		out.URL = s.config.PublicURL + "/auth/oidc/start?organization=" + url.QueryEscape(org.ID)
	case operatortool.SupportStart:
		// Support requires the existing browser cookie and provider impersonation.
		// Return the authorized entry step; never mint platform/customer authority.
		out.URL = s.config.PublicURL + "/support?organization=" + url.QueryEscape(in.OrganizationID) + "&reason=" + url.QueryEscape(in.Reason)
		out.Data = json.RawMessage(`{"interactive_required":true}`)
	default:
		err = operatoradmin.ErrUnavailable
	}
	if err != nil {
		return operatoradmin.Output{}, err
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err = s.recordAdministrationReceipt(persist, session, m, "succeeded", out); err != nil {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	return out, nil
}

type administrationReceipt struct {
	Kind    string               `json:"kind"`
	Retry   string               `json:"retry"`
	Hash    string               `json:"hash"`
	Outcome string               `json:"outcome"`
	Output  operatoradmin.Output `json:"output"`
}

func (s *Service) administrationReceipt(ctx context.Context, session accountSession, m mutation.Metadata) (operatoradmin.Output, bool, error) {
	var raw string
	err := s.auth.store.db.QueryRowContext(ctx, `SELECT event FROM audit WHERE subject=? AND organization_id=? AND CASE WHEN json_valid(event) THEN json_extract(event,'$.kind') END='operator_administration' AND json_extract(CASE WHEN json_valid(event) THEN event ELSE '{}' END,'$.retry')=? ORDER BY id DESC LIMIT 1`, session.Subject, m.OrganizationID, m.RetryIdentity).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return operatoradmin.Output{}, false, nil
	}
	if err != nil {
		return operatoradmin.Output{}, false, err
	}
	var receipt administrationReceipt
	if json.Unmarshal([]byte(raw), &receipt) != nil {
		return operatoradmin.Output{}, false, operatoradmin.ErrUnavailable
	}
	if receipt.Hash != m.InputHash {
		return operatoradmin.Output{}, false, mutation.ErrConflict
	}
	if receipt.Outcome != "succeeded" {
		return operatoradmin.Output{}, false, mutation.ErrUncertain
	}
	return receipt.Output, true, nil
}
func (s *Service) recordAdministrationReceipt(ctx context.Context, session accountSession, m mutation.Metadata, outcome string, out operatoradmin.Output) error {
	raw, err := json.Marshal(administrationReceipt{Kind: "operator_administration", Retry: m.RetryIdentity, Hash: m.InputHash, Outcome: outcome, Output: out})
	if err != nil {
		return err
	}
	return s.auth.audit(ctx, session.Subject, m.OrganizationID, string(raw))
}
func (a entryAdministration) Audit(ctx context.Context, m mutation.Metadata, outcome string) {
	m.RetryIdentity, m.InputHash = "", ""
	a.service.config.Logger.InfoContext(ctx, "operator mutation", "audit", m, "outcome", outcome)
}

// accountContext is the semantic browser landing/session projection without form secrets.
type accountContext struct {
	Subject      string `json:"subject"`
	Email        string `json:"email"`
	CanCreate    bool   `json:"can_create"`
	CanSupport   bool   `json:"can_support"`
	PlatformRole string `json:"platform_role"`
	Destination  string `json:"destination"`
	Reconnect    bool   `json:"reconnect"`
}

func (s *Service) accountContextFor(ctx context.Context, session accountSession) (accountContext, error) {
	canCreate, err := s.canCreate(ctx, session)
	return accountContext{Subject: session.Subject, Email: session.Email, CanCreate: canCreate, CanSupport: session.Identity.SupportActor == "" && s.supportActor(ctx, session.Email), PlatformRole: s.sessionPlatformRole(ctx, session), Destination: s.config.PublicURL + s.landing(ctx, session.Email, session.Identity), Reconnect: true}, err
}
