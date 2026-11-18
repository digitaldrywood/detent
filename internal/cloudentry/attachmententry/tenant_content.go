package attachmententry

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func (s *Service) Content(ctx context.Context, organization Organization, request entryauthority.ContentRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if len(request.Data) > attachment.MaxArchiveBytes || request.Organization != organization.ID || request.Reference.Validate(organization.ID) != nil {
		return nil, attachment.ErrInvalid
	}
	unlock := s.Lock(organization.ID, false)
	defer unlock()
	current, err := s.Ready(ctx, organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return nil, attachment.ErrInvalid
	}
	storage, err := s.ArchiveStorage(ctx, organization.ID, request.Reference)
	if err != nil {
		return nil, err
	}
	if request.Write {
		return nil, attachment.StoreArchive(ctx, storage, organization.ID, request.Reference, request.Data)
	}
	return attachment.ReadArchiveObject(ctx, storage, organization.ID, request.Reference)
}
