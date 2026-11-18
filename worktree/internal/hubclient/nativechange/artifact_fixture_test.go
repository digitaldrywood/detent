package nativechange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
)

type changeEvidenceUpload struct {
	upload    artifact.Upload
	manifest  artifact.Manifest
	reference artifact.Reference
	body      []byte
	objects   map[string][]byte
}

type changeEvidenceService struct {
	mu      sync.Mutex
	remote  *artifact.RemoteHub
	uploads map[string]*changeEvidenceUpload
}

func (s *changeEvidenceService) PublishPending(ctx context.Context, publisher *artifact.RemoteHub) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, upload := range s.uploads {
		if upload.reference.ArtifactID != "" {
			if err := publisher.Publish(ctx, upload.reference); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *changeEvidenceService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	fail := func(err error) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": err.Error()})
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/uploads" {
		var reservation artifact.Reservation
		if err := artifact.Decode(r.Body, &reservation, artifact.MaxManifestBytes); err != nil {
			fail(err)
			return
		}
		if err := s.remote.Upload(r.Context(), token, reservation); err != nil {
			fail(err)
			return
		}
		for _, prior := range s.uploads {
			if prior.upload.Key == reservation.Key && prior.upload.Scope == reservation.Scope {
				_ = json.NewEncoder(w).Encode(prior.upload)
				return
			}
		}
		now := time.Now().UTC()
		upload := artifact.Upload{ArtifactID: artifact.NewID("artifact"), Reservation: reservation, State: "uploading", PartLimit: artifact.MaxChunkBytes, CreatedAt: now, Deadline: now.Add(time.Hour), ExpiresAt: now.Add(time.Hour)}
		s.uploads[upload.ArtifactID] = &changeEvidenceUpload{upload: upload, objects: map[string][]byte{}}
		_ = json.NewEncoder(w).Encode(upload)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	upload := s.uploads[parts[2]]
	if upload == nil {
		http.NotFound(w, r)
		return
	}
	if parts[1] == "uploads" {
		if err := s.remote.Upload(r.Context(), token, upload.upload.Reservation); err != nil {
			fail(err)
			return
		}
		if r.Method == http.MethodPut && parts[3] == "parts" {
			var part artifact.Part
			if err := artifact.Decode(r.Body, &part, 2*artifact.MaxChunkBytes+artifact.MaxManifestBytes); err != nil {
				fail(err)
				return
			}
			if artifact.Digest(part.Data) != part.SHA256 {
				fail(artifact.ErrIntegrity)
				return
			}
			object := artifact.Object{ID: artifact.NewID("object"), MediaType: part.MediaType, Size: int64(len(part.Data)), SHA256: part.SHA256, Path: part.Path, Side: part.Side, Sequence: part.Sequence, Offset: upload.manifest.TotalBytes}
			upload.objects[object.ID] = part.Data
			upload.manifest.Objects = append(upload.manifest.Objects, object)
			upload.manifest.TotalBytes += object.Size
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		if r.Method == http.MethodPost && parts[3] == "finalize" {
			var request struct {
				State   string            `json:"state"`
				Capture *artifact.Capture `json:"capture,omitempty"`
			}
			if err := artifact.Decode(r.Body, &request, artifact.MaxManifestBytes); err != nil {
				fail(err)
				return
			}
			manifest := &upload.manifest
			manifest.SchemaVersion, manifest.Scope = 1, upload.upload.Scope
			manifest.ArtifactID, manifest.ManifestID, manifest.Revision = upload.upload.ArtifactID, artifact.NewID("manifest"), 1
			manifest.Kind, manifest.State, manifest.Capture = upload.upload.Kind, request.State, request.Capture
			manifest.CreatedAt, manifest.ExpiresAt, manifest.RetentionPolicyID = upload.upload.CreatedAt, upload.upload.ExpiresAt, "test"
			if err := manifest.Validate(); err != nil {
				fail(err)
				return
			}
			upload.body, _ = json.Marshal(manifest)
			upload.reference = artifact.Reference{SchemaVersion: 1, Scope: manifest.Scope, ServiceID: upload.upload.ServiceID, ArtifactID: manifest.ArtifactID, ManifestID: manifest.ManifestID, Revision: manifest.Revision, SHA256: artifact.Digest(upload.body), Kind: manifest.Kind, State: manifest.State, Availability: "available", Bytes: manifest.TotalBytes, Objects: len(manifest.Objects), ExpiresAt: manifest.ExpiresAt, ObservedAt: time.Now().UTC()}
			_ = json.NewEncoder(w).Encode(upload.reference)
			return
		}
	}
	if r.Method == http.MethodGet && parts[1] == "artifacts" && len(parts) >= 5 {
		revision, err := strconv.ParseInt(parts[4], 10, 64)
		if err != nil || revision != upload.reference.Revision {
			fail(artifact.ErrMissing)
			return
		}
		if err := s.remote.Read(r.Context(), artifact.ReadAuthorization{ProjectID: upload.upload.ProjectID, Token: token, ArtifactID: upload.upload.ArtifactID, Revision: revision}); err != nil {
			fail(err)
			return
		}
		body := upload.body
		if len(parts) == 7 {
			body = upload.objects[parts[6]]
		}
		w.Header().Set("X-Artifact-SHA256", artifact.Digest(body))
		_, _ = w.Write(body)
		return
	}
	http.NotFound(w, r)
}

func changeEvidenceArtifactService(t *testing.T, h *nativeChangeHub) (*changeEvidenceService, *artifact.RemoteHub) {
	t.Helper()
	publisherID, publisherToken := testsupport.NativeChangePublisher(t, h.config, h.project)
	serviceID := artifact.NewID("service")
	remote := &artifact.RemoteHub{Client: fixtureClient(h.admin.Transport()).httpClient, Origin: fixtureClient(h.admin.Transport()).baseURL.String(), ServiceID: serviceID, OrganizationID: string(h.organization), ProjectID: string(h.project), PublisherToken: func() string { return publisherToken }}
	store := &changeEvidenceService{remote: remote, uploads: map[string]*changeEvidenceUpload{}}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	binding := artifact.Binding{ServiceID: serviceID, Origin: server.URL, Mode: "customer", PublisherTokenID: publisherID}
	if err := h.admin.Transport().Request(t.Context(), http.MethodPut, h.admin.Scope()+"/onboarding/artifact-services/"+binding.ServiceID, binding, nil); err != nil {
		t.Fatal(err)
	}
	fixtureArtifactConfiguration(t, h.scheduler, binding.ServiceID, 4<<20)
	return store, remote
}
