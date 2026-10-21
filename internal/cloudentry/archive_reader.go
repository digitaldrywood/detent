package cloudentry

import (
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func (s *Service) tenantArchiveReader(organization Organization) entryauthority.ArchiveReader {
	return func(ctx context.Context, ref attachment.ArchiveReference, presign bool) (entryauthority.ArchiveResponse, error) {
		var result entryauthority.ArchiveResponse
		if ref.Validate(organization.ID) != nil || ref.UncompressedSize > attachment.MaxSectionBytes {
			return result, attachment.ErrInvalid
		}
		unlock := s.attachmentLocks.lock(organization.ID, false)
		defer unlock()
		current, err := s.readyOrganization(ctx, organization.ID)
		if err != nil || current.Generation != organization.Generation {
			return result, attachment.ErrInvalid
		}
		storage, ok := s.attachments.(attachment.ArchiveStorage)
		if !ok {
			return result, attachment.ErrInvalid
		}
		if err := s.auth.audit(ctx, "tenant", organization.ID, "archive_read"); err != nil {
			return result, err
		}
		body, err := storage.Open(ctx, ref.Key)
		if err != nil {
			return result, err
		}
		result.Data, err = io.ReadAll(io.LimitReader(body, ref.CompressedSize+1))
		if err = errors.Join(err, body.Close()); err != nil {
			return entryauthority.ArchiveResponse{}, err
		}
		if _, err := attachment.DecodeArchiveSection(organization.ID, ref, result.Data); err != nil {
			return entryauthority.ArchiveResponse{}, err
		}
		if presign {
			access, err := storage.PresignArchive(ctx, ref)
			if err != nil {
				return entryauthority.ArchiveResponse{}, err
			}
			result.Access = &access
		}
		return result, nil
	}
}
