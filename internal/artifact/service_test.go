package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStorage struct {
	mu        sync.Mutex
	data      map[string][]byte
	failure   error
	lostReply bool
}

func (m *memoryStorage) Put(_ context.Context, key string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return "", errors.Join(m.failure, ErrWriteRejected)
	}
	if _, ok := m.data[key]; ok {
		return "version", ErrConflict
	}
	m.data[key] = bytes.Clone(data)
	if m.lostReply {
		m.lostReply = false
		return "", ErrStorage
	}
	return "version", nil
}

func (m *memoryStorage) Get(_ context.Context, key, _ string, limit int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return nil, m.failure
	}
	data, ok := m.data[key]
	if !ok {
		return nil, ErrMissing
	}
	if int64(len(data)) > limit {
		return nil, ErrIntegrity
	}
	return bytes.Clone(data), nil
}

func (m *memoryStorage) Delete(_ context.Context, key, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return m.failure
	}
	delete(m.data, key)
	return nil
}

type testAllowances struct{ limits Limits }

func (a *testAllowances) Limits(context.Context, string) (Limits, error) {
	limits := a.limits
	limits.RelayBytes = 1 << 40
	limits.WindowSeconds = 3600
	limits.TelemetrySeconds = 86400
	return limits, nil
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{ServiceID: NewID("service"), OrganizationID: NewID("org"), Mode: "customer", ProjectID: NewID("prj"), Policy: Policy{ID: "test", Limits: Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: MaxChunkBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 600, DeletionRecordSeconds: 7200, BackupSeconds: 3600}}
	cfg.Catalog = newMemoryCatalog(cfg)
	return cfg
}

func testService(t *testing.T) (*Service, *memoryStorage) {
	t.Helper()
	storage := &memoryStorage{data: map[string][]byte{}}
	s, err := newTestService(t.Context(), testConfig(t), storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, storage
}

func testReservation(s *Service) Reservation {
	return Reservation{ServiceID: s.config.ServiceID, Mode: s.config.Mode, HostedOptIn: s.config.HostedOptIn, Scope: Scope{OrganizationID: s.config.OrganizationID, ProjectID: s.config.ProjectID, WorkItemID: NewID("wi"), RunID: NewID("run"), AttemptID: NewID("attempt")}, Key: "log", Kind: "log", Bytes: 4 << 20, LeaseID: "lease", FencingToken: 1}
}

func testPart(sequence int, data string) Part {
	return Part{Sequence: sequence, MediaType: "text/plain; charset=utf-8", SHA256: Digest([]byte(data)), Data: []byte(data)}
}

func TestUploadRecoveryAndImmutableManifests(t *testing.T) {
	t.Parallel()
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost_reply_%t", lost), func(t *testing.T) {
			s, storage := testService(t)
			r := testReservation(s)
			u, err := s.Reserve(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := s.Reserve(t.Context(), r)
			if err != nil || retry.ArtifactID != u.ArtifactID {
				t.Fatalf("reserve retry: %v %#v", err, retry)
			}
			changed := r
			changed.Bytes--
			if _, err := s.Reserve(t.Context(), changed); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed reserve: %v", err)
			}
			storage.lostReply = lost
			part := testPart(0, "<script>customer log</script>\n")
			if lost {
				if _, err := s.Append(t.Context(), u.ArtifactID, part); !errors.Is(err, ErrStorage) {
					t.Fatal(err)
				}
			}
			obj, err := s.Append(t.Context(), u.ArtifactID, part)
			if err != nil {
				t.Fatal(err)
			}
			usage, err := s.Usage(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(t.Context(), u.ArtifactID, part); err != nil {
				t.Fatal(err)
			}
			again, err := s.Usage(t.Context())
			if err != nil || again != usage {
				t.Fatalf("duplicate charged: %v %v %v", usage, again, err)
			}
			partial, pref, err := s.Manifest(t.Context(), u.ArtifactID, 1)
			if err != nil || pref.State != "partial" {
				t.Fatalf("partial: %v %v", pref, err)
			}
			ref, err := s.Finalize(t.Context(), u.ArtifactID, "complete", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(t.Context(), u.ArtifactID, part); err != nil {
				t.Fatalf("retry completed part: %v", err)
			}
			if _, err := s.Append(t.Context(), u.ArtifactID, testPart(1, "later")); !errors.Is(err, ErrConflict) {
				t.Fatalf("append completed: %v", err)
			}
			final, err := s.Finalize(t.Context(), u.ArtifactID, "complete", nil)
			if err != nil || final != ref {
				t.Fatalf("finalize retry: %v", err)
			}
			old, _, err := s.Manifest(t.Context(), u.ArtifactID, 1)
			if err != nil || !bytes.Equal(old, partial) {
				t.Fatal("partial mutated", err)
			}
			_, data, err := s.ReadObject(t.Context(), u.ArtifactID, ref.Revision, obj.ID)
			if err != nil || !bytes.Equal(data, part.Data) {
				t.Fatal("read", err)
			}
			stored, err := s.command(t.Context(), CatalogCommand{Operation: "manifest", UploadID: u.ArtifactID, Revision: 1})
			if err != nil {
				t.Fatal(err)
			}
			changedManifest := stored.Manifest
			changedManifest.SHA256 = Digest([]byte("changed"))
			if _, err := s.command(t.Context(), CatalogCommand{Operation: "commit", UploadID: u.ArtifactID, Manifest: changedManifest}); !errors.Is(err, ErrConflict) {
				t.Fatal("manifest update allowed", err)
			}

			usage, err = s.Usage(t.Context())
			if err != nil || usage.ReservedBytes != 0 || usage.RetainedBytes <= int64(len(part.Data)) {
				t.Fatal("accounting", usage, err)
			}
			cfg := s.config
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := newTestService(t.Context(), cfg, storage, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			_, data, err = reopened.ReadObject(t.Context(), u.ArtifactID, ref.Revision, obj.ID)
			if err != nil || !bytes.Equal(data, part.Data) {
				t.Fatal("restart", err)
			}
			publisher := &testPublisher{failure: ErrStorage}
			if err := reopened.PublishPending(t.Context(), publisher); !errors.Is(err, ErrStorage) {
				t.Fatal(err)
			}
			publisher.failure, publisher.rejectFirst = nil, true
			if err := reopened.PublishPending(t.Context(), publisher); !errors.Is(err, ErrDenied) || len(publisher.refs) != 1 {
				t.Fatalf("rejected receipt blocked later receipts: refs=%d error=%v", len(publisher.refs), err)
			}
			if err := reopened.PublishPending(t.Context(), publisher); err != nil {
				t.Fatal(err)
			}
			if len(publisher.refs) != 2 {
				t.Fatalf("outbox: %d", len(publisher.refs))
			}
			if err := reopened.PublishPending(t.Context(), publisher); err != nil || len(publisher.refs) != 2 {
				t.Fatal("outbox duplicate", err)
			}
		})
	}
}

func TestPublishPendingProgressesPastRejectedReceipts(t *testing.T) {
	t.Parallel()
	s, _ := testService(t)
	start := time.Now().UTC().Truncate(time.Second)
	var valid string
	for i := range 65 {
		s.now = func() time.Time { return start.Add(time.Duration(i) * time.Second) }
		r := testReservation(s)
		r.Key, r.Bytes = fmt.Sprintf("receipt-%d", i), 128<<10
		u, err := s.Reserve(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(t.Context(), u.ArtifactID, testPart(0, "log")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finalize(t.Context(), u.ArtifactID, "complete", nil); err != nil {
			t.Fatal(err)
		}
		valid = u.ArtifactID
	}
	publisher := &rejectingPublisher{accept: valid}
	for range 8 {
		if err := s.PublishPending(t.Context(), publisher); err != nil && !errors.Is(err, ErrDenied) {
			t.Fatalf("publish error = %v, want denied", err)
		}
		if publisher.published {
			return
		}
	}
	t.Fatalf("valid receipt was not published after %d rejected attempts", publisher.attempts)
}

type rejectingPublisher struct {
	accept    string
	attempts  int
	published bool
}

func (p *rejectingPublisher) Publish(_ context.Context, ref Reference) error {
	p.attempts++
	if ref.ArtifactID != p.accept {
		return ErrDenied
	}
	p.published = true
	return nil
}

type testPublisher struct {
	failure     error
	rejectFirst bool
	refs        []Reference
}

func (p *testPublisher) Publish(_ context.Context, ref Reference) error {
	if p.failure != nil {
		return p.failure
	}
	if p.rejectFirst {
		p.rejectFirst = false
		return ErrDenied
	}
	p.refs = append(p.refs, ref)
	return nil
}

func TestArtifactFailureStates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*Service, *memoryStorage, Upload, Object)
		want   error
	}{
		{"missing", func(s *Service, m *memoryStorage, u Upload, o Object) {
			delete(m.data, s.objectKey(u.ArtifactID, o.ID))
		}, ErrMissing},
		{"corrupt", func(s *Service, m *memoryStorage, u Upload, o Object) {
			m.data[s.objectKey(u.ArtifactID, o.ID)] = []byte("bad")
		}, ErrIntegrity},
		{"outage", func(s *Service, m *memoryStorage, _ Upload, _ Object) { m.failure = ErrStorage }, ErrStorage},
		{"expired", func(s *Service, _ *memoryStorage, u Upload, _ Object) {
			s.now = func() time.Time { return u.ExpiresAt }
		}, ErrExpired},
		{"deleted", func(s *Service, _ *memoryStorage, u Upload, _ Object) {
			if err := s.Delete(t.Context(), u.ArtifactID); err != nil {
				t.Error(err)
			}
		}, ErrMissing},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, m := testService(t)
			u, err := s.Reserve(t.Context(), testReservation(s))
			if err != nil {
				t.Fatal(err)
			}
			o, err := s.Append(t.Context(), u.ArtifactID, testPart(0, "log"))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(s, m, u, o)
			_, _, err = s.ReadObject(t.Context(), u.ArtifactID, 1, o.ID)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
		})
	}
}

func TestHostedConcurrentReservationsAndDowngrade(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Mode = "hosted"
	cfg.HostedOptIn = true
	allow := &testAllowances{cfg.Policy.Limits}
	allow.limits.RetainedBytes = 4 << 20
	allow.limits.ReservedBytes = 4 << 20
	s, err := newTestService(t.Context(), cfg, &memoryStorage{data: map[string][]byte{}}, allow)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := testReservation(s)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() { r := r; r.Key = strconv.Itoa(i); _, err := s.Reserve(t.Context(), r); results <- err })
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners %d", winners)
	}
	allow.limits.RetainedBytes = 1
	r.Key = "downgrade"
	if _, err := s.Reserve(t.Context(), r); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	usage, err := s.Usage(t.Context())
	if err != nil || usage.ReservedBytes != 4<<20 {
		t.Fatal("downgrade lost reservation", usage, err)
	}
}

func TestAbandonedUploadsAndDeletionRecovery(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"log", "diff"} {
		t.Run(kind, func(t *testing.T) {
			s, m := testService(t)
			r := testReservation(s)
			r.Kind = kind
			u, err := s.Reserve(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(t.Context(), u.ArtifactID, testPart(0, "log")); err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return u.Deadline }
			if err := s.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			updated, err := s.Upload(t.Context(), u.ArtifactID)
			if err != nil {
				t.Fatal(err)
			}
			want := "deleted"
			if kind == "log" {
				want = "interrupted"
			}
			if updated.State != want {
				t.Fatalf("%s want %s", updated.State, want)
			}
			s.now = func() time.Time { return u.ExpiresAt }
			m.failure = ErrStorage
			if kind == "log" {
				if err := s.Maintain(t.Context()); !errors.Is(err, ErrStorage) {
					t.Fatal(err)
				}
			}
			m.failure = nil
			if err := s.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			usage, err := s.Usage(t.Context())
			if err != nil || usage != (Usage{}) {
				t.Fatal(usage, err)
			}
			s.now = func() time.Time {
				return u.ExpiresAt.Add(time.Duration(s.config.Policy.DeletionRecordSeconds+1) * time.Second)
			}
			if err := s.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Upload(t.Context(), u.ArtifactID); !errors.Is(err, ErrMissing) {
				t.Fatal(err)
			}
			m.mu.Lock()
			remaining := len(m.data)
			m.mu.Unlock()
			if remaining != 0 {
				t.Fatal("pruned upload left external receipt objects", remaining)
			}
		})
	}
}

func TestCatalogSharedHubAndExport(t *testing.T) {
	t.Parallel()
	s, storage := testService(t)
	other, err := newTestService(t.Context(), s.config, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	u, err := s.Reserve(t.Context(), testReservation(s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(t.Context(), u.ArtifactID, testPart(0, "export")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Manifest(t.Context(), u.ArtifactID, 1); err != nil {
		t.Fatal("second service cannot read Hub catalog", err)
	}
	path := filepath.Join(t.TempDir(), "catalog.jsonl")
	if err := s.Backup(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(t.Context(), path); err == nil {
		t.Fatal("existing export overwritten")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var exported CatalogResult
	if err := json.NewDecoder(file).Decode(&exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Uploads) != 1 || len(exported.Manifests) != 1 || exported.Manifests[0].ArtifactID != u.ArtifactID {
		t.Fatalf("export=%+v", exported)
	}
}

func TestCatalogRequiresHub(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", ":memory:", "file:catalog.db", filepath.Join(t.TempDir(), "catalog.db")} {
		cfg := testConfig(t)
		cfg.Catalog = nil
		cfg.DatabasePath = path
		if service, err := NewService(t.Context(), cfg, &memoryStorage{data: map[string][]byte{}}, nil); !errors.Is(err, ErrAuthorization) {
			if service != nil {
				service.Close()
			}
			t.Fatal("local catalog accepted", err)
		}
	}
}

func TestAbandonedUnverifiedObjectKeepsReservationUntilCleanup(t *testing.T) {
	t.Parallel()
	for _, outage := range []bool{false, true} {
		t.Run(strconv.FormatBool(outage), func(t *testing.T) {
			s, storage := testService(t)
			u, err := s.Reserve(t.Context(), testReservation(s))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(t.Context(), u.ArtifactID, testPart(0, "verified")); err != nil {
				t.Fatal(err)
			}
			storage.lostReply = true
			if _, err := s.Append(t.Context(), u.ArtifactID, testPart(1, "unverified")); !errors.Is(err, ErrStorage) {
				t.Fatal(err)
			}
			s.now = func() time.Time { return u.Deadline }
			if outage {
				storage.failure = ErrStorage
				if err := s.Maintain(t.Context()); !errors.Is(err, ErrStorage) {
					t.Fatal(err)
				}
				usage, err := s.Usage(t.Context())
				if err != nil || usage.ReservedBytes == 0 {
					t.Fatal("reservation released during outage", usage, err)
				}
				storage.failure = nil
			}
			if err := s.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, ref, err := s.Manifest(t.Context(), u.ArtifactID, 0)
			if err != nil || ref.State != "interrupted" || ref.Objects != 1 {
				t.Fatal(ref, err)
			}
			if _, ok := storage.data[u.ArtifactID+"/"+"unverified"]; ok {
				t.Fatal("unverified object retained")
			}
			for _, entry := range storage.data {
				if string(entry) == "unverified" {
					t.Fatal("unverified object retained")
				}
			}
			usage, err := s.Usage(t.Context())
			if err != nil || usage.ReservedBytes != 0 {
				t.Fatal(usage, err)
			}
		})
	}
}

func TestRestoredCatalogCannotResurrectDeletedArtifact(t *testing.T) {
	t.Parallel()
	s, storage := testService(t)
	r := testReservation(s)
	u, err := s.Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	p := testPart(0, "must stay deleted")
	if _, err := s.Append(t.Context(), u.ArtifactID, p); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "before-deletion.db")
	if err := s.Backup(t.Context(), backup); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), u.ArtifactID); err != nil {
		t.Fatal(err)
	}
	cfg := s.config
	cfg.DatabasePath = backup
	restored, err := newTestService(t.Context(), cfg, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"manifest", func() error { _, _, err := restored.Manifest(t.Context(), u.ArtifactID, 1); return err }},
		{"append", func() error { _, err := restored.Append(t.Context(), u.ArtifactID, p); return err }},
		{"finalize", func() error { _, err := restored.Finalize(t.Context(), u.ArtifactID, "complete", nil); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil {
				t.Fatal("deleted artifact resurrected")
			}
		})
	}
	if err := restored.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactManifestHeadroom(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{4096, 16384} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			s, _ := testService(t)
			r := testReservation(s)
			r.Kind, r.Bytes = "diff", size
			u, err := s.Reserve(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			p := testPart(0, "x")
			p.Path, p.Side = strings.Repeat("a", 4096), "head"
			_, err = s.Append(t.Context(), u.ArtifactID, p)
			if size == 4096 {
				if !errors.Is(err, ErrQuota) {
					t.Fatal("manifest headroom not reserved", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			if _, err := s.Finalize(t.Context(), u.ArtifactID, "complete", &Capture{Base: sha, Head: sha, MergeBase: sha, ContextLines: 3, FileContext: "changed_files"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReservationCustodyBinding(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"customer", "hosted"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Mode, cfg.HostedOptIn = mode, mode == "hosted"
			storage := &memoryStorage{data: map[string][]byte{}}
			s, err := newTestService(t.Context(), cfg, storage, &testAllowances{cfg.Policy.Limits})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, test := range []struct {
				name string
				edit func(*Reservation)
			}{
				{"different service", func(r *Reservation) { r.ServiceID = NewID("service") }},
				{"different mode", func(r *Reservation) { r.Mode = "local" }},
				{"different consent", func(r *Reservation) { r.HostedOptIn = !r.HostedOptIn }},
			} {
				t.Run(test.name, func(t *testing.T) {
					r := testReservation(s)
					test.edit(&r)
					if _, err := s.Reserve(t.Context(), r); !errors.Is(err, ErrDenied) {
						t.Fatal("mismatched custody accepted", err)
					}
				})
			}
			usage, err := s.Usage(t.Context())
			if err != nil || usage.RetainedBytes != 0 || usage.ReservedBytes != 0 || usage.RelayBytes != 0 || usage.StorageRequests != 0 || len(storage.data) != 0 {
				t.Fatal("mismatched custody created stored data", usage, err)
			}
			upload, err := s.Reserve(t.Context(), testReservation(s))
			if err != nil {
				t.Fatal(err)
			}
			s.config.ServiceID = NewID("service")
			if _, err := s.Upload(t.Context(), upload.ArtifactID); !errors.Is(err, ErrDenied) {
				t.Fatal("existing catalog silently changed service custody", err)
			}
		})
	}
}

func newTestService(ctx context.Context, cfg Config, storage Storage, allowances Allowances) (*Service, error) {
	if cfg.Catalog == nil {
		cfg.Catalog = newMemoryCatalog(cfg)
	}
	if catalog, ok := cfg.Catalog.(*memoryCatalog); ok {
		catalog.config = cfg
		if allow, ok := allowances.(*testAllowances); ok && cfg.Mode == "hosted" {
			catalog.limits = func() Limits { return allow.limits }
		}
	}
	return NewService(ctx, cfg, storage, allowances)
}

type rejectedManifestCatalog struct {
	Catalog
	rejected bool
}

func (c *rejectedManifestCatalog) Execute(ctx context.Context, command CatalogCommand) (CatalogResult, error) {
	if command.Operation == "commit" && command.Manifest.State == "complete" && !c.rejected {
		c.rejected = true
		return CatalogResult{}, ErrConflict
	}
	return c.Catalog.Execute(ctx, command)
}

func TestRejectedManifestCandidateDoesNotBlockInterruption(t *testing.T) {
	service, _ := testService(t)
	upload, err := service.Reserve(t.Context(), testReservation(service))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Append(t.Context(), upload.ArtifactID, testPart(0, "example")); err != nil {
		t.Fatal(err)
	}
	service.catalog = &rejectedManifestCatalog{Catalog: service.catalog}
	if _, err := service.Finalize(t.Context(), upload.ArtifactID, "complete", nil); !errors.Is(err, ErrConflict) {
		t.Fatal("expected definite commit rejection", err)
	}
	ref, err := service.Finalize(t.Context(), upload.ArtifactID, "interrupted", nil)
	if err != nil || ref.State != "interrupted" {
		t.Fatalf("rejected complete body permanently blocks interruption: state=%q error=%v", ref.State, err)
	}
}

func TestIntentReceiptCleanupResumesWithoutStarvingOtherUploads(t *testing.T) {
	service, storage := testService(t)
	catalog := service.catalog.(*memoryCatalog)
	ids := []string{NewID("artifact"), NewID("artifact")}
	deleted := service.now().Add(-time.Duration(service.config.Policy.DeletionRecordSeconds+1) * time.Second)
	for index, id := range ids {
		upload := Upload{ArtifactID: id, Reservation: testReservation(service), State: "deleted", DeletedAt: &deleted}
		catalog.uploads[id] = upload
		catalog.intents[id] = map[string]CatalogIntent{}
		count := 65
		if index == 1 {
			count = 1
		}
		for range count {
			intentID := NewID("intent")
			key := service.scopedObjectKey(upload.ProjectID, id, "discarded/"+intentID)
			catalog.intents[id][intentID] = CatalogIntent{ID: intentID, State: "discarded", WriteState: "rejected", Receipt: key}
			storage.data[key] = []byte("receipt")
		}
	}
	if err := service.Maintain(t.Context()); err != nil && !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if len(storage.data) < 34 || len(storage.data) >= 66 {
		t.Fatalf("one maintenance tick drained intent history: remaining receipts=%d, want 34..65", len(storage.data))
	}
	if _, exists := catalog.uploads[ids[1]]; exists {
		t.Fatal("long intent history starved another upload")
	}
	for range 4 {
		if err := service.Maintain(t.Context()); err != nil && !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if len(storage.data) != 0 || len(catalog.uploads) != 0 {
		t.Fatal("cleanup did not resume from Hub progress", len(storage.data), len(catalog.uploads))
	}
}

func TestRetiredIntentCannotReceiveALateCleanupReceipt(t *testing.T) {
	cleaner, storage := testService(t)
	upload, err := cleaner.Reserve(t.Context(), testReservation(cleaner))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := cleaner.command(t.Context(), CatalogCommand{Operation: "prepare_manifest", UploadID: upload.ArtifactID, Intent: CatalogIntent{ID: NewID("intent"), Revision: 1, ManifestState: "complete", SHA256: Digest([]byte("manifest")), Bytes: 100}})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &admittedPutBarrier{Storage: storage, match: func(key string) bool { return strings.Contains(key, "/discarded/intent_") }, entered: make(chan string, 1), resume: make(chan struct{})}
	cfg := cleaner.config
	cfg.Catalog = cleaner.catalog
	other, err := newTestService(t.Context(), cfg, barrier, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var discardErr error
	t.Cleanup(func() { barrier.unblock(); <-finished })
	go func() {
		discardErr = other.discardManifestIntent(t.Context(), upload, prepared.Intent)
		close(finished)
	}()
	select {
	case key := <-barrier.entered:
		if err := cleaner.discardManifestIntent(t.Context(), upload, prepared.Intent); err != nil {
			t.Fatal(err)
		}
		if _, err := cleaner.retireIntentBatch(t.Context(), upload, 32, false); err != nil {
			t.Fatal(err)
		}
		barrier.unblock()
		<-finished
		if _, err := storage.Get(t.Context(), key, "", 0); !errors.Is(err, ErrMissing) {
			t.Fatal("a retired intent received an orphan cleanup receipt", err)
		}
	case <-finished:
		if discardErr != nil {
			t.Fatal(discardErr)
		}
		if _, err := cleaner.retireIntentBatch(t.Context(), upload, 32, false); err != nil {
			t.Fatal(err)
		}
	}
}

type admittedPutBarrier struct {
	Storage
	match   func(string) bool
	entered chan string
	resume  chan struct{}
	once    sync.Once
	release sync.Once
}

type uncertainPutStorage struct {
	*memoryStorage
	manifest  bool
	committed bool
	key       string
	body      []byte
	calls     int
}

func (s *uncertainPutStorage) Put(ctx context.Context, key string, body []byte) (string, error) {
	if s.key == "" && strings.Contains(key, "/manifests/") == s.manifest {
		s.key, s.body = key, bytes.Clone(body)
	}
	if key != s.key {
		return s.memoryStorage.Put(ctx, key, body)
	}
	s.calls++
	if s.committed {
		if _, err := s.memoryStorage.Put(ctx, key, body); err != nil {
			return "", err
		}
	}
	return "", ErrStorage
}

func TestUncertainPutRetainsCustodyUntilItsSingleWriteIsObserved(t *testing.T) {
	for _, manifest := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("manifest=%t/committed=%t", manifest, committed), func(t *testing.T) {
				storage := &uncertainPutStorage{memoryStorage: &memoryStorage{data: map[string][]byte{}}, manifest: manifest, committed: committed}
				service, err := newTestService(t.Context(), testConfig(t), storage, nil)
				if err != nil {
					t.Fatal(err)
				}
				upload, err := service.Reserve(t.Context(), testReservation(service))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.Append(t.Context(), upload.ArtifactID, testPart(0, "example")); !errors.Is(err, ErrStorage) {
					t.Fatal("fixture did not lose the physical Put result", err)
				}
				if !committed {
					if err := service.Delete(t.Context(), upload.ArtifactID); !errors.Is(err, ErrConflict) {
						t.Fatal("absence settled an uncertain Put", err)
					}
					current, err := service.Upload(t.Context(), upload.ArtifactID)
					if err != nil || current.ReservedBytes+current.RetainedBytes != upload.Bytes {
						t.Fatal("uncertain Put lost quota custody", current, err)
					}
					if _, err := storage.memoryStorage.Put(t.Context(), storage.key, storage.body); err != nil {
						t.Fatal(err)
					}
				}
				if err := service.Delete(t.Context(), upload.ArtifactID); err != nil {
					t.Fatal(err)
				}
				if _, err := storage.Get(t.Context(), storage.key, "", 0); !errors.Is(err, ErrMissing) || storage.calls != 1 {
					t.Fatal("reconciliation repeated the uncertain Put or orphaned its blob", storage.calls, err)
				}
				usage, err := service.Usage(t.Context())
				if err != nil || usage.ReservedBytes != 0 || usage.RetainedBytes != 0 {
					t.Fatal("reconciled cleanup retained quota", usage, err)
				}
			})
		}
	}
}

func (b *admittedPutBarrier) Put(ctx context.Context, key string, data []byte) (string, error) {
	if b.match(key) {
		b.once.Do(func() { b.entered <- key; <-b.resume })
	}
	return b.Storage.Put(ctx, key, data)
}
func (b *admittedPutBarrier) unblock() { b.release.Do(func() { close(b.resume) }) }

func TestAdmittedPutRetainsCustodyDuringCleanup(t *testing.T) {
	for _, target := range []string{"object", "manifest"} {
		for _, cleanup := range []string{"delete", "interrupt"} {
			t.Run(target+"/"+cleanup, func(t *testing.T) {
				writer, storage := testService(t)
				cleaner, err := NewService(t.Context(), writer.config, storage, nil)
				if err != nil {
					t.Fatal(err)
				}
				barrier := &admittedPutBarrier{Storage: storage, match: func(key string) bool {
					if target == "manifest" {
						return strings.Contains(key, "/manifests/")
					}
					return strings.Contains(key, "/object_")
				}, entered: make(chan string, 1), resume: make(chan struct{})}
				writer.storage = barrier
				request := testReservation(writer)
				upload, err := writer.Reserve(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				part := testPart(0, "admitted write stays in custody")
				finished := make(chan struct{})
				var object Object
				var appendErr error
				t.Cleanup(func() { barrier.unblock(); <-finished })
				go func() { object, appendErr = writer.Append(t.Context(), upload.ArtifactID, part); close(finished) }()
				var key string
				select {
				case key = <-barrier.entered:
				case <-finished:
					t.Fatal("append failed before its storage barrier", appendErr)
				}
				clean := func() error {
					if cleanup == "delete" {
						return cleaner.Delete(t.Context(), upload.ArtifactID)
					}
					_, err := cleaner.Finalize(t.Context(), upload.ArtifactID, "interrupted", nil)
					return err
				}
				if err := clean(); err == nil {
					t.Fatal("cleanup released custody while an admitted storage Put was outstanding")
				}
				usage, err := cleaner.Usage(t.Context())
				if err != nil || usage.ReservedBytes+usage.RetainedBytes != request.Bytes {
					t.Fatal("outstanding write lost its reservation", usage, err)
				}
				barrier.unblock()
				<-finished
				if err := clean(); err != nil {
					t.Fatal("cleanup did not reconcile the completed write", err, appendErr)
				}
				usage, err = cleaner.Usage(t.Context())
				if err != nil || usage.ReservedBytes != 0 {
					t.Fatal("settled cleanup retained a reservation", usage, err)
				}
				if cleanup == "delete" || target == "manifest" {
					if _, err := storage.Get(t.Context(), key, "", MaxManifestBytes); !errors.Is(err, ErrMissing) {
						t.Fatal("cleanup left a late blob outside custody", err)
					}
				}
				if cleanup == "delete" {
					if usage.RetainedBytes != 0 {
						t.Fatal("deletion retained bytes", usage)
					}
				} else {
					_, ref, err := cleaner.Manifest(t.Context(), upload.ArtifactID, 0)
					if err != nil || ref.State != "interrupted" {
						t.Fatal("interrupted publication was lost", ref, err)
					}
					_, data, err := cleaner.ReadObject(t.Context(), upload.ArtifactID, ref.Revision, object.ID)
					if err != nil || !bytes.Equal(data, part.Data) {
						t.Fatal("settled content was neither retained nor deleted", err)
					}
				}
			})
		}
	}
}

func (m *memoryStorage) GetVersion(ctx context.Context, key, version string, limit int64) ([]byte, string, error) {
	data, err := m.Get(ctx, key, version, limit)
	return data, "version", err
}
func (b *admittedPutBarrier) GetVersion(ctx context.Context, key, version string, limit int64) ([]byte, string, error) {
	if storage, ok := b.Storage.(VersionedStorage); ok {
		return storage.GetVersion(ctx, key, version, limit)
	}
	return nil, "", ErrUnsupported
}
