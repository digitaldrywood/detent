package cloudentry

import (
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
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
	body, err := storage.Open(ctx, request.Reference.Key)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(body, request.Reference.CompressedSize+1))
	if err = errors.Join(err, body.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) != request.Reference.CompressedSize || artifact.Digest(data) != request.Reference.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}
