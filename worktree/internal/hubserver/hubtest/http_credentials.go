package hubtest

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (h *HTTPHarness) AdminKey(t *testing.T) HTTPActor {
	t.Helper()
	return h.key(t, CredentialOptions{Scope: "admin", Now: h.Now()})
}
func (h *HTTPHarness) OperatorKey(t *testing.T, projects ...tracker.ProjectID) HTTPActor {
	t.Helper()
	return h.key(t, CredentialOptions{Projects: projects, Now: h.Now()})
}
func (h *HTTPHarness) key(t *testing.T, options CredentialOptions) HTTPActor {
	t.Helper()
	seeded := Credential(t, h.Repository, h.Organization, options)
	return HTTPActor{Token: seeded.Token, Credential: seeded}
}
func (h *HTTPHarness) RunnerCredential(t *testing.T, options RunnerOptions) (HTTPActor, SeededRunner) {
	t.Helper()
	if options.Now.IsZero() {
		options.Now = h.Now()
	}
	runner := Runner(t, h.Repository, h.Organization, options)
	return HTTPActor{Token: runner.Credential.Token, Credential: runner.Credential}, runner
}
func (h *HTTPHarness) PersonalKey(t *testing.T, member SeededMember, options CredentialOptions) HTTPActor {
	t.Helper()
	h.registerMember(member)
	options.Member = &member
	if options.Now.IsZero() {
		options.Now = h.Now()
	}
	return h.key(t, options)
}
func (h *HTTPHarness) MemberSession(t *testing.T, options MemberOptions) HTTPActor {
	t.Helper()
	if options.Now.IsZero() {
		options.Now = h.Now()
	}
	return h.SessionFor(t, Member(t, h.Repository, h.Organization, options), "", "")
}
func (h *HTTPHarness) StaffSession(t *testing.T, options MemberOptions, actor, reason string) HTTPActor {
	t.Helper()
	if h.hosted == nil {
		t.Fatal("staff session requires hosted configuration")
	}
	if options.Now.IsZero() {
		options.Now = h.Now()
	}
	member := Member(t, h.Repository, h.Organization, options)
	h.hosted.StaffEmails = append(h.hosted.StaffEmails, member.Member.Email)
	h.hosted.SupportActors = append(h.hosted.SupportActors, actor)
	return h.SessionFor(t, member, actor, reason)
}
func (h *HTTPHarness) SessionFor(t *testing.T, member SeededMember, supportActor, reason string) HTTPActor {
	t.Helper()
	h.registerMember(member)
	token := hubapi.NewID("session")
	hash := apikey.HashToken(token)
	now := seedTime(h.Now())
	write(t, h.Repository, member.Member.OrganizationID, "HTTP session", func(w tenantstore.Writer) error {
		return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: member.Member.OrganizationID, Hash: hash, Email: member.Member.Email, Subject: member.Member.UserID, ProviderOrganizationID: "provider_" + string(member.Member.OrganizationID), SessionID: hubapi.NewID("provider_session"), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour), SupportActor: supportActor, SupportReason: reason, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	})
	return HTTPActor{Token: token, Cookie: true, Member: member, Credential: member.Principal}
}
func (h *HTTPHarness) registerMember(member SeededMember) {
	h.mu.Lock()
	defer h.mu.Unlock()
	membership := auth.Membership{ID: member.Member.MembershipID, UserID: member.Member.UserID, OrganizationID: "provider_" + string(member.Member.OrganizationID), Status: "active"}
	membership.Role.Slug = member.Member.Role
	h.memberships[member.Member.UserID] = membership
}
func (h *HTTPHarness) SetProviderRole(subject, role string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	member := h.memberships[subject]
	member.Role.Slug = role
	h.memberships[subject] = member
}
func (h *HTTPHarness) RevokeMember(t *testing.T, member SeededMember) {
	t.Helper()
	write(t, h.Repository, member.Member.OrganizationID, "revoke member", func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), member.Member.UserID, h.Now())
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	membership := h.memberships[member.Member.UserID]
	membership.Status = "inactive"
	h.memberships[member.Member.UserID] = membership
}
