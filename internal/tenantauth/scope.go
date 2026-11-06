package tenantauth

import (
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrDenied = accessstore.ErrDenied
var ErrUnavailable = errors.New("tenant authority storage is unavailable")

type RequestIdentity struct {
	Organization        tracker.OrganizationID
	CredentialHash      string
	RenewalRunner       string
	RenewalOrganization tracker.OrganizationID
}

type Requirement struct {
	Project tracker.ProjectID
	Action  string
}

type Principal struct {
	CredentialID, Name, Scope                 string
	UserID, MembershipID, PrincipalID, Role   string
	KeyScope, ProjectAccess, Kind, ProviderID string
	NativeOnly                                bool
}

type Scope struct {
	organization tracker.OrganizationID
	principal    Principal
	runner       runnerauth.Identity
	generation   int64
	expires      *time.Time
	created      time.Time
	renewal      bool
	proof        RequestIdentity
}

func (s Scope) Organization() tracker.OrganizationID { return s.organization }
func (s Scope) Principal() Principal                 { return s.principal }
func (s Scope) RunnerID() string                     { return s.runner.RunnerID }
func (s Scope) Generation() int64                    { return s.generation }
func (s Scope) Renewal() bool                        { return s.renewal }
func (s Scope) String() string                       { return "tenantauth.Scope" }
func (s Scope) GoString() string                     { return s.String() }

func (s Scope) Runner() runnerauth.Identity {
	runner := s.runner
	runner.ProjectIDs = slices.Clone(runner.ProjectIDs)
	runner.Operations = slices.Clone(runner.Operations)
	return runner
}

func (s Scope) Actor() tracker.Actor {
	principal := s.principal.PrincipalID
	if principal == "" {
		principal = s.principal.CredentialID
	}
	return tracker.Actor{Kind: "key", PrincipalID: principal, ViaKey: s.principal.CredentialID, DisplayName: s.principal.Name}
}

func (s Scope) ValidAt(now time.Time) error {
	if s.organization == "" || s.principal.CredentialID == "" || now.Before(s.created) {
		return ErrDenied
	}
	if !s.renewal && (s.expires != nil && !now.Before(*s.expires) || s.RunnerID() != "" && !now.Before(s.runner.ExpiresAt)) {
		return ErrDenied
	}
	return nil
}
