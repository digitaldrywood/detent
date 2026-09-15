package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestStopRecordedRunBeforeProjectStartup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		pending, stale bool
		target         string
	}{
		{name: "initializing", pending: true},
		{name: "removed"},
		{name: "initializing custom target", pending: true, target: "Paused"},
		{name: "stale identity", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "stop.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { backend.Close() })
			id, err := backend.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "project", IssueID: "issue", WorkerType: "agent", StartedAt: time.Now().Add(-22 * time.Hour), DetentSessionID: 91})
			if err != nil {
				t.Fatal(err)
			}
			registry := project.NewRegistry()
			if tc.pending {
				cfg := globalconfig.Project{ID: "project"}
				if tc.target != "" {
					cfg.Workflow = filepath.Join(t.TempDir(), "WORKFLOW.md")
					if err := os.WriteFile(cfg.Workflow, []byte(`---
tracker:
  kind: memory
  observed_states: [Blocked, Paused]
agent:
  stop_run:
    target_state: Paused
---
Work`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := registry.SetPending(cfg, project.RuntimeError{}); err != nil {
					t.Fatal(err)
				}
			}
			stopper := registryRefresher{registry: registry, attempts: backend, processes: backend}
			sessionID := int64(91)
			if tc.stale {
				sessionID++
			}
			server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "test-token"}, StaticDir: t.TempDir()}, web.Dependencies{Hub: hub.New[telemetry.Snapshot](), Registry: registry, Store: backend, Connector: memory.New(memory.Config{}), RunStopper: stopper})
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"issue_id":"issue","work_attempt_id":%d,"detent_session_id":%d,"confirm":true}`, id, sessionID)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project/runs/1/stop", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			wantStatus := http.StatusOK
			if tc.stale {
				wantStatus = http.StatusConflict
			}
			if response.Code != wantStatus {
				t.Fatalf("HTTP %d, want %d: %s", response.Code, wantStatus, response.Body.String())
			}
			if !tc.stale {
				var result orchestrator.StopRunResult
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				wantOutcome := "stopped"
				if tc.target != "" {
					wantOutcome = "pending"
				}
				if result.Destination != tc.target {
					t.Fatalf("destination = %q, want %q", result.Destination, tc.target)
				}
				if result.Outcome != wantOutcome {
					t.Fatalf("outcome = %q", result.Outcome)
				}
			}
			attempt, err := backend.WorkAttempt(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stale {
				if attempt.Status != store.WorkAttemptStatusActive {
					t.Fatalf("stale request changed attempt: %+v", attempt)
				}
			} else if attempt.TerminalState != store.WorkAttemptTerminalOperatorStopped {
				t.Fatalf("terminal state = %s", attempt.TerminalState)
			}
		})
	}
}
