package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedaccounts"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// A dedicated bootstrap/staff account may not yet have customer membership.
// This identity can use account setup only; it carries no project/admin role.
func (s *Service) hostedAccountCredential(ctx context.Context, hash string) (hubapi.APICredential, error) {
	session, err := s.WebSession(ctx, hash, s.config.Clock())
	if err != nil || session.Identity == nil || session.Identity.SupportActor != "" {
		return hubapi.APICredential{}, operatortool.ErrAccessDenied
	}
	return hubapi.APICredential{ID: "account:" + session.Identity.Subject, Hash: hash, SessionHash: hash, Scope: hubapi.APIScopeOperator, NativeOnly: true, Hosted: session.Identity, HostedRole: "account"}, nil
}

func (s *Service) createHostedOrganizationFor(ctx context.Context, credential hubapi.APICredential, name string) (string, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" || credential.Hosted.Subject != s.config.Hosted.BootstrapSubject {
		return "", operatortool.ErrAccessDenied
	}
	session, err := s.storedWebSession(ctx, credential.SessionHash, s.config.Clock())
	if err != nil || hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) {
		return "", operatortool.ErrAccessDenied
	}
	var members []accessstore.Member
	if err := s.accessRead(readCtx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		members, err = r.AccessMembers(readCtx, "", 1)
		return err
	}); err != nil || len(members) != 0 {
		return "", operatortool.ErrAccessDenied
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return "", operatortool.ErrInvalidArguments
	}
	providerID, err := s.hostedProviderOrganization(ctx)
	if err != nil {
		return "", err
	}
	if providerID == "" {
		organization, err := s.config.Hosted.Provider.CreateOrganization(ctx, s.config.Hosted.OrganizationID, name)
		if err != nil || organization.ExternalID != s.config.Hosted.OrganizationID || !hostedplans.SafeID(organization.ID) {
			return "", auth.ErrHostedIdentity
		}
		providerID = organization.ID
		if err := s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return w.BindAccessTenantProvider(ctx, providerID) }); err != nil {
			return "", err
		}
	}
	membership, err := s.config.Hosted.Provider.CreateMembership(ctx, credential.Hosted.Subject, providerID, "owner")
	if err != nil {
		return "", err
	}
	if err := s.storeHostedMember(ctx, auth.Identity{Subject: credential.Hosted.Subject, Email: session.Email, EmailVerified: true}, membership, name); err != nil {
		return "", err
	}
	return s.config.Hosted.OrganizationID, nil
}

type accountReceipt = hostedaccounts.Receipt

func (s *Service) executeHostedAccountFor(ctx context.Context, credential hubapi.APICredential, name string, in operatoradmin.Input, metadata mutation.Metadata) (operatoradmin.Output, error) {
	service := hostedaccounts.New(hostedaccounts.Deps{
		Now:                s.config.Clock,
		HostedOrganization: s.config.Hosted.OrganizationID,
		PublicURL:          s.config.Hosted.PublicURL,
		ReadReceipt:        s.readAccessAccountReceipt,
		StoreReceipt:       s.storeAccessAccountReceipt,
		CreateOrganization: s.createHostedOrganizationFor,
		StoredSession:      s.storedWebSession,
		AcceptInvitation:   s.acceptHostedInvitationIDFor,
	})
	return service.Execute(ctx, credential, name, in, metadata)
}
