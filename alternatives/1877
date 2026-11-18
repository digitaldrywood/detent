package detailbatch

import (
	"bytes"
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type detailBatchContextKey struct{}
type detailBatchEntry struct {
	ready chan struct{}
	data  []byte
	err   error
}
type requestDetailStore struct {
	owner    attachment.DetailStore
	mu       sync.Mutex
	entries  map[attachment.ArchiveReference]*detailBatchEntry
	reserved int64
}

type Deps struct {
	Store func() attachment.DetailStore
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) WithRequest(ctx context.Context) context.Context {
	if ctx.Value(detailBatchContextKey{}) != nil || s.deps.Store() == nil {
		return ctx
	}
	return WithStore(ctx, s.deps.Store())
}
func (s *Service) StoreFor(ctx context.Context) attachment.DetailStore {
	if store, ok := ctx.Value(detailBatchContextKey{}).(*requestDetailStore); ok {
		return store
	}
	return s.deps.Store()
}
func (s *Service) Contextual() attachment.DetailStore {
	if s.deps.Store() == nil {
		return nil
	}
	return contextualStore{service: s}
}

type contextualStore struct{ service *Service }

func (c contextualStore) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	return c.service.StoreFor(ctx).StoreDetail(ctx, organization, data)
}
func (c contextualStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return c.service.StoreFor(ctx).ReadDetail(ctx, organization, ref)
}
func (s *Service) Prefetch(ctx context.Context, organization string, refs []attachment.ArchiveReference) {
	if store, ok := ctx.Value(detailBatchContextKey{}).(*requestDetailStore); ok {
		store.prefetch(ctx, organization, refs)
	}
}
func (s *requestDetailStore) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	return s.owner.StoreDetail(ctx, organization, data)
}
func (s *requestDetailStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	s.mu.Lock()
	entry := s.entries[ref]
	if entry == nil {
		if ref.UncompressedSize > attachment.MaxArchiveBytes-s.reserved {
			s.mu.Unlock()
			return s.owner.ReadDetail(ctx, organization, ref)
		}
		s.reserved += ref.UncompressedSize
		entry = &detailBatchEntry{ready: make(chan struct{})}
		s.entries[ref] = entry
		s.mu.Unlock()
		entry.data, entry.err = s.owner.ReadDetail(ctx, organization, ref)
		close(entry.ready)
		return bytes.Clone(entry.data), entry.err
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.ready:
	}
	return bytes.Clone(entry.data), entry.err
}
func (s *requestDetailStore) prefetch(ctx context.Context, organization string, refs []attachment.ArchiveReference) {
	pending := []attachment.ArchiveReference{}
	s.mu.Lock()
	for _, ref := range refs {
		if ref.Validate(organization) != nil || s.entries[ref] != nil || ref.UncompressedSize > attachment.MaxArchiveBytes-s.reserved {
			continue
		}
		s.reserved += ref.UncompressedSize
		s.entries[ref] = &detailBatchEntry{ready: make(chan struct{})}
		pending = append(pending, ref)
	}
	s.mu.Unlock()
	for len(pending) > 0 {
		count := 0
		var compressed, content int64
		for count < len(pending) && count < attachment.MaxDetailBatchReferences {
			ref := pending[count]
			if compressed+ref.CompressedSize > attachment.MaxArchiveBytes || content+ref.UncompressedSize > attachment.MaxArchiveBytes {
				break
			}
			compressed += ref.CompressedSize
			content += ref.UncompressedSize
			count++
		}
		batch := pending[:count]
		pending = pending[count:]
		var rows []attachment.DetailReadResult
		var err error
		if owner, ok := s.owner.(attachment.DetailBatchStore); ok {
			rows, err = owner.ReadDetails(ctx, organization, batch)
		} else {
			rows, err = attachment.ReadDetailBatch(ctx, organization, batch, s.owner.ReadDetail)
		}
		if err == nil && len(rows) != len(batch) {
			err = artifact.ErrIntegrity
		}
		for index, ref := range batch {
			s.mu.Lock()
			entry := s.entries[ref]
			s.mu.Unlock()
			entry.err = err
			if err == nil {
				if rows[index].Reference != ref {
					entry.err = artifact.ErrIntegrity
				} else {
					entry.data, entry.err = rows[index].Bytes()
				}
			}
			close(entry.ready)
		}
	}
}

func Clear(ctx context.Context) context.Context {
	return context.WithValue(ctx, detailBatchContextKey{}, nil)
}
func WithStore(ctx context.Context, owner attachment.DetailStore) context.Context {
	return context.WithValue(ctx, detailBatchContextKey{}, &requestDetailStore{owner: owner, entries: map[attachment.ArchiveReference]*detailBatchEntry{}})
}
