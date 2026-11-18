package policystore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OrganizationModels struct {
	OrganizationID tracker.OrganizationID
	Revision       int64
	Payload        attachment.ArchiveReference
}

type ModelReader interface {
	OrganizationModels(context.Context) (OrganizationModels, error)
}

type ModelWriter interface {
	ModelReader
	SetOrganizationModels(context.Context, OrganizationModels, int64) error
	RequireOrganizationModels(context.Context, int64) error
}
