package attachmententry

import (
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

func (s *Service) ArchiveReader(organization Organization) entryauthority.ArchiveReader {
	return func(ctx context.Context, ref attachment.ArchiveReference, presign bool) (entryauthority.ArchiveResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
		defer cancel()
		var result entryauthority.ArchiveResponse
		if ref.Validate(organization.ID) != nil || ref.UncompressedSize > attachment.MaxSectionBytes {
			return result, attachment.ErrInvalid
		}
		unlock := s.Lock(organization.ID, false)
		defer unlock()
		current, err := s.Ready(ctx, organization.ID)
		if err != nil || current.Generation != organization.Generation {
			return result, attachment.ErrInvalid
		}
		storage, err := s.ArchiveStorage(ctx, organization.ID, ref)
		if err != nil {
			return result, err
		}
		if err := s.Audit(ctx, "tenant", organization.ID, "archive_read"); err != nil {
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

func (d Details) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if organization != d.allocation.ID || len(content) > attachment.MaxArchiveBytes {
		return attachment.ArchiveReference{}, attachment.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	unlock := d.service.Lock(organization, false)
	defer unlock()
	storage, err := d.current(ctx)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, err := (attachment.ArchiveContents{Storage: storage}).PublishContent(ctx, organization, content)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	return d.confirmPublication(ctx, ref)
}

func (d Details) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if organization != d.allocation.ID || ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	unlock := d.service.Lock(organization, false)
	defer unlock()
	storage, err := d.current(ctx)
	if err != nil {
		return nil, err
	}
	return (attachment.ArchiveContents{Storage: storage}).ReadContent(ctx, organization, ref)
}
