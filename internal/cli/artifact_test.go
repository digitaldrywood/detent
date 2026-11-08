package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func TestArtifactMaintenanceCommands(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, operation := range []string{"usage", "backup", "invalid config", "public listener"} {
		t.Run(operation, func(t *testing.T) {
			cfg := artifact.Config{ServiceID: artifact.NewID("service"), OrganizationID: artifact.NewID("org"), Mode: "customer", DatabasePath: filepath.Join(t.TempDir(), "catalog.db"), HubOrigin: "https://hub.example.com", PublishTokenEnv: "PUBLISHER", Policy: artifact.Policy{ID: "test", Limits: artifact.Limits{RetainedBytes: 4 << 20, ReservedBytes: 4 << 20, ArtifactBytes: 2 << 20, UploadBytes: 1 << 20, RetentionSeconds: 60}, AbandonedUploadSeconds: 30, DeletionRecordSeconds: 120, BackupSeconds: 60}, Storage: artifact.StorageConfig{Kind: "s3", Region: "us-east-1", Bucket: "example"}}
			if operation == "invalid config" {
				cfg.Policy.BackupSeconds = 0
			}
			handler, err := artifact.NewCatalogHandler(artifactCommandCatalog(func(ctx context.Context, command artifact.CatalogCommand) (artifact.CatalogResult, error) {
				if command.OrganizationID != cfg.OrganizationID || command.ServiceID != cfg.ServiceID {
					return artifact.CatalogResult{}, artifact.ErrDenied
				}
				switch command.Operation {
				case "usage":
					return artifact.CatalogResult{}, nil
				case "export":
					return artifact.CatalogResult{Complete: true}, nil
				}
				return artifact.CatalogResult{}, artifact.ErrInvalid
			}), func(_ context.Context, token string, _ artifact.CatalogCommand) error {
				if token != "test" {
					return artifact.ErrDenied
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := httptest.NewServer(handler)
			t.Cleanup(hub.Close)
			cfg.HubOrigin = hub.URL
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "catalog.jsonl")
			args := []string{"--artifact-config", path}
			switch operation {
			case "backup":
				args = append(args, "backup", "--output", destination)
			case "public listener":
				args = append(args, "serve", "--listen", "0.0.0.0:7788")
			default:
				args = append(args, "usage")
			}
			cmd := newArtifactCommand(func(string) string { return "test" })
			cmd.SetArgs(args)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err = cmd.ExecuteContext(t.Context())
			if operation == "invalid config" || operation == "public listener" {
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if operation == "usage" && out.String() != "{\"retained_bytes\":0,\"reserved_bytes\":0}\n" {
				t.Fatal(out.String())
			}
			if operation == "backup" {
				if _, err := os.Stat(destination); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type artifactCommandCatalog func(context.Context, artifact.CatalogCommand) (artifact.CatalogResult, error)

func (f artifactCommandCatalog) Execute(ctx context.Context, command artifact.CatalogCommand) (artifact.CatalogResult, error) {
	return f(ctx, command)
}

func TestArtifactServeInitializesHubCatalog(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "example")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "multiple projects", true: "one project"}[restricted], func(t *testing.T) {
			var objects sync.Map
			storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fail := func(status int, code string) {
					w.WriteHeader(status)
					if _, err := io.WriteString(w, "<Error><Code>"+code+"</Code></Error>"); err != nil {
						t.Error(err)
					}
				}
				switch r.Method {
				case http.MethodPut:
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if _, exists := objects.LoadOrStore(r.URL.Path, body); exists {
						fail(http.StatusPreconditionFailed, "PreconditionFailed")
					}
				case http.MethodGet, http.MethodHead:
					value, exists := objects.Load(r.URL.Path)
					if !exists {
						fail(http.StatusNotFound, "NoSuchKey")
						return
					}
					if r.Method == http.MethodGet {
						if _, err := w.Write(value.([]byte)); err != nil {
							t.Error(err)
						}
					}
				case http.MethodDelete:
					objects.Delete(r.URL.Path)
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			t.Cleanup(storage.Close)
			cfg := artifact.Config{ServiceID: artifact.NewID("service"), OrganizationID: artifact.NewID("org"), Mode: "customer", PublishTokenEnv: "PUBLISHER", Policy: artifact.Policy{ID: "example", Limits: artifact.Limits{RetainedBytes: 4 << 20, ReservedBytes: 4 << 20, ArtifactBytes: 2 << 20, UploadBytes: 1 << 20, RetentionSeconds: 60}, AbandonedUploadSeconds: 30, DeletionRecordSeconds: 120, BackupSeconds: 60}, Storage: artifact.StorageConfig{Kind: "s3", Endpoint: storage.URL, Region: "us-east-1", Bucket: "example", PathStyle: true}}
			project := ""
			if restricted {
				project = artifact.NewID("prj")
			}
			ready := make(chan struct{})
			var once sync.Once
			handler, err := artifact.NewCatalogHandler(artifactCommandCatalog(func(ctx context.Context, command artifact.CatalogCommand) (artifact.CatalogResult, error) {
				if command.Operation != "traffic_settings" || command.OrganizationID != cfg.OrganizationID || command.ServiceID != cfg.ServiceID || command.ProjectID != project {
					t.Error("CLI selected the wrong Hub catalog scope")
					return artifact.CatalogResult{}, artifact.ErrInvalid
				}
				once.Do(func() { close(ready) })
				return artifact.CatalogResult{Traffic: artifact.CatalogTraffic{WindowSeconds: 3600, RetentionSeconds: 86400}}, nil
			}), func(_ context.Context, token string, _ artifact.CatalogCommand) error {
				if token != "publisher" {
					return artifact.ErrDenied
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := httptest.NewServer(handler)
			t.Cleanup(hub.Close)
			cfg.HubOrigin = hub.URL
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "service.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--artifact-config", path, "serve", "--listen", "127.0.0.1:0"}
			if restricted {
				args = append(args, "--project", project)
			}
			command := newArtifactCommand(func(key string) string {
				if key == "PUBLISHER" {
					return "publisher"
				}
				return ""
			})
			command.SetArgs(args)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			ctx, cancel := context.WithCancel(t.Context())
			finished := make(chan struct{})
			var runErr error
			t.Cleanup(func() { cancel(); <-finished })
			go func() { runErr = command.ExecuteContext(ctx); close(finished) }()
			select {
			case <-ready:
			case <-finished:
				t.Fatalf("serve exited before initializing Hub catalog: %v %s", runErr, output.String())
			case <-t.Context().Done():
				t.Fatal("serve did not initialize Hub catalog")
			}
			cancel()
			<-finished
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Fatal(runErr, output.String())
			}
		})
	}
}
