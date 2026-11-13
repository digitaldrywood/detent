package hubserver

import (
	"bytes"
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

type conversationArchiveStore struct {
	Call entryauthority.ContentHandler
}

func (s conversationArchiveStore) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	if stored, err := s.ReadContent(ctx, organization, ref); err == nil && bytes.Equal(stored, content) {
		return ref, nil
	}
	if _, err := s.Call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Data: compressed, Write: true}); err != nil {
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
	content := &changeContent{call: s.Call, readOrganization: organization}
	if prior, ok := ctx.Value(changeContentKey{}).(*changeContent); ok && prior.readOrganization != "" {
		if prior.readOrganization != organization {
			return nil, artifact.ErrIntegrity
		}
		content.readOrganization, content.readReference = prior.readOrganization, prior.readReference
	}
	ctx = context.WithValue(ctx, changeContentKey{}, content)
	return readChangeContent(ctx, organization, ref)
}
