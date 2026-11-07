package api

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type repodiscDetails struct {
	mu      sync.Mutex
	objects map[string]repodiscObject
}

type repodiscObject struct {
	reference attachment.ArchiveReference
	data      []byte
}

func (s *repodiscDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil || ctx.Err() != nil {
		return ref, errors.Join(err, ctx.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = make(map[string]repodiscObject)
	}
	s.objects[ref.Key] = repodiscObject{reference: ref, data: bytes.Clone(raw)}
	return ref, nil
}

func (s *repodiscDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[ref.Key]
	if !ok || object.reference != ref {
		return nil, attachment.ErrInvalid
	}
	return bytes.Clone(object.data), ctx.Err()
}

func (s *repodiscDetails) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if err := attachment.ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	if !ok {
		return nil, attachment.ErrInvalid
	}
	return bytes.Clone(object.data), ctx.Err()
}
