package cloudentry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) createOrganizationFor(ctx context.Context, session accountSession, name, key string) (string, error) {
	if s.config.Allocation == nil {
		return "", ErrOrganizationNotFound
	}
	if session.Identity.SupportActor != "" || !s.signupAllowed(session.Email) {
		return "", operatortool.ErrAccessDenied
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 || len(key) < 16 || len(key) > 128 || !safeID(key) {
		return "", operatortool.ErrInvalidArguments
	}
	fingerprint := sha256.Sum256([]byte(name))
	id, err := s.recordIntent(ctx, session, key, hex.EncodeToString(fingerprint[:]), name)
	if err != nil {
		return "", err
	}
	s.wakeAllocator()
	return id, nil
}

func (s *Service) ownerOrganizationFor(ctx context.Context, session accountSession, id string) (Organization, error) {
	organization, err := s.readyOrganization(ctx, id)
	if err != nil || !organization.Managed {
		return Organization{}, ErrOrganizationNotFound
	}
	authorized, err := s.auth.authorization(ctx, session, id)
	if err != nil || authorized.Support {
		return Organization{}, errNoSession
	}
	current, err := s.config.Provider.CurrentSession(ctx, authorized.Identity)
	if err != nil || current.Subject != session.Subject || current.OrganizationID != organization.ProviderID || !current.ExpiresAt.After(s.config.now()) {
		return Organization{}, errNoSession
	}
	memberships, err := s.config.Provider.Memberships(ctx, session.Subject, organization.ProviderID)
	if err != nil {
		return Organization{}, err
	}
	for _, member := range memberships {
		if member.UserID == session.Subject && member.OrganizationID == organization.ProviderID && member.Status == "active" && member.Role.Slug == "owner" {
			return organization, nil
		}
	}
	return Organization{}, errNoSession
}

func (s *Service) deleteOrganizationFor(ctx context.Context, session accountSession, organization Organization, confirm string) error {
	if confirm != organization.Name {
		return operatortool.ErrInvalidArguments
	}
	state, err := s.tenantBillingState(ctx, organization, true)
	if err != nil {
		return err
	}
	if billingBlocksDeletion(state, s.config.now()) {
		return errNoSession
	}
	if _, err := s.registry.store.db.ExecContext(ctx, "UPDATE organizations SET state='deleting',updated_at=? WHERE id=? AND state='ready'", formatTime(s.config.now()), organization.ID); err != nil {
		return err
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := s.finishDeletion(detached, organization.ID); err != nil {
		return err
	}
	return s.auth.audit(detached, session.Subject, organization.ID, "organization_deleted")
}

// Invitation acceptance is the same explicit tenant command used after login;
// it is never a generic URL or HTTP forwarding operation.
func (s *Service) joinOrganizationFor(ctx context.Context, session accountSession, reference string) (Organization, error) {
	invitation, err := auth.LookupInvitationID(ctx, s.config.Provider, reference)
	if err != nil || !strings.EqualFold(invitation.Email, session.Email) || !invitation.ExpiresAt.After(s.config.now()) || session.Identity.SupportActor != "" {
		return Organization{}, operatortool.ErrAccessDenied
	}
	organization, err := s.registry.ByProvider(ctx, invitation.OrganizationID)
	if err != nil || organization.State != "ready" {
		return Organization{}, ErrOrganizationNotFound
	}
	if err := s.acceptInvitationID(ctx, organization, session.Subject, session.Email, session.Identity.SessionID, reference); err != nil {
		return Organization{}, err
	}
	return organization, nil
}
