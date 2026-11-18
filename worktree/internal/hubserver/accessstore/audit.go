package accessstore

import (
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Audit struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	SessionID      string
	ActualActor    string
	EffectiveUser  string
	Reason         string
	Event          string
	Route          string
	ProjectID      tracker.ProjectID
	Status         int
	StartedAt      time.Time
	ExpiresAt      time.Time
	RecordedAt     time.Time
	RecordedAtNS   int64
	Mutation       *attachment.ArchiveReference
	AccountKind    string
	AccountRetry   string
	KeyID          string
	KeyKind        string
	KeyOwner       string
	Resource       string
}

func SessionAudit(session Session, event, route string, at time.Time) Audit {
	actor := session.Subject
	if session.SupportActor != "" {
		actor = session.SupportActor
	}
	return Audit{OrganizationID: session.OrganizationID, SessionID: session.SessionID, ActualActor: actor, EffectiveUser: session.Subject, Reason: session.SupportReason, Event: event, Route: route, Status: http.StatusOK, StartedAt: session.IdentityCreatedAt, ExpiresAt: session.IdentityExpiresAt, RecordedAt: at, RecordedAtNS: at.UnixNano()}
}
