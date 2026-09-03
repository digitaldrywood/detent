package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthEndpoint(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Version:      "v-test",
	})

	tests := []struct {
		name       string
		path       string
		ready      bool
		wantStatus int
		wantBody   healthResponse
	}{
		{
			name:       "healthy",
			path:       "/health",
			ready:      true,
			wantStatus: http.StatusOK,
			wantBody: healthResponse{
				Status:        "ok",
				SchemaVersion: supportedSchemaVersion,
				Version:       "v-test",
			},
		},
		{
			name:       "not ready",
			path:       "/health",
			ready:      false,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   healthResponse{Status: "unavailable"},
		},
		{
			name:       "unregistered route",
			path:       "/api/v1/not-found",
			ready:      true,
			wantStatus: http.StatusNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service.ready.Store(test.ready)
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			authorizeHubTestRequest(request)
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantBody.Status == "" {
				return
			}
			var got healthResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !reflect.DeepEqual(got, test.wantBody) {
				t.Fatalf("response = %#v, want %#v", got, test.wantBody)
			}
		})
	}
	service.ready.Store(true)
}

func TestHealthEndpointDegradesForStaleRepository(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{
		DatabasePath:      filepath.Join(t.TempDir(), "hub.db"),
		ReconcileInterval: time.Minute,
		now:               func() time.Time { return now },
	})
	if _, err := service.database.db.ExecContext(t.Context(), `
		INSERT INTO repositories (github_node_id, github_owner, github_name, last_webhook_at, created_at, updated_at)
		VALUES ('R_stale', 'digitaldrywood', 'detent', ?, ?, ?)
	`, formatWebhookTime(now), testTimestamp, testTimestamp); err != nil {
		t.Fatalf("insert stale repository: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	authorizeHubTestRequest(request)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d body = %s, want 200", response.Code, response.Body.String())
	}
	var got healthResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if got.Status != "degraded" || got.Repositories.Total != 1 || got.Repositories.Stale != 1 {
		t.Fatalf("health response = %#v, want degraded stale repository", got)
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- service.Serve(listener)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/health", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	authorizeHubTestRequest(request)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", response.StatusCode)
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not return after Shutdown()")
	}
	if err := service.Backup(t.Context(), filepath.Join(t.TempDir(), "backup.db")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Backup() after Shutdown error = %v, want ErrNotReady", err)
	}
}

func TestServeRequiresListener(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	if err := service.Serve(nil); err == nil {
		t.Fatal("Serve(nil) error = nil")
	}
}

func TestRunStopsOnContextCancellationAndReleasesDatabase(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "hub.db")
	serving := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(&servingSignalWriter{ready: serving}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Config{
			DatabasePath:  databasePath,
			ListenAddress: "127.0.0.1:0",
			Logger:        logger,
		})
	}()

	select {
	case <-serving:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not bind its listener")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}

	service := openTestService(t, Config{DatabasePath: databasePath})
	if service.database.schemaVersion != supportedSchemaVersion {
		t.Fatalf("reopened schema version = %d, want %d", service.database.schemaVersion, supportedSchemaVersion)
	}
}

type servingSignalWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (w *servingSignalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "hub serving") {
		w.once.Do(func() {
			close(w.ready)
		})
	}
	return io.Discard.Write(p)
}
