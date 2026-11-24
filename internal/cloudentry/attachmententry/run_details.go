package attachmententry

import (
	"bytes"
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
)

type Details struct {
	service    *Service
	allocation Organization
}

func (d Details) current(ctx context.Context) (attachment.ArchiveStorage, error) {
	current, err := d.service.Lookup(ctx, d.allocation.ID)
	if err != nil || current.Generation != d.allocation.Generation || current.Endpoint != d.allocation.Endpoint || current.ProviderID != d.allocation.ProviderID {
		return nil, attachment.ErrInvalid
	}
	provisioning := d.allocation.State == "allocating" && current.Managed && current.State == "allocating" && (current.Step == "tenant_files" || current.Step == "tenant_start" || current.Step == "owner_bootstrap")
	if current.State != "ready" && !provisioning {
		return nil, attachment.ErrInvalid
	}
	storage, ok := d.service.Storage().(attachment.ArchiveStorage)
	if !ok {
		return nil, attachment.ErrInvalid
	}
	return storage, nil
}

func (d Details) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	if len(data) == 0 || len(data) > attachment.MaxArchiveBytes || organization != d.allocation.ID {
		return attachment.ArchiveReference{}, attachment.ErrInvalid
	}
	unlock := d.service.Lock(organization, false)
	defer unlock()
	storage, err := d.current(ctx)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if err := storage.PutArchive(ctx, ref, compressed); err != nil {
		return attachment.ArchiveReference{}, err
	}
	if err := attachment.StoreArchive(ctx, storage, organization, ref, compressed); err != nil {
		return attachment.ArchiveReference{}, err
	}
	return d.confirmPublication(ctx, ref)
}

func (d Details) confirmPublication(ctx context.Context, ref attachment.ArchiveReference) (attachment.ArchiveReference, error) {
	if _, err := d.current(ctx); err != nil {
		return attachment.ArchiveReference{}, err
	}
	return ref, nil
}

func (d Details) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	if organization != d.allocation.ID {
		return nil, attachment.ErrInvalid
	}
	unlock := d.service.Lock(organization, false)
	storage, err := d.current(ctx)
	unlock()
	if err != nil {
		return nil, err
	}
	return attachment.ReadArchive(ctx, storage, organization, ref)
}

func (d Details) ReadDetails(ctx context.Context, organization string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	if organization != d.allocation.ID || attachment.ValidateDetailBatch(organization, refs) != nil {
		return nil, attachment.ErrInvalid
	}
	unlock := d.service.Lock(organization, false)
	storage, err := d.current(ctx)
	unlock()
	if err != nil {
		return nil, err
	}
	return attachment.ReadDetailBatch(ctx, organization, refs, func(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
		return attachment.ReadArchive(ctx, storage, organization, ref)
	})
}

func (d Details) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tenant.CallTimeout)
	defer cancel()
	if organization != d.allocation.ID || attachment.ValidateCanonicalDetailKey(organization, project, key) != nil {
		return nil, attachment.ErrInvalid
	}
	unlock := d.service.Lock(organization, false)
	defer unlock()
	storage, err := d.current(ctx)
	if err != nil {
		return nil, err
	}
	return attachment.ReadCanonicalArchive(ctx, storage, organization, project, key)
}

func (s *Service) Details(org Organization) Details {
	return Details{service: s, allocation: org}
}
