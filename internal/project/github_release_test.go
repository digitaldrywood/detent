package project

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestNativeReleaseUsesGitHubIntegration(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/repos/example-org/example-project/git/ref/heads/main" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected release request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"fixture refusal"}`))
	}))
	t.Cleanup(server.Close)
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerHubNative
	cfg.Tracker.Endpoint = server.URL + "/graphql"
	cfg.Tracker.Repository = "example-org/example-project"
	cfg.Release.Enabled = true
	cfg = workflowConfigWithGitHubToken(cfg, "fixture-token")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	build, err := buildReleaseCoordinator(cfg, fakehub.New(fakehub.Config{}))
	if err != nil || build.coordinator == nil {
		t.Fatalf("native release coordinator: %v", err)
	}
	status, decision := build.coordinator.Evaluate(t.Context(), time.Now())
	if !status.Enabled || decision.Action != "inspect_failed" || calls.Load() != 1 {
		t.Fatalf("release evaluation: %+v, %+v, calls=%d", status, decision, calls.Load())
	}
}
