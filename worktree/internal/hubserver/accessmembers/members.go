package accessmembers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository    tenantstore.Repository
	Organization  tracker.OrganizationID
	Now           func() time.Time
	GenerateToken func() (string, error)
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) read(ctx context.Context, read func(tenantstore.Reader) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.ReadTenant(ctx, s.deps.Organization, corestore.Serving, read)
}

type Preparation struct {
	organization tracker.OrganizationID
	subject      string
	principal    string
	hash         string
	fingerprint  string
}

func (s *Service) Prepare(ctx context.Context, identity auth.Identity) (Preparation, error) {
	prepared := Preparation{organization: s.deps.Organization, subject: identity.Subject, principal: "hosted_" + apikey.HashToken(identity.Subject)[:32]}
	if prepared.organization == "" || prepared.subject == "" || !identity.EmailVerified {
		return Preparation{}, auth.ErrHostedIdentity
	}
	err := s.read(ctx, func(r tenantstore.Reader) error {
		_, err := r.AccessCredential(ctx, prepared.principal)
		return err
	})
	if err == nil {
		return prepared, nil
	}
	if errors.Is(err, tenantauth.ErrUnavailable) || !errors.Is(err, corestore.ErrNotFound) {
		return Preparation{}, err
	}
	token, err := s.deps.GenerateToken()
	if err != nil {
		return Preparation{}, err
	}
	prepared.hash = apikey.HashToken(token)
	prepared.fingerprint = fingerprint(prepared.hash)
	return prepared, nil
}

func (s *Service) Store(ctx context.Context, w tenantstore.Writer, identity auth.Identity, membership auth.Membership, prepared Preparation) error {
	if !identity.EmailVerified || identity.Subject != membership.UserID || membership.Status != "active" || !auth.ValidOrganizationRole(membership.Role.Slug) {
		return auth.ErrHostedIdentity
	}
	principal := "hosted_" + apikey.HashToken(identity.Subject)[:32]
	if prepared.organization != s.deps.Organization || prepared.subject != identity.Subject || prepared.principal != principal {
		return auth.ErrHostedIdentity
	}
	if err := w.LockAccessOrganization(ctx); err != nil {
		return err
	}
	tenant, err := w.AccessTenant(ctx)
	if err != nil {
		return err
	}
	if membership.OrganizationID != tenant.ProviderID {
		return auth.ErrHostedIdentity
	}
	now := s.deps.Now()
	if _, err := w.AccessCredential(ctx, principal); errors.Is(err, tenantauth.ErrUnavailable) {
		return err
	} else if errors.Is(err, corestore.ErrNotFound) {
		if prepared.hash == "" || prepared.fingerprint == "" {
			return auth.ErrHostedIdentity
		}
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: s.deps.Organization, ID: principal, Name: principal, Hash: prepared.hash, Fingerprint: prepared.fingerprint, Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	member, err := w.AccessMember(ctx, identity.Subject)
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return w.CreateAccessMember(ctx, accessstore.Member{OrganizationID: s.deps.Organization, UserID: identity.Subject, Email: strings.ToLower(identity.Email), MembershipID: membership.ID, Role: membership.Role.Slug, Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now, NotifyIssues: true})
	}
	if err != nil {
		return err
	}
	if member.MembershipID != membership.ID || !member.Active {
		if err := w.ClearAccessMemberGrants(ctx, identity.Subject); err != nil {
			return err
		}
	} else if accessstore.Demoted(member.Role, membership.Role.Slug) {
		if err := w.ClearAccessRoleGrants(ctx, identity.Subject); err != nil {
			return err
		}
	}
	member.Email, member.MembershipID, member.Role, member.Active, member.UpdatedAt = strings.ToLower(identity.Email), membership.ID, membership.Role.Slug, true, now
	return w.UpdateAccessMember(ctx, member)
}

func fingerprint(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func (s *Service) write(ctx context.Context, write func(tenantstore.Writer) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.WriteTenant(ctx, s.deps.Organization, corestore.Serving, write)
}

func (s *Service) Publish(ctx context.Context, identity auth.Identity, membership auth.Membership, organizationName string) (resultErr error) {
	prepared, err := s.Prepare(ctx, identity)
	if err != nil {
		return err
	}
	return s.write(ctx, func(w tenantstore.Writer) error {
		if err := s.Store(ctx, w, identity, membership, prepared); err != nil {
			return err
		}
		if organizationName != "" {
			return w.UpdateAccessOrganizationName(ctx, organizationName)
		}
		return nil
	})
}
