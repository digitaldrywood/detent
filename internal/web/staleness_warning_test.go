package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestStalenessWarningAcknowledgementSurvivesNextSSESnapshot(t *testing.T) {
	t.Parallel()
	backend := &stalenessWebStore{}
	now := time.Date(2026, 8, 20, 16, 0, 0, 0, time.UTC)
	raw := telemetry.Snapshot{
		Seq:         1,
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		StalenessWarnings: []telemetry.StalenessWarning{
			{ID: "warning-1", Class: observability.ClassFault, ProjectID: "detent", Kind: "lane_aging", Identifier: "detent#1", Detail: "first warning"},
			{ID: "warning-2", Class: observability.ClassFault, ProjectID: "detent", Kind: "repeated_decision", Identifier: "detent#2", Detail: "second warning"},
		},
	}
	deps := testDeps(t)
	deps.Store = backend
	acknowledgements, err := staleness.NewAcknowledgements(t.Context(), backend, deps.Hub, []string{"detent"})
	if err != nil {
		t.Fatalf("NewAcknowledgements() error = %v", err)
	}
	deps.StalenessWarnings = acknowledgements
	if err := acknowledgements.Publish(raw); err != nil {
		t.Fatalf("Publish(initial) error = %v", err)
	}
	server, err := web.NewServer(web.Config{
		GlobalConfig:        globalconfig.Config{APIToken: "detent_test_token"},
		Now:                 func() time.Time { return now },
		SSETickInterval:     time.Hour,
		SSEFragmentInterval: -1,
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	addr := startWebServer(t, server)
	conn, reader := openRawEventStream(t, addr, "/events?view=board")

	initial := readRawSSEEventNamed(t, conn, reader, "snapshot")
	if !strings.Contains(initial.data, "detent#1") || !strings.Contains(initial.data, `data-board-alert-count="2"`) {
		t.Fatalf("initial SSE snapshot missing both warnings:\n%s", initial.data)
	}

	path := "/api/v1/projects/detent/staleness-warnings/warning-1/acknowledge"
	recorder := performJSON(t, server.Handler(), http.MethodPost, path, "", map[string]string{"Authorization": "Bearer detent_test_token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		WarningID      string    `json:"warning_id"`
		AcknowledgedAt time.Time `json:"acknowledged_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.WarningID != "warning-1" || !response.AcknowledgedAt.Equal(now) {
		t.Fatalf("response = %#v, want warning-1 at %v", response, now)
	}

	afterDismiss := readRawSSEEventNamed(t, conn, reader, "snapshot")
	assertFilteredStalenessSnapshot(t, afterDismiss.data)

	raw.Seq = 2
	raw.GeneratedAt = now.Add(time.Second)
	raw.StalenessWarnings[1].AgeSeconds = 1
	if err := acknowledgements.Publish(raw); err != nil {
		t.Fatalf("Publish(unchanged orchestrator state) error = %v", err)
	}
	afterNextTick := readRawSSEEventNamed(t, conn, reader, "snapshot")
	assertFilteredStalenessSnapshot(t, afterNextTick.data)

	state := performJSON(t, server.Handler(), http.MethodGet, "/api/v1/state", "", map[string]string{"Authorization": "Bearer detent_test_token"})
	if state.Code != http.StatusOK {
		t.Fatalf("state status = %d, want %d; body = %s", state.Code, http.StatusOK, state.Body.String())
	}
	if strings.Contains(state.Body.String(), `"id":"warning-1"`) || !strings.Contains(state.Body.String(), `"id":"warning-2"`) {
		t.Fatalf("state API did not use effective snapshot: %s", state.Body.String())
	}

	unknown := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/staleness-warnings/unknown/acknowledge", "", map[string]string{"Authorization": "Bearer detent_test_token"})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown warning status = %d, want %d; body = %s", unknown.Code, http.StatusNotFound, unknown.Body.String())
	}
}

func TestBulkStalenessWarningAcknowledgementUsesExplicitTransactionalIDs(t *testing.T) {
	t.Parallel()
	backend := &stalenessWebStore{}
	now := time.Date(2026, 8, 20, 17, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	deps.Store = backend
	acknowledgements, err := staleness.NewAcknowledgements(t.Context(), backend, deps.Hub, []string{"detent"})
	if err != nil {
		t.Fatalf("NewAcknowledgements() error = %v", err)
	}
	deps.StalenessWarnings = acknowledgements
	if err := acknowledgements.Publish(telemetry.Snapshot{
		GeneratedAt: now,
		StalenessWarnings: []telemetry.StalenessWarning{
			{ID: "warning-1", ProjectID: "detent"},
			{ID: "warning-2", ProjectID: "detent"},
			{ID: "warning-after-render", ProjectID: "detent"},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{
		GlobalConfig: globalconfig.Config{APIToken: "detent_test_token"},
		Now:          func() time.Time { return now },
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	path := "/api/v1/projects/detent/staleness-warnings/acknowledge"
	headers := map[string]string{"Authorization": "Bearer detent_test_token"}
	requestBody := `{"warning_ids":["warning-1","warning-2"]}`
	for attempt := range 2 {
		recorder := performJSON(t, server.Handler(), http.MethodPost, path, requestBody, headers)
		if recorder.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want %d; body = %s", attempt+1, recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}
	latest, ok := deps.Hub.Latest()
	if !ok || len(latest.StalenessWarnings) != 1 || latest.StalenessWarnings[0].ID != "warning-after-render" {
		t.Fatalf("effective warnings = %#v, want only warning-after-render", latest.StalenessWarnings)
	}
	states, err := backend.ListStalenessWarningStates(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListStalenessWarningStates() error = %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("acknowledged states = %#v, want exactly two explicit acknowledgements", states)
	}
	if !reflect.DeepEqual(backend.batches, [][]string{{"warning-1", "warning-2"}, {"warning-1", "warning-2"}}) {
		t.Fatalf("batch calls = %+v", backend.batches)
	}

	invalid := performJSON(t, server.Handler(), http.MethodPost, path, `{"warning_ids":["warning-after-render","unknown"]}`, headers)
	if invalid.Code != http.StatusNotFound {
		t.Fatalf("mixed invalid status = %d, want %d; body = %s", invalid.Code, http.StatusNotFound, invalid.Body.String())
	}
	states, err = backend.ListStalenessWarningStates(t.Context(), "detent")
	if err != nil {
		t.Fatalf("ListStalenessWarningStates() after invalid request error = %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("mixed invalid request persisted partial acknowledgement: %#v", states)
	}

	empty := performJSON(t, server.Handler(), http.MethodPost, path, `{"warning_ids":["warning-after-render",""]}`, headers)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty warning ID status = %d, want %d; body = %s", empty.Code, http.StatusBadRequest, empty.Body.String())
	}
	if len(backend.batches) != 2 {
		t.Fatalf("invalid request called batch writer: %+v", backend.batches)
	}

}

func assertFilteredStalenessSnapshot(t *testing.T, snapshot string) {
	t.Helper()
	for _, want := range []string{`data-board-alert-count="1"`, ">1 fault<", "detent#2"} {
		if !strings.Contains(snapshot, want) {
			t.Fatalf("filtered snapshot missing %q:\n%s", want, snapshot)
		}
	}
	for _, forbidden := range []string{"detent#1", "first warning"} {
		if strings.Contains(snapshot, forbidden) {
			t.Fatalf("filtered snapshot restored %q:\n%s", forbidden, snapshot)
		}
	}
}

type stalenessWebStore struct {
	storeProbe
	mu      sync.Mutex
	states  []store.StalenessWarningState
	batches [][]string
}

func (s *stalenessWebStore) ListStalenessWarningStates(ctx context.Context, project string) ([]store.StalenessWarningState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []store.StalenessWarningState{}
	for _, state := range s.states {
		if state.ProjectID == project {
			result = append(result, state)
		}
	}
	return result, ctx.Err()
}

func (s *stalenessWebStore) AcknowledgeStalenessWarnings(ctx context.Context, project string, ids []string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, slices.Clone(ids))
	for _, id := range ids {
		index := slices.IndexFunc(s.states, func(state store.StalenessWarningState) bool {
			return state.ProjectID == project && state.WarningID == id
		})
		if index < 0 {
			s.states = append(s.states, store.StalenessWarningState{ProjectID: project, WarningID: id, AcknowledgedAt: new(at)})
		} else {
			s.states[index].AcknowledgedAt = new(at)
		}
	}
	return nil
}
