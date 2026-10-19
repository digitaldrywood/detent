package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
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

func TestNativeReleasePublishesReportBeforeTag(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"Detent-Work-Item: wi_example", "Fixes #12\n\nDetent-Work-Item: wi_example"} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			const head = "0123456789abcdef0123456789abcdef01234567"
			tracker := fakehub.New(fakehub.Config{Stateful: true, Issues: []connector.Issue{{ID: "wi_example", Identifier: "prj_example#1"}}})
			var published atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var response string
				switch r.Method + " " + r.URL.Path {
				case "GET /repos/example-org/example-project/git/ref/heads/main":
					response = `{"object":{"sha":"` + head + `"}}`
				case "GET /repos/example-org/example-project/tags":
					response = `[]`
				case "GET /repos/example-org/example-project/commits":
					response = `[{"sha":"` + head + `","commit":{"message":"feat: initial release","committer":{"date":"2026-10-01T00:00:00Z"}}}]`
				case "GET /repos/example-org/example-project/commits/" + head + "/pulls":
					data, err := json.Marshal([]map[string]any{{"number": 9, "body": body}})
					if err != nil {
						t.Error(err)
						return
					}
					response = string(data)
				case "GET /repos/example-org/example-project/commits/" + head + "/check-runs":
					response = `{"check_runs":[{"id":101,"head_sha":"` + head + `","name":"CI","status":"completed","conclusion":"success"}]}`
				case "GET /repos/example-org/example-project/commits/" + head + "/status":
					response = `{"statuses":[]}`
				case "GET /repos/example-org/example-project/commits/refs/tags/v0.1.0":
					http.Error(w, "tag does not exist", http.StatusNotFound)
					return
				case "POST /repos/example-org/example-project/git/tags":
					events := tracker.Events()
					if len(events) != 1 || events[0].IssueID != "wi_example" || !strings.Contains(events[0].Body, "tag_intent") {
						t.Errorf("native report missing before tag: %+v", events)
					}
					response = `{"sha":"tag-object"}`
				case "POST /repos/example-org/example-project/git/refs":
					published.Store(true)
					response = `{}`
				default:
					t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(response))
			}))
			t.Cleanup(server.Close)
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerHubNative
			cfg.Tracker.Endpoint = server.URL + "/graphql"
			cfg.Tracker.APIKey = "fixture-token"
			cfg.Tracker.Repository = "example-org/example-project"
			cfg.Gate.RequiredStatusChecks = []string{"CI"}
			cfg.Release.Enabled = true
			cfg.Release.MinMergedIssues = 1
			build, err := buildReleaseCoordinator(cfg, tracker)
			if err != nil {
				t.Fatal(err)
			}
			status, decision := build.coordinator.Evaluate(t.Context(), time.Now())
			if decision.Action != "tag_created" || !published.Load() || status.PendingTag != "v0.1.0" {
				t.Fatalf("release evaluation: %+v, %+v, published=%t", status, decision, published.Load())
			}
			created, err := newGitHubReleaseBackend(cfg, tracker).EnsureReleaseReport(t.Context(), releasepkg.Report{
				IssueRefs: []string{"wi_example"}, Fingerprint: "tag_intent:example-org/example-project:" + head, Body: "Repeated tag intent",
			})
			if err != nil || created || len(tracker.Events()) != 1 {
				t.Fatalf("release report replay: created=%t, err=%v, events=%+v", created, err, tracker.Events())
			}
		})
	}
}

func TestNativeReleaseReportRejectsUnownedOrigins(t *testing.T) {
	t.Parallel()
	for _, refs := range [][]string{nil, {"example-org/example-project#12"}, {"wi_missing"}} {
		t.Run(strings.Join(refs, ","), func(t *testing.T) {
			t.Parallel()
			tracker := fakehub.New(fakehub.Config{})
			created, err := newGitHubReleaseBackend(workflowconfig.Default(), tracker).EnsureReleaseReport(t.Context(), releasepkg.Report{IssueRefs: refs})
			if err == nil || created || len(tracker.Events()) != 0 {
				t.Fatalf("unowned report accepted: created=%t, err=%v, events=%+v", created, err, tracker.Events())
			}
		})
	}
}
