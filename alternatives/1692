package eventstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AuditDisposition struct {
	ActorID        string
	RequestKey     string
	RequestHash    string
	Organization   tracker.OrganizationID
	Project        tracker.ProjectID
	ChangeID       string
	ReviewSequence int64
	Disposition    securityaudit.Disposition
	Reference      attachment.ArchiveReference
}

type AuditDispositionReader interface {
	AuditDispositionRequest(context.Context, tracker.ProjectID, string, string) (AuditDisposition, error)
	AuditReviewCandidates(context.Context, tracker.ProjectID, securityaudit.Key, int) ([]Evidence, error)
	AuditDispositions(context.Context, tracker.ProjectID, int64) ([]AuditDisposition, error)
}

type AuditDispositionWriter interface {
	AuditDispositionReader
	CreateAuditDisposition(context.Context, AuditDisposition) (AuditDisposition, error)
}
