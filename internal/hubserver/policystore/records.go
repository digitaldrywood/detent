package policystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Revision struct {
	OrganizationID      tracker.OrganizationID
	ProjectID           tracker.ProjectID
	PolicyID            string
	ApprovedBy          string
	ApprovedAt          time.Time
	PayloadObjectKey    string
	PayloadSHA256       string
	PayloadBytes        int64
	PayloadContentBytes int64
}

type Reader interface {
	ModelReader
	ProjectPolicy(context.Context, tracker.ProjectID) (Revision, error)
}

type Writer interface {
	ModelWriter
	Reader
	ApproveProjectPolicy(context.Context, Revision, string) error
}
