package hubserver

import (
	"bytes"
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

type conversationArchiveStore struct {
	call entryauthority.ContentHandler
}

func (s conversationArchiveStore) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	if stored, err := s.ReadContent(ctx, organization, ref); err == nil && bytes.Equal(stored, content) {
		return ref, nil
	}
	if _, err := s.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Data: compressed, Write: true}); err != nil {
		return ref, err
	}
	stored, err := s.ReadContent(ctx, organization, ref)
	if err != nil {
		return ref, err
	}
	if !bytes.Equal(stored, content) {
		return ref, artifact.ErrIntegrity
	}
	return ref, nil
}

func (s conversationArchiveStore) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	ctx = context.WithValue(ctx, changeContentKey{}, &changeContent{call: s.call})
	return readChangeContent(ctx, organization, ref)
}
