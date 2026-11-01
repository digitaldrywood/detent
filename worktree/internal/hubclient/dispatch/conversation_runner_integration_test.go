package dispatch

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type conversationArchiveFixture struct {
	mu      sync.Mutex
	content map[string][]byte
}

func (f *conversationArchiveFixture) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.content == nil {
		f.content = map[string][]byte{}
	}
	f.content[ref.Key] = bytes.Clone(content)
	return ref, nil
}

func (f *conversationArchiveFixture) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.content[ref.Key]
	if !ok {
		return nil, errors.New("archive content missing")
	}
	return bytes.Clone(content), nil
}
