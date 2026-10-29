package cloudentry

import (
	"bytes"
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type tenantDetailStore struct {
	service    *Service
	allocation Organization
}

func (d tenantDetailStore) current(ctx context.Context) (attachment.ArchiveStorage, error) {
	current, err := d.service.readyOrganization(ctx, d.allocation.ID)
	if err != nil || current.Generation != d.allocation.Generation || current.Endpoint != d.allocation.Endpoint {
		return nil, attachment.ErrInvalid
	}
	storage, ok := d.service.attachments.(attachment.ArchiveStorage)
	if !ok {
		return nil, attachment.ErrInvalid
	}
	return storage, nil
}

func (d tenantDetailStore) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if organization != d.allocation.ID {
		return attachment.ArchiveReference{}, attachment.ErrInvalid
	}
	unlock := d.service.attachmentLocks.lock(organization, false)
	defer unlock()
	storage, err := d.current(ctx)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	exists, err := d.service.attachmentObjectExists(ctx, d.allocation, "/internal/v1/attachments/exists", map[string]string{"key": ref.Key})
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if !exists {
		if err := storage.PutArchive(ctx, ref, compressed); err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
	if err := attachment.StoreArchive(ctx, storage, organization, ref, compressed); err != nil {
		return attachment.ArchiveReference{}, err
	}
	return ref, nil
}

func (d tenantDetailStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if organization != d.allocation.ID {
		return nil, attachment.ErrInvalid
	}
	unlock := d.service.attachmentLocks.lock(organization, false)
	defer unlock()
	storage, err := d.current(ctx)
	if err != nil {
		return nil, err
	}
	return attachment.ReadArchive(ctx, storage, organization, ref)
}
