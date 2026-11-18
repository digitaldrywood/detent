package runnerconfiguration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type olderHub struct {
	mu       sync.Mutex
	older    bool
	reports  []bool
	claims   int
	refusals int
}

func (h *olderHub) setOlder(older bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.older, h.reports = older, nil
}

func (h *olderHub) observed() ([]bool, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reports), h.claims, h.refusals
}

func (h *olderHub) wrap(hub http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.mu.Lock()
		older := h.older
		if strings.Contains(req.URL.Path, "/claims") {
			h.claims++
		}
		h.mu.Unlock()
		if !older {
			hub.ServeHTTP(w, req)
			return
		}
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/capabilities"):
			recorder := httptest.NewRecorder()
			hub.ServeHTTP(recorder, req)
			var capabilities map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &capabilities); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			features, _ := capabilities["features"].([]any)
			capabilities["features"] = slices.DeleteFunc(features, func(feature any) bool { return feature == tracker.NativeHubProjectConfigurationCapability })
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(capabilities)
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/heartbeat"):
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var body struct {
				ProjectConfiguration *runnerauth.ProjectConfiguration `json:"project_configuration"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			h.reports = append(h.reports, body.ProjectConfiguration != nil)
			refuse := body.ProjectConfiguration != nil && body.ProjectConfiguration.Authority != "local_global_configuration"
			if refuse {
				h.refusals++
			}
			h.mu.Unlock()
			if refuse {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"not_ready","message":"Run event storage is not available"}`))
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(raw))
			recorder := httptest.NewRecorder()
			hub.ServeHTTP(recorder, req)
			var snapshot runnerauth.RoutingSnapshot
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &snapshot) != nil {
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			snapshot.ProjectConfiguration, snapshot.ProjectConfigurationRequest = nil, nil
			if snapshot.Changes != nil {
				snapshot.Changes.CapabilitiesDigest = "older-" + snapshot.Changes.CapabilitiesDigest
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snapshot)
		default:
			hub.ServeHTTP(w, req)
		}
	})
}

func TestRunnerOmitsHubConfigurationForOlderHub(t *testing.T) {
	for _, state := range []string{"idle", "paused", "drained"} {
		t.Run(state, func(t *testing.T) {
			hub := &olderHub{}
			r := startConfiguredRunner(t, hub.wrap)
			r.mustTick(t)
			r.mustTick(t)
			r.read(t)
			if state == "drained" {
				r.queue(t, "drain_local_project", "drain")
				r.mustTick(t)
				r.settled(t, "drain_local_project", true)
			}

			hub.setOlder(true)
			if err := r.tick(t); err == nil {
				t.Fatal("the downgraded Hub accepted a Hub-owned configuration report")
			}
			if err := r.flush(t); err != nil {
				t.Fatalf("host metrics flush after the downgrade: %v", err)
			}
			if state == "paused" {
				r.runtime.mu.Lock()
				r.runtime.paused = true
				r.runtime.mu.Unlock()
			}
			r.runtime.mu.Lock()
			paused, draining, applied, observations := r.runtime.paused, r.runtime.draining, slices.Clone(r.runtime.applied), r.runtime.observations
			r.runtime.mu.Unlock()
			for range 3 {
				r.mustTick(t)
			}
			reports, claims, refusals := hub.observed()
			if refusals != 1 || len(reports) < 4 || !reports[0] || slices.Contains(reports[1:], true) {
				t.Fatalf("older Hub received Hub-owned configuration reports %v after the first refusal (refusals=%d)", reports, refusals)
			}
			r.runtime.mu.Lock()
			unchanged := r.runtime.paused == paused && r.runtime.draining == draining && slices.Equal(r.runtime.applied, applied) && r.runtime.observations == observations
			r.runtime.mu.Unlock()
			if !unchanged {
				t.Fatalf("runner applied configuration against the older Hub: %+v", r.runtime)
			}
			if claims != 0 {
				t.Fatalf("runner requested %d claims", claims)
			}

			hub.setOlder(false)
			r.mustTick(t)
			command, requestID, draining := "drain_local_project", "drain-after-upgrade", true
			if state == "drained" {
				command, requestID, draining = "resume_local_project", "resume-after-upgrade", false
			}
			r.queue(t, command, requestID)
			r.mustTick(t)
			if current, applied := r.runtime.state(); current != draining || len(applied) == 0 || applied[len(applied)-1] != requestID || slices.Index(applied, requestID) != len(applied)-1 {
				t.Fatalf("upgraded Hub command delivery draining=%t applied=%v", current, applied)
			}
			r.settled(t, command, draining)
			r.mustTick(t)
			if _, applied := r.runtime.state(); slices.Index(applied, requestID) != len(applied)-1 {
				t.Fatalf("settled command was delivered again after the upgrade: %v", applied)
			}
		})
	}
}
