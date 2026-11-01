package cloudentry

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func (s *Service) tenantContent(ctx context.Context, organization Organization, request entryauthority.ContentRequest) ([]byte, error) {
	if request.Organization != organization.ID || request.Reference.Validate(organization.ID) != nil {
		return nil, attachment.ErrInvalid
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	current, err := s.readyOrganization(ctx, organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return nil, attachment.ErrInvalid
	}
	storage, err := s.archiveStorageForReference(ctx, organization.ID, request.Reference)
	if err != nil {
		return nil, err
	}
	if request.Write {
		return nil, attachment.StoreArchive(ctx, storage, organization.ID, request.Reference, request.Data)
	}
	return attachment.ReadArchiveObject(ctx, storage, organization.ID, request.Reference)
}
