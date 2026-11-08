package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestHealthEndpoint(t *testing.T) {
	t.Parallel()

	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Version:      "v-test",
	})

	schemaVersion, err := postgres.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}

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
				Status:              "ok",
				SchemaVersion:       schemaVersion,
				SchemaFingerprint:   service.database.schemaFingerprint,
				HubSchemaApplicable: true,
				SchemaFactsComplete: true,
				Version:             "v-test",
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
				var body struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != "not_found" {
					t.Fatalf("unmounted route: %s (%v)", response.Body.String(), err)
				}
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

	if err := service.Shutdown(testenv.WaitContext(t, context.Background())); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
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
	if testing.Short() {
		t.Skip("service lifecycle integration")
	}

	t.Parallel()

	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.Logger = discardLogger()
	expectedVersion, err := postgres.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	listenerAccepting := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	runResult := make(chan error, 1)
	cfg.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &acceptSignalListener{Listener: listener, accepting: listenerAccepting}, nil
	}
	go func() {
		defer close(runDone)
		runResult <- Run(ctx, cfg)
	}()
	cancelAndJoin := func() bool {
		cancel()
		select {
		case <-runDone:
			return true
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Error("Run() did not stop after cancellation")
			return false
		}
	}
	t.Cleanup(func() {
		cancel()
		<-runDone
	})

	select {
	case <-listenerAccepting:
	case <-runDone:
		t.Fatalf("Run() returned before accepting listener connections: %v", <-runResult)
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("Run() did not start accepting listener connections")
	}
	if !cancelAndJoin() {
		return
	}
	if err := <-runResult; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	assertStartupDisconnected(t, admin, connection)
	service := openTestService(t, cfg)
	if service.database.schemaVersion != expectedVersion {
		t.Fatalf("reopened schema version = %d, want %d", service.database.schemaVersion, expectedVersion)
	}
}

type acceptSignalListener struct {
	net.Listener
	accepting chan struct{}
	once      sync.Once
}

func (l *acceptSignalListener) Accept() (net.Conn, error) {
	l.once.Do(func() {
		close(l.accepting)
	})
	return l.Listener.Accept()
}
