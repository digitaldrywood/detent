package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type restRecoveryHTTPClient func(*http.Request) (*http.Response, error)

func (f restRecoveryHTTPClient) Do(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGitHubRESTOutageSurvivesContradictoryProbes(t *testing.T) {
	t.Parallel()
	for _, remaining := range []int{5000, 4000} {
		t.Run(strconv.Itoa(remaining), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				reset := started.Add(13 * time.Minute)
				healthy := false
				reads, probes := 0, 0
				tracker, err := github.NewConnector(github.Config{
					Endpoint:    fmt.Sprintf("https://recovery-%d.test/graphql", remaining),
					Repository:  "example/repo",
					TokenSource: github.StaticTokenSource("test-token"),
					HTTPClient: restRecoveryHTTPClient(func(r *http.Request) (*http.Response, error) {
						headers := http.Header{}
						headers.Set("X-RateLimit-Limit", "5000")
						headers.Set("X-RateLimit-Resource", "core")
						status := http.StatusOK
						headers.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
						headers.Set("X-RateLimit-Used", strconv.Itoa(5000-remaining))
						headers.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
						if r.URL.Path == "/rate_limit" {
							probes++
						} else {
							reads++
							if !healthy {
								status = http.StatusForbidden
								headers.Set("X-RateLimit-Remaining", "0")
								headers.Set("X-RateLimit-Used", "5000")
								headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
							}
						}
						return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tracker.RepositoryMergeSettings(t.Context(), "example/repo"); !errors.Is(err, github.ErrRateLimited) {
					t.Fatalf("seed = %v", err)
				}
				cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, GitHubRESTMinReserve: 1000})
				orch := newRateLimitTestOrchestrator(cfg, tracker)
				var logs bytes.Buffer
				orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
				state := newState(cfg)
				if !orch.githubLookupBackoffGate(t.Context(), &state, time.Now()) {
					t.Fatal("initial outage missing")
				}
				for step := 1; step <= 6; step++ {
					_, outage, active := githubLookupBackoff(state.BackendOutages)
					if !active || outage.ProbeAttempts != step || !outage.DetectedAt.Equal(started) {
						t.Fatalf("outage lost continuity: %+v", outage)
					}
					if !orch.githubLookupBackoffGate(t.Context(), &state, time.Now()) {
						t.Fatal("early recovery")
					}
					time.Sleep(time.Until(outage.NextProbeAt))
					if !orch.githubLookupBackoffGate(t.Context(), &state, time.Now()) {
						t.Fatal("contradictory quota recovered outage")
					}
				}
				if strings.Contains(logs.String(), "github lookup backoff recovered") {
					t.Fatal("false recovery event")
				}
				if reads != 3 || probes != 6 {
					t.Fatalf("outage requests: reads=%d probes=%d, want 3/6", reads, probes)
				}
				healthy = true
				_, outage, _ := githubLookupBackoff(state.BackendOutages)
				time.Sleep(time.Until(outage.NextProbeAt))
				if orch.githubLookupBackoffGate(t.Context(), &state, time.Now()) {
					t.Fatal("representative success did not recover")
				}
				if strings.Count(logs.String(), "github lookup backoff recovered") != 1 || !strings.Contains(logs.String(), "backoff_steps=7") {
					t.Fatalf("recovery telemetry: %s", logs.String())
				}
			})
		})
	}
}
