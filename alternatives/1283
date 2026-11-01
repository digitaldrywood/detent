package nativechange

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

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

func changeEvidenceArtifactService(t *testing.T, h *nativeChangeHub) (*artifact.Service, *artifact.RemoteHub) {
	t.Helper()
	var publisher struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := h.admin.Transport().Request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "artifacts", "scope": "worker"}, &publisher); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Transport().Request(t.Context(), http.MethodPost, "/api/v2/tokens/"+publisher.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	serviceID := artifact.NewID("service")
	remote := &artifact.RemoteHub{Client: fixtureClient(h.admin.Transport()).httpClient, Origin: fixtureClient(h.admin.Transport()).baseURL.String(), ServiceID: serviceID, OrganizationID: string(h.organization), ProjectID: string(h.project), PublisherToken: func() string { return publisher.Token }}
	store, err := artifact.NewService(t.Context(), artifact.Config{ServiceID: serviceID, OrganizationID: string(h.organization), Mode: "customer", DatabasePath: filepath.Join(t.TempDir(), "artifacts.db"), Policy: artifact.Policy{ID: "test", Limits: artifact.Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: artifact.MaxChunkBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 600, DeletionRecordSeconds: 7200, BackupSeconds: 3600}}, &changeEvidenceStorage{objects: map[string][]byte{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	handler, err := artifact.NewHTTPServer(store, remote)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	binding := artifact.Binding{ServiceID: serviceID, Origin: server.URL, Mode: "customer", PublisherTokenID: publisher.ID}
	if err := h.admin.Transport().Request(t.Context(), http.MethodPut, h.admin.Scope()+"/artifact-services/"+binding.ServiceID, binding, nil); err != nil {
		t.Fatal(err)
	}
	fixtureArtifactConfiguration(t, h.scheduler, binding.ServiceID, 4<<20)
	return store, remote
}
