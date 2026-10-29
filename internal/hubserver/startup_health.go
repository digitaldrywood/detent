package hubserver

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

type MigrationHealth struct {
	SchemaVersion     int64 `json:"schema_version"`
	MigrationsRunning bool  `json:"migrations_running"`
}

type startupHealth struct {
	verifier *cloudassert.Verifier
	config   Config
	service  atomic.Pointer[Service]
	database atomic.Pointer[sql.DB]
	running  atomic.Bool
}

func (s *startupHealth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if service := s.service.Load(); service != nil {
		service.Handler().ServeHTTP(w, r)
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
	response := MigrationHealth{MigrationsRunning: s.running.Load()}
	if db := s.database.Load(); db != nil {
		var exists bool
		err = db.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='hub_schema_version')").Scan(&exists)
		if err == nil && exists {
			response.SchemaVersion, err = currentSchemaVersion(r.Context(), db)
		}
		if err != nil {
			http.Error(w, "Hub schema unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(response)
}
