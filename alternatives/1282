package evidence

import (
	"bytes"
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type changeEvidenceStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *changeEvidenceStorage) Put(_ context.Context, key string, data []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; ok {
		return "", artifact.ErrConflict
	}
	s.objects[key] = bytes.Clone(data)
	return "version", nil
}

func (s *changeEvidenceStorage) Get(_ context.Context, key, _ string, limit int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	if int64(len(data)) > limit {
		return nil, artifact.ErrIntegrity
	}
	return bytes.Clone(data), nil
}

func (s *changeEvidenceStorage) Delete(_ context.Context, key, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}
