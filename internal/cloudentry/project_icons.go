package cloudentry

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
)

type tenantIconStore struct {
	service    *Service
	allocation Organization
}

func (s tenantIconStore) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	if len(content) > 256*1024 || organization != s.allocation.ID {
		return attachment.ErrInvalid
	}
	unlock := s.service.attachmentLocks.lock(organization, false)
	defer unlock()
	current, err := s.service.readyOrganization(ctx, organization)
	if err != nil || current.Generation != s.allocation.Generation || current.Endpoint != s.allocation.Endpoint {
		return attachment.ErrInvalid
	}
	return (attachment.Icons{Storage: s.service.attachments}).PutIcon(ctx, organization, id, content)
}

func (s tenantIconStore) DeleteIcon(ctx context.Context, organization, id string) error {
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	if organization != s.allocation.ID {
		return attachment.ErrInvalid
	}
	unlock := s.service.attachmentLocks.lock(organization, false)
	defer unlock()
	current, err := s.service.readyOrganization(ctx, organization)
	if err != nil || current.Generation != s.allocation.Generation || current.Endpoint != s.allocation.Endpoint {
		return attachment.ErrInvalid
	}
	return (attachment.Icons{Storage: s.service.attachments}).DeleteIcon(ctx, organization, id)
}
