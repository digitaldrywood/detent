package cloudentry

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type tenantIconStore struct {
	service    *Service
	allocation Organization
}

func (s tenantIconStore) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	if organization != s.allocation.ID {
		return attachment.ErrInvalid
	}
	current, err := s.service.readyOrganization(ctx, organization)
	if err != nil || current.Generation != s.allocation.Generation || current.Endpoint != s.allocation.Endpoint {
		return attachment.ErrInvalid
	}
	return (attachment.Icons{Storage: s.service.attachments}).PutIcon(ctx, organization, id, content)
}
