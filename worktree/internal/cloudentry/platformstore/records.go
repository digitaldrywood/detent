package platformstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Organization struct {
	ID             tracker.OrganizationID
	ProviderID     string
	Slug           string
	Name           string
	State          string
	Endpoint       string
	Generation     int64
	Managed        bool
	CreatorSubject string
	CreatorEmail   string
	Step           string
	Attempts       int64
	NextAttemptAt  *time.Time
	ErrorCode      string
	ErrorDetail    string
	CheckoutPrice  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Session struct {
	Hash       string
	Subject    string
	Email      string
	CSRFSecret string
	Identity   auth.HostedIdentity
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

type Authorization struct {
	Binding        string
	SessionHash    string
	OrganizationID tracker.OrganizationID
	Identity       auth.HostedIdentity
	Support        bool
	EffectiveEmail string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RevokedAt      *time.Time
}

type LoginTransaction struct {
	Hash                   string
	ID                     string
	State                  string
	Verifier               string
	OrganizationID         string
	ReturnPath             string
	InvitationToken        string
	InvitationOrganization string
	SupportActor           string
	SupportSession         string
	SupportReason          string
	ExpiresAt              time.Time
}

type Key struct {
	ID                    string
	Hash                  string
	Name                  string
	Kind                  string
	OwnerSubject          string
	OwnerEmail            string
	Permission            string
	AccessContext         string
	ServiceOrganizationID *tracker.OrganizationID
	CreatedAt             time.Time
	ExpiresAt             *time.Time
	RevokedAt             *time.Time
	Organizations         []KeyOrganization
}

type KeyOrganization struct {
	OrganizationID tracker.OrganizationID
	ProjectAccess  string
	Projects       []tracker.ProjectID
}

type Member struct {
	Email     string
	Role      string
	AddedBy   string
	AddedAt   time.Time
	UpdatedAt time.Time
}

type OAuthClient struct {
	ID           string
	Name         string
	AuthMethod   string
	SecretHash   string
	RedirectURIs []string
}

type OAuthGrant struct {
	OrganizationID  tracker.OrganizationID
	ID              string
	ClientID        string
	OwnerSubject    string
	OwnerEmail      string
	Permission      string
	Resource        string
	CreatedAt       time.Time
	AccessHash      *string
	AccessExpiresAt *time.Time
	RevokedAt       *time.Time
}

type OAuthRequest struct {
	ID            string
	RedirectURI   string
	Challenge     string
	CodeHash      string
	CodeExpiresAt time.Time
}

type OAuthTokens struct {
	AccessHash       string
	RefreshHash      string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}
