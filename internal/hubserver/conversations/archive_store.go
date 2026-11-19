package conversations

import (
	"bytes"
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
)

type ArchiveStore struct {
	Call entryauthority.ContentHandler
}

func (s ArchiveStore) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
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

func (s ArchiveStore) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	content := &hubcontent.ChangeContent{Call: s.Call, ReadOrganization: organization}
	if prior, ok := ctx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent); ok && prior.ReadOrganization != "" {
		if prior.ReadOrganization != organization {
			return nil, artifact.ErrIntegrity
		}
		content.ReadOrganization, content.ReadReference = prior.ReadOrganization, prior.ReadReference
	}
	ctx = context.WithValue(ctx, hubcontent.ChangeContentKey{}, content)
	return hubcontent.ReadChangeContent(ctx, organization, ref)
}
