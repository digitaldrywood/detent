package hubserver

import (
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

type MigrationHealth struct {
	SchemaVersion       int64  `json:"schema_version"`
	SchemaFingerprint   string `json:"schema_fingerprint"`
	MigrationsRunning   bool   `json:"migrations_running"`
	SchemaFactsComplete bool   `json:"schema_facts_complete"`
}

type startupHealthResponse struct {
	MigrationHealth
	HubSchemaApplicable      bool   `json:"hub_schema_applicable"`
	RegistrySchemaVersion    int64  `json:"registry_schema_version"`
	RegistrySchemaApplicable bool   `json:"registry_schema_applicable"`
	AuthSchemaVersion        int64  `json:"auth_schema_version"`
	AuthSchemaApplicable     bool   `json:"auth_schema_applicable"`
	PlatformSchemaVersion    int64  `json:"platform_schema_version"`
	PlatformSchemaApplicable bool   `json:"platform_schema_applicable"`
	Status                   string `json:"status"`
	Version                  string `json:"version"`
	Commit                   string `json:"commit"`
}

type startupHealth struct {
	verifier *cloudassert.Verifier
	config   Config
	service  atomic.Pointer[Service]
	auth     atomic.Pointer[Service]
	schema   atomic.Pointer[MigrationHealth]
	running  atomic.Bool
}

func (s *startupHealth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if service := s.service.Load(); service != nil {
		service.Handler().ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.RequestURI() == "/health" && (s.config.Hosted == nil || s.config.Hosted.SharedEntry == nil) {
		s.serveStandaloneHealth(w, r)
		return
	}
	if s.config.Hosted == nil || s.config.Hosted.SharedEntry == nil || r.Method != http.MethodPost || r.URL.RequestURI() != "/internal/v1/health" {
		http.Error(w, "Hub is starting", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(body) > 1024 {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	claims, err := s.verifier.Verify(r.Header.Get(cloudassert.Header), r.Method, r.URL.RequestURI(), body)
	if err != nil || claims.Kind != cloudassert.KindService {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.writeUnavailable(w)
}

func (s *startupHealth) serveStandaloneHealth(w http.ResponseWriter, r *http.Request) {
	service := s.auth.Load()
	if service == nil {
		http.Error(w, "Hub is starting", http.StatusServiceUnavailable)
		return
	}
	c := echo.New().NewContext(r, w)
	c.SetPath("/health")
	credential, status, err := service.authenticateAPIRequestContext(r.Context(), c)
	if err != nil {
		http.Error(w, "Unauthorized", status)
		return
	}
	if credential.Scope != hubapi.APIScopeWorker && credential.Scope != hubapi.APIScopeOperator && credential.Scope != hubapi.APIScopeAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	s.writeUnavailable(w)
}

func (s *startupHealth) writeUnavailable(w http.ResponseWriter) {
	schema := MigrationHealth{MigrationsRunning: s.running.Load()}
	if verified := s.schema.Load(); verified != nil {
		schema = *verified
	}
	response := startupHealthResponse{MigrationHealth: schema, HubSchemaApplicable: true, Status: "unavailable", Version: s.config.Version, Commit: s.config.Commit}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(response)
}
