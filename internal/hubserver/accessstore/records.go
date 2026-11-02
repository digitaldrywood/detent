package accessstore

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrTransactionRequired = errors.New("access mutation requires the shared tenant transaction")

var ErrDenied = errors.New("tenant access is unavailable or inactive")

var ErrSeatLimit = errors.New("hosted member allowance is exhausted")

type Organization struct {
	OrganizationID tracker.OrganizationID
	Name           string
	Slug           string
	Local          bool
	CreatedAt      time.Time
}

type HubIdentity struct {
	OrganizationID tracker.OrganizationID
	ID             string
	CursorKey      []byte
}

type Tenant struct {
	OrganizationID       tracker.OrganizationID
	ProviderID           string
	BootstrapSubject     string
	Deployment           string
	PublicURL            string
	AllocationGeneration int64
}

type Credential struct {
	OrganizationID tracker.OrganizationID
	ID             string
	Name           string
	Hash           string
	Fingerprint    string
	Scope          string
	NativeOnly     bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	RotatedAt      *time.Time
	RevokedAt      *time.Time
	ExpiresAt      *time.Time
	LastUsedAt     *time.Time
	UserID         string
	MembershipID   string
	KeyScope       string
	ProjectAccess  string
	Kind           string
}

type Member struct {
	OrganizationID tracker.OrganizationID
	UserID         string
	Email          string
	MembershipID   string
	Role           string
	Active         bool
	PrincipalID    string
	NotifyIssues   bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ProjectGrant struct {
	OrganizationID tracker.OrganizationID
	UserID         string
	ProjectID      tracker.ProjectID
	CanWrite       bool
	ManageRunner   bool
}

type TokenGrant struct {
	OrganizationID tracker.OrganizationID
	TokenID        string
	ProjectID      tracker.ProjectID
}

type Session struct {
	OrganizationID         tracker.OrganizationID
	Hash                   string
	Email                  string
	Subject                string
	ProviderOrganizationID string
	SessionID              string
	IdentityCreatedAt      time.Time
	IdentityExpiresAt      time.Time
	SupportActor           string
	SupportReason          string
	CreatedAt              time.Time
	ExpiresAt              time.Time
	RevokedAt              *time.Time
}

type LoginTransaction struct {
	OrganizationID         tracker.OrganizationID
	Hash                   string
	State                  string
	Verifier               string
	ProviderOrganizationID string
	SupportActor           string
	SupportSession         string
	InvitationToken        string
	ExpiresAt              time.Time
	ConsumedAt             *time.Time
}

type Invitation struct {
	OrganizationID tracker.OrganizationID
	ID             string
	Email          string
	Role           string
	AcceptedUserID string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type InvitationGrant struct {
	OrganizationID tracker.OrganizationID
	InvitationID   string
	ProjectID      tracker.ProjectID
	CanWrite       bool
	ManageRunner   bool
}

type MemberReservation struct {
	OrganizationID tracker.OrganizationID
	Email          string
	ExpiresAt      time.Time
}

type BindingMigration struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	FromPublicURL  string
	ToPublicURL    string
	FromDeployment string
	FromGeneration int64
	ToGeneration   int64
	MigratedAt     time.Time
	Applied        bool
}

type Secret struct {
	OrganizationID   tracker.OrganizationID
	ProjectID        tracker.ProjectID
	Kind             string
	OrganizationSlug string
	Envelope         hubsecrets.Envelope
	UpdatedAt        time.Time
}

type SecretAudit struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Actor          string
	Kind           string
	Event          string
	KeyVersion     int
	RecordedAt     time.Time
}

func SecretAAD(organization tracker.OrganizationID, project tracker.ProjectID, kind string) []byte {
	encoded, err := json.Marshal([3]string{string(organization), string(project), kind})
	if err != nil {
		return nil
	}
	return encoded
}

func (c Credential) Valid(hash string, now time.Time) bool {
	return subtle.ConstantTimeCompare([]byte(c.Hash), []byte(hash)) == 1 && c.RevokedAt == nil && !now.Before(c.CreatedAt) && (c.ExpiresAt == nil || now.Before(*c.ExpiresAt))
}

type Authority struct {
	Action       string
	CredentialID string
	Hash         string
	UserID       string
	MembershipID string
	SessionHash  string
	Role         string
	ProjectID    tracker.ProjectID
	Write        bool
	Admin        bool
}

type ProjectAccessFilter struct {
	UserID                 string
	PrincipalID            string
	CredentialID           string
	KeyKind                string
	ProjectAccess          string
	RequireCredentialGrant bool
	Projects               []tracker.ProjectID
	BlockedProjects        []tracker.ProjectID
	After                  tracker.ProjectID
	Only                   tracker.ProjectID
	Limit                  int
}

type ProjectAccess struct {
	ProjectID    tracker.ProjectID
	CanWrite     bool
	ManageRunner bool
}

type ProjectAccessPageReader interface {
	ProjectAccessPage(context.Context, ProjectAccessFilter) ([]ProjectAccess, error)
}

type ProjectAccessCleaner interface {
	DeleteAccessProjectGrants(context.Context, tracker.ProjectID) error
}

type Reader interface {
	HubIdentity(context.Context) (HubIdentity, error)
	AccessMemberCredentials(context.Context, string, string, int) ([]Credential, error)
	AccessAudit(context.Context, int64) (Audit, error)
	AccessMembers(context.Context, string, int) ([]Member, error)
	AccessMembersByEmail(context.Context, string, string, int) ([]Member, error)
	AccessCredentials(context.Context, string, int) ([]Credential, error)
	AccessInvitations(context.Context, string, int) ([]Invitation, error)
	AccessInvitationGrants(context.Context, string, tracker.ProjectID, int) ([]InvitationGrant, error)
	AccessProjectGrants(context.Context, string, tracker.ProjectID, int) ([]ProjectGrant, error)
	AccessTokenGrants(context.Context, string, tracker.ProjectID, int) ([]TokenGrant, error)
	AccessOrganization(context.Context) (Organization, error)
	AccessOrganizationSlug(context.Context, string) (bool, error)
	AccessTenant(context.Context) (Tenant, error)
	AccessCredential(context.Context, string) (Credential, error)
	AccessMember(context.Context, string) (Member, error)
	AccessProjectGrant(context.Context, string, tracker.ProjectID) (ProjectGrant, error)
	AccessTokenGrant(context.Context, string, tracker.ProjectID) (TokenGrant, error)
	AccessSession(context.Context, string) (Session, error)
	AccessLoginTransaction(context.Context, string) (LoginTransaction, error)
	AccessInvitation(context.Context, string) (Invitation, error)
	AccessInvitationGrant(context.Context, string, tracker.ProjectID) (InvitationGrant, error)
	AccessMemberReservation(context.Context, string) (MemberReservation, error)
	AccessMemberSeatCounts(context.Context, time.Time) (int64, int64, error)
	AccessBindingMigration(context.Context, int64) (BindingMigration, error)
	CredentialByHash(context.Context, string) (Credential, error)
	AuthorizeAccess(context.Context, Authority, time.Time) (Member, error)
	AccessSecret(context.Context, tracker.ProjectID, string) (Secret, error)
	AccessSecrets(context.Context, tracker.ProjectID, string, int) ([]Secret, error)
}

type Writer interface {
	CreateHubIdentity(context.Context, HubIdentity) error
	UpdateAccessKeyAudit(context.Context, int64, string, string, tracker.ProjectID, string, int) error
	SyncHostedAdministratorProjectAccess(context.Context, string, string) error
	RecordAccessAudit(context.Context, Audit) (int64, error)
	EnsureAccessCredential(context.Context, Credential) error
	SetAccessTransactionInvitation(context.Context, string, string) error
	DeleteAccessTokenGrant(context.Context, string, tracker.ProjectID) error
	RewrapAccessSecret(context.Context, Secret, hubsecrets.Envelope, SecretAudit) error
	EnsureAccessSession(context.Context, Session) error
	ClearAccessMemberGrants(context.Context, string) error
	SetAccessProjectGrant(context.Context, ProjectGrant) error
	SetAccessTokenGrant(context.Context, TokenGrant) error
	Reader
	CreateAccessOrganization(context.Context, Organization) error
	LockAccessOrganization(context.Context) error
	UpdateAccessOrganizationName(context.Context, string) error
	AddAccessOrganizationSlug(context.Context, string) error
	SetAccessOrganizationSlug(context.Context, *string, string) error
	CreateAccessTenant(context.Context, Tenant) error
	CreateAccessCredential(context.Context, Credential) error
	CreateAccessMember(context.Context, Member) error
	CreateAccessProjectGrant(context.Context, ProjectGrant) error
	CreateAccessTokenGrant(context.Context, TokenGrant) error
	CreateAccessSession(context.Context, Session) error
	CreateAccessLoginTransaction(context.Context, LoginTransaction) error
	CreateAccessInvitation(context.Context, Invitation) error
	CreateAccessInvitationGrant(context.Context, InvitationGrant) error
	CreateAccessMemberReservation(context.Context, MemberReservation) error
	ReserveAccessMemberReservation(context.Context, MemberReservation, time.Time, int64) (bool, error)
	DeleteAccessMemberReservation(context.Context, string) error
	CreateAccessBindingMigration(context.Context, BindingMigration) error
	UpdateAccessMember(context.Context, Member) error
	RevokeAccessMember(context.Context, string, time.Time) error
	RotateAccessCredential(context.Context, string, string, string, time.Time) error
	RenewAccessCredential(context.Context, string, string, string, time.Time, time.Time) error
	RevokeAccessCredential(context.Context, string, time.Time) error
	RevokeAccessSession(context.Context, string, time.Time) error
	ConsumeAccessTransaction(context.Context, string, string, time.Time) (LoginTransaction, error)
	AcceptAccessInvitation(context.Context, string, string, string, time.Time) error
	ReplaceAccessInvitationGrants(context.Context, string, []InvitationGrant) error
	DeleteAccessInvitation(context.Context, string) (string, error)
	DeleteAccessProjectGrant(context.Context, string, tracker.ProjectID) error
	StoreAccessSecret(context.Context, Secret, SecretAudit) error
	DeleteAccessSecret(context.Context, tracker.ProjectID, string, SecretAudit) error
	RecordAccessSecretAudit(context.Context, SecretAudit) error
}
