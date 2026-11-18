package auth

import (
	"context"
	"errors"
	"time"
)

var ErrHostedIdentity = errors.New("hosted identity is unavailable or inactive")

// ErrAccessExpired reports a well-formed access token whose lifetime ended;
// the caller refreshes it instead of treating the session as revoked.
var ErrAccessExpired = errors.New("hosted access token expired")

// HostedTokens are the provider tokens behind one signed-in session. They are
// kept out of HostedIdentity so they are never serialized with it.
type HostedTokens struct {
	AccessToken  string
	RefreshToken string
}

// HostedAccess is what a verified access token asserts.
type HostedAccess struct {
	Subject        string
	OrganizationID string
	SessionID      string
	Role           string
	SupportActor   string
	ExpiresAt      time.Time
}

type HostedIdentity struct {
	FirstName         string    `json:"-"`
	LastName          string    `json:"-"`
	ProfilePictureURL string    `json:"-"`
	Subject           string    `json:"subject"`
	OrganizationID    string    `json:"organization_id"`
	SessionID         string    `json:"session_id"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	SupportActor      string    `json:"support_actor,omitempty"`
	SupportReason     string    `json:"support_reason,omitempty"`
}

type Organization struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
}

type Membership struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
	Status         string `json:"status"`
	Role           struct {
		Slug string `json:"slug"`
	} `json:"role"`
}

type HostedUser struct {
	FirstName         string `json:"first_name,omitempty"`
	LastName          string `json:"last_name,omitempty"`
	ProfilePictureURL string `json:"profile_picture_url,omitempty"`
	ID                string `json:"id"`
	Email             string `json:"email"`
	Name              string `json:"name"`
}

type HostedUserProvider interface {
	User(context.Context, string) (HostedUser, error)
}

func LookupHostedUser(ctx context.Context, provider HostedProvider, id string) (HostedUser, error) {
	users, ok := provider.(HostedUserProvider)
	if !ok {
		return HostedUser{}, ErrHostedIdentity
	}
	return users.User(ctx, id)
}

type Invitation struct {
	ID             string    `json:"id"`
	Email          string    `json:"email"`
	OrganizationID string    `json:"organization_id"`
	State          string    `json:"state"`
	ExpiresAt      time.Time `json:"expires_at"`
	AcceptedUserID string    `json:"accepted_user_id"`
}

type HostedProvider interface {
	IdentityProvider
	CurrentSession(context.Context, HostedIdentity) (HostedIdentity, error)
	VerifyAccess(context.Context, string) (HostedAccess, error)
	RefreshAccess(context.Context, string, string) (HostedAccess, HostedTokens, error)
	Memberships(context.Context, string, string) ([]Membership, error)
	Organization(context.Context, string) (Organization, error)
	CreateOrganization(context.Context, string, string) (Organization, error)
	CreateMembership(context.Context, string, string, string) (Membership, error)
	SetMembershipRole(context.Context, string, string) error
	RevokeMembership(context.Context, string) error
	Invite(context.Context, string, string, string, string) (Invitation, error)
	Invitation(context.Context, string) (Invitation, error)
	HasUser(context.Context, string) (bool, error)
	AcceptInvitation(context.Context, string, string) error
	RevokeSession(context.Context, string) error
}

type OrganizationRole struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func OrganizationRoles() []OrganizationRole {
	return []OrganizationRole{
		{Slug: "owner", Name: "Owner", Description: "Owns the organization and manages its settings and members."},
		{Slug: "admin", Name: "Admin", Description: "Manages the organization, members and projects."},
		{Slug: "member", Name: "Member", Description: "Works in the projects you give them."},
		{Slug: "reporter", Name: "Reporter", Description: "Reads issues and files new ones into Triage."},
		{Slug: "viewer", Name: "Viewer", Description: "Reads the projects you give them."},
	}
}

func ValidOrganizationRole(role string) bool {
	for _, option := range OrganizationRoles() {
		if option.Slug == role {
			return true
		}
	}
	return false
}

func ValidSupportReason(reason string) bool {
	switch reason {
	case "customer-request", "account-recovery", "troubleshooting":
		return true
	default:
		return false
	}
}

type WidgetTokenProvider interface {
	WidgetToken(context.Context, string) (string, error)
}
