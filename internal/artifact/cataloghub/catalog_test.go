package cataloghub

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var catalogTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	root := filepath.Join("..", "..", "hubserver", "postgres")
	roles, err := os.ReadFile(filepath.Join(root, "roles.sql"))
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS(filepath.Join(root, "migrations"))), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

func TestMain(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "catalog tests require a deadline")
		os.Exit(1)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	if err := catalogTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}

type catalogFunction func(context.Context, artifact.CatalogCommand) (artifact.CatalogResult, error)

func (f catalogFunction) Execute(ctx context.Context, c artifact.CatalogCommand) (artifact.CatalogResult, error) {
	return f(ctx, c)
}

type objects struct {
	mu   sync.Mutex
	data map[string][]byte
	lost bool
}

func (o *objects) Put(_ context.Context, key string, data []byte) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.data[key]; exists {
		return "version", artifact.ErrConflict
	}
	o.data[key] = bytes.Clone(data)
	if o.lost {
		o.lost = false
		return "", artifact.ErrStorage
	}
	return "version", nil
}
func (o *objects) Get(_ context.Context, key, version string, limit int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, exists := o.data[key]
	if !exists {
		return nil, artifact.ErrMissing
	}
	if int64(len(data)) > limit {
		return nil, artifact.ErrIntegrity
	}
	return bytes.Clone(data), nil
}
func (o *objects) Delete(_ context.Context, key, version string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.data, key)
	return nil
}

type scopedFixture struct {
	config     artifact.Config
	item       string
	credential string
	token      string
}

func TestAuthenticatedCatalogSurvivesHubRestart(t *testing.T) {
	admin := catalogTemplate.Database(t)
	role := "example_" + strings.TrimPrefix(artifact.NewID("role"), "role_")
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "GRANT detent_runtime_v2 TO "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	native := catalogTemplate.NativeConfig(t, admin, role, "")
	native.MaxConns = 3
	openRepository := func() *postgres.Repository {
		pool, err := pgxpool.NewWithConfig(t.Context(), native)
		if err != nil {
			t.Fatal(err)
		}
		repo, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repo.Close(); err != nil {
				t.Error(err)
			}
		})
		return repo
	}
	repo := openRepository()
	fixtures := []scopedFixture{}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for range 2 {
		org := tracker.OrganizationID(artifact.NewID("org"))
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			for n := range 2 {
				project := tracker.ProjectID(artifact.NewID("prj"))
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: fmt.Sprintf("example-%d", n), Profile: "native"}); err != nil {
					return err
				}
				item := artifact.NewID("wi")
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: tracker.NativeWorkItemID(item), Number: 1, Title: "Example"}); err != nil {
					return err
				}
				for range 2 {
					service, token, credential := artifact.NewID("service"), artifact.NewID("token"), artifact.NewID("credential")
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential, Name: credential, Hash: apikey.HashToken(token), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ProjectAccess: "selected", Kind: "service"}); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential, ProjectID: project}); err != nil {
						return err
					}
					if err := w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: service, Origin: "https://example.invalid", Mode: "customer", PublisherTokenID: credential}); err != nil {
						return err
					}
					cfg := artifact.Config{OrganizationID: string(org), ProjectID: string(project), ServiceID: service, Mode: "customer", Policy: artifact.Policy{ID: "example", Limits: artifact.Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: artifact.MaxChunkBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 600, DeletionRecordSeconds: 7200, BackupSeconds: 3600}}
					fixtures = append(fixtures, scopedFixture{config: cfg, item: item, credential: credential, token: token})
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	policy := func(ctx context.Context, _ postgres.ArtifactCatalogReader, scope artifact.CatalogScope) (artifact.Config, error) {
		for _, fixture := range fixtures {
			cfg := fixture.config
			if cfg.OrganizationID == scope.OrganizationID && cfg.ProjectID == scope.ProjectID && cfg.ServiceID == scope.ServiceID {
				return cfg, nil
			}
		}
		return artifact.Config{}, artifact.ErrDenied
	}
	backend, err := New(repo, policy)
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(ctx context.Context, token string, command artifact.CatalogCommand) error {
		return backend.repository.ReadTenant(ctx, tracker.OrganizationID(command.OrganizationID), corestore.Serving, func(r tenantstore.Reader) error {
			credential, err := r.CredentialByHash(ctx, apikey.HashToken(token))
			if err != nil || credential.RevokedAt != nil {
				return artifact.ErrDenied
			}
			if _, err := r.AccessTokenGrant(ctx, credential.ID, tracker.ProjectID(command.ProjectID)); err != nil {
				return artifact.ErrDenied
			}
			binding, err := r.ArtifactService(ctx, tracker.ProjectID(command.ProjectID), command.ServiceID)
			if err != nil || binding.PublisherTokenID != credential.ID {
				return artifact.ErrDenied
			}
			return nil
		})
	}
	var rejectComplete atomic.Bool
	var lostOperation atomic.Value
	lostOperation.Store("")
	handler, err := artifact.NewCatalogHandler(catalogFunction(func(ctx context.Context, c artifact.CatalogCommand) (artifact.CatalogResult, error) {
		if c.Operation == "commit" && c.Manifest.State == "complete" && rejectComplete.Swap(false) {
			return artifact.CatalogResult{}, artifact.ErrConflict
		}
		result, err := backend.Execute(ctx, c)
		if err == nil && c.Operation == lostOperation.Load().(string) {
			lostOperation.Store("")
			return artifact.CatalogResult{}, artifact.ErrAuthorization
		}
		return result, err
	}), authorize)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	unauthenticated := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+fixtures[0].config.OrganizationID+"/projects/"+fixtures[0].config.ProjectID+"/artifact-services/"+fixtures[0].config.ServiceID+"/catalog", strings.NewReader(fmt.Sprintf(`{"organization_id":%q,"project_id":%q,"service_id":%q,"operation":"traffic_settings"}`, fixtures[0].config.OrganizationID, fixtures[0].config.ProjectID, fixtures[0].config.ServiceID)))
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, unauthenticated)
	if denied.Code != http.StatusForbidden {
		t.Fatal("unauthenticated catalog route accepted", denied.Code)
	}
	storage := &objects{data: map[string][]byte{}}
	type persisted struct {
		fixture   scopedFixture
		upload    artifact.Upload
		reference artifact.Reference
		object    artifact.Object
		part      artifact.Part
	}
	persistedRows := []persisted{}
	sharedRows := []persisted{}
	sharedID := artifact.NewID("artifact")
	remote := func(f scopedFixture, token string) *artifact.RemoteCatalog {
		transport := &http.Transport{}
		t.Cleanup(transport.CloseIdleConnections)
		return &artifact.RemoteCatalog{Hub: &artifact.RemoteHub{Origin: server.URL, OrganizationID: f.config.OrganizationID, ProjectID: f.config.ProjectID, ServiceID: f.config.ServiceID, PublisherToken: func() string { return token }, Client: &http.Client{Transport: transport}}}
	}
	for _, fixture := range fixtures {
		cfg := fixture.config
		cfg.Catalog = remote(fixture, fixture.token)
		service, err := artifact.NewService(t.Context(), cfg, storage, nil)
		if err != nil {
			t.Fatal(err)
		}
		request := artifact.Reservation{Scope: artifact.Scope{OrganizationID: cfg.OrganizationID, ProjectID: cfg.ProjectID, WorkItemID: fixture.item, RunID: artifact.NewID("run"), AttemptID: artifact.NewID("attempt")}, ServiceID: cfg.ServiceID, Mode: cfg.Mode, Key: "example", Kind: "log", Bytes: 4 << 20, LeaseID: "example", FencingToken: 1}
		lostOperation.Store("reserve")
		if _, err := service.Reserve(t.Context(), request); !errors.Is(err, artifact.ErrAuthorization) {
			t.Fatal("lost reservation response", err)
		}
		upload, err := service.Reserve(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := service.Reserve(t.Context(), request)
		if err != nil || replay.ArtifactID != upload.ArtifactID {
			t.Fatal("reservation replay", replay, err)
		}
		changed := request
		changed.Bytes--
		if _, err := service.Reserve(t.Context(), changed); !errors.Is(err, artifact.ErrConflict) {
			t.Fatal("changed replay accepted", err)
		}
		part := artifact.Part{Sequence: 0, MediaType: "text/plain; charset=utf-8", Data: []byte("example content"), SHA256: artifact.Digest([]byte("example content"))}
		storage.lost = true
		if _, err := service.Append(t.Context(), upload.ArtifactID, part); !errors.Is(err, artifact.ErrStorage) {
			t.Fatal("lost blob response", err)
		}
		lostOperation.Store("commit")
		if _, err := service.Append(t.Context(), upload.ArtifactID, part); !errors.Is(err, artifact.ErrAuthorization) {
			t.Fatal("lost partial commit response", err)
		}
		object, err := service.Append(t.Context(), upload.ArtifactID, part)
		if err != nil {
			t.Fatal(err)
		}
		lostOperation.Store("commit")
		if _, err := service.Finalize(t.Context(), upload.ArtifactID, "complete", nil); !errors.Is(err, artifact.ErrAuthorization) {
			t.Fatal("lost final commit response", err)
		}
		ref, err := service.Finalize(t.Context(), upload.ArtifactID, "complete", nil)
		if err != nil {
			t.Fatal(err)
		}
		before, err := service.Usage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Finalize(t.Context(), upload.ArtifactID, "complete", nil); err != nil {
			t.Fatal(err)
		}
		after, err := service.Usage(t.Context())
		if err != nil || before != after {
			t.Fatal("replay charged quota", before, after, err)
		}
		if _, err := remote(fixture, "").Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: cfg.OrganizationID, ProjectID: cfg.ProjectID, ServiceID: cfg.ServiceID}, Operation: "upload", UploadID: upload.ArtifactID}); !errors.Is(err, artifact.ErrAuthorization) {
			t.Fatal("unauthenticated request", err)
		}
		if _, err := remote(fixture, fixtures[(len(persistedRows)+1)%len(fixtures)].token).Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: cfg.OrganizationID, ProjectID: cfg.ProjectID, ServiceID: cfg.ServiceID}, Operation: "upload", UploadID: upload.ArtifactID}); !errors.Is(err, artifact.ErrDenied) {
			t.Fatal("foreign credential accepted", err)
		}
		sharedRequest := request
		sharedRequest.Key = "shared-upload-id"
		sharedResult, err := cfg.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: cfg.OrganizationID, ProjectID: cfg.ProjectID, ServiceID: cfg.ServiceID}, Operation: "reserve", Upload: artifact.Upload{ArtifactID: sharedID, Reservation: sharedRequest, PartLimit: artifact.MaxArtifactBytes}})
		if err != nil {
			t.Fatal(err)
		}
		if sharedResult.Upload.PartLimit != cfg.Policy.Limits.UploadBytes {
			t.Fatal("client supplied admission limit accepted")
		}
		sharedPart := part
		sharedPart.Data = []byte(cfg.OrganizationID + "/" + cfg.ProjectID + "/" + cfg.ServiceID)
		sharedPart.SHA256 = artifact.Digest(sharedPart.Data)
		sharedObject, err := service.Append(t.Context(), sharedID, sharedPart)
		if err != nil {
			t.Fatal("object storage scope collision", err)
		}
		sharedRef, err := service.Finalize(t.Context(), sharedID, "complete", nil)
		if err != nil {
			t.Fatal("manifest storage scope collision", err)
		}
		sharedRows = append(sharedRows, persisted{fixture: fixture, upload: sharedResult.Upload, reference: sharedRef, object: sharedObject, part: sharedPart})
		rejectedRequest := request
		rejectedRequest.Key = "rejected-manifest"
		rejectedUpload, err := service.Reserve(t.Context(), rejectedRequest)
		if err != nil {
			t.Fatal(err)
		}
		rejectedObject, err := service.Append(t.Context(), rejectedUpload.ArtifactID, part)
		if err != nil {
			t.Fatal(err)
		}
		rejectComplete.Store(true)
		if _, err := service.Finalize(t.Context(), rejectedUpload.ArtifactID, "complete", nil); !errors.Is(err, artifact.ErrConflict) {
			t.Fatal("expected rejected commit", err)
		}
		interrupted, err := service.Finalize(t.Context(), rejectedUpload.ArtifactID, "interrupted", nil)
		if err != nil || interrupted.State != "interrupted" {
			t.Fatal("rejected body blocks interrupted finalization", err)
		}
		sharedRows = append(sharedRows, persisted{fixture: fixture, upload: rejectedUpload, reference: interrupted, object: rejectedObject, part: part})
		pendingRequest := request
		pendingRequest.Key = "pending-admission"
		pendingRequest.Kind = "diff"
		admitted, err := service.Reserve(t.Context(), pendingRequest)
		if err != nil {
			t.Fatal(err)
		}
		scope := artifact.CatalogScope{OrganizationID: cfg.OrganizationID, ProjectID: cfg.ProjectID, ServiceID: cfg.ServiceID}
		descriptor := artifact.CatalogObject{Object: artifact.Object{ID: artifact.NewID("object"), Sequence: 0, MediaType: "text/plain; charset=utf-8", Size: admitted.PartLimit + 1, SHA256: artifact.Digest([]byte("example"))}}
		if _, err := cfg.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: scope, Operation: "put_object", UploadID: admitted.ArtifactID, Object: descriptor}); !errors.Is(err, artifact.ErrQuota) {
			t.Fatal("admitted part limit bypassed", err)
		}
		var firstDescriptor artifact.CatalogObject
		for sequence := range 4 {
			descriptor.ID = artifact.NewID("object")
			descriptor.Sequence = sequence
			descriptor.Offset = int64(sequence) * admitted.PartLimit
			descriptor.Size = admitted.PartLimit
			descriptor.SHA256 = artifact.Digest(bytes.Repeat([]byte("x"), int(admitted.PartLimit)))
			if sequence == 0 {
				firstDescriptor = descriptor
			}
			if _, err := cfg.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: scope, Operation: "put_object", UploadID: admitted.ArtifactID, Object: descriptor}); err != nil {
				t.Fatal(err)
			}
		}
		descriptor.ID = artifact.NewID("object")
		descriptor.Sequence = 4
		descriptor.Size = 1
		descriptor.Offset = admitted.Bytes
		if _, err := cfg.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: scope, Operation: "put_object", UploadID: admitted.ArtifactID, Object: descriptor}); !errors.Is(err, artifact.ErrQuota) {
			t.Fatal("pending descriptors exceeded reservation", err)
		}
		dataKey := cfg.OrganizationID + "/" + cfg.ProjectID + "/" + cfg.ServiceID + "/" + admitted.ArtifactID + "/" + firstDescriptor.ID
		version, err := storage.Put(t.Context(), dataKey, bytes.Repeat([]byte("x"), int(admitted.PartLimit)))
		if err != nil {
			t.Fatal(err)
		}
		firstDescriptor.StorageVersion = version
		for range 2 {
			if _, err := cfg.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: scope, Operation: "verify_object", UploadID: admitted.ArtifactID, Object: firstDescriptor}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := cfg.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: scope, Operation: "put_object", UploadID: admitted.ArtifactID, Object: descriptor}); !errors.Is(err, artifact.ErrQuota) {
			t.Fatal("pending plus verified descriptors exceeded reservation", err)
		}
		if err := service.Delete(t.Context(), admitted.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		persistedRows = append(persistedRows, persisted{fixture: fixture, upload: upload, reference: ref, object: object, part: part})
	}
	server.Close()
	if err := backend.repository.Close(); err != nil {
		t.Fatal(err)
	}
	backend.repository = openRepository()
	server = httptest.NewServer(handler)
	t.Cleanup(server.Close)
	for _, row := range sharedRows {
		cfg := row.fixture.config
		cfg.Catalog = remote(row.fixture, row.fixture.token)
		service, err := artifact.NewService(t.Context(), cfg, storage, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, data, err := service.ReadObject(t.Context(), row.upload.ArtifactID, row.reference.Revision, row.object.ID)
		if err != nil || !bytes.Equal(data, row.part.Data) {
			t.Fatal("same artifact ID crossed storage scope", err)
		}
		if err := service.Delete(t.Context(), row.upload.ArtifactID); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range persistedRows {
		cfg := row.fixture.config
		cfg.Catalog = remote(row.fixture, row.fixture.token)
		service, err := artifact.NewService(t.Context(), cfg, storage, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, data, err := service.ReadObject(t.Context(), row.upload.ArtifactID, row.reference.Revision, row.object.ID)
		if err != nil || !bytes.Equal(data, row.part.Data) {
			t.Fatal("Hub restart lost catalog", err)
		}
		if err := service.Delete(t.Context(), row.upload.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.Manifest(t.Context(), row.upload.ArtifactID, 0); !errors.Is(err, artifact.ErrMissing) {
			t.Fatal("revoked artifact readable", err)
		}
		if err := backend.repository.WriteTenant(t.Context(), tracker.OrganizationID(cfg.OrganizationID), corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessCredential(t.Context(), row.fixture.credential, time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Upload(t.Context(), row.upload.ArtifactID); !errors.Is(err, artifact.ErrDenied) {
			t.Fatal("revoked publisher accepted", err)
		}
	}
}
