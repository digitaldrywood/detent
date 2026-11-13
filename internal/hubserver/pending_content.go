package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type runEventDetailStore struct{ attachment.DetailStore }

func (store runEventDetailStore) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	expected, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	actual, writeErr := store.DetailStore.StoreDetail(ctx, organization, raw)
	if writeErr == nil {
		if actual != expected {
			return attachment.ArchiveReference{}, artifact.ErrIntegrity
		}
		return actual, nil
	}
	published, readErr := store.DetailStore.ReadDetail(ctx, organization, expected)
	if readErr != nil {
		return attachment.ArchiveReference{}, immutableContentStorageError(fmt.Errorf("reconcile run content upload: %w", errors.Join(writeErr, readErr)))
	}
	if !bytes.Equal(published, raw) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	return expected, nil
}

func (store runEventDetailStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	raw, err := store.DetailStore.ReadDetail(ctx, organization, ref)
	if err != nil {
		return nil, immutableContentStorageError(err)
	}
	return raw, nil
}

func (store runEventDetailStore) restore(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	expected, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	published, readErr := store.DetailStore.ReadDetail(ctx, organization, expected)
	if readErr == nil {
		if !bytes.Equal(published, raw) {
			return attachment.ArchiveReference{}, artifact.ErrIntegrity
		}
		return expected, nil
	}
	if !errors.Is(readErr, artifact.ErrMissing) && !errors.Is(readErr, fs.ErrNotExist) {
		return attachment.ArchiveReference{}, readErr
	}
	return store.StoreDetail(ctx, organization, raw)
}
