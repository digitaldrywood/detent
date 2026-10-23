package hubserver

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type conversationContentFixture struct {
	mu      sync.Mutex
	content map[string][]byte
	fail    bool
}

func (s *conversationContentFixture) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return attachment.ArchiveReference{}, errors.New("object store unavailable")
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	if s.content == nil {
		s.content = map[string][]byte{}
	}
	s.content[ref.Key] = bytes.Clone(content)
	return ref, nil
}

func (s *conversationContentFixture) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	content, ok := s.content[ref.Key]
	if !ok {
		return nil, errors.New("object missing")
	}
	return bytes.Clone(content), nil
}

func (s *conversationContentFixture) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}
