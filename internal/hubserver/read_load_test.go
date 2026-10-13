package hubserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestProjectListAndRunnerReadContention(t *testing.T) {
	if testing.Short() {
		t.Skip("tenant read and heartbeat load")
	}
	var logs hostedLogBuffer
	plans := capacityHostedPlans()
	plans.Base = PlanReference{ID: "scale", Version: 1}
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, func(cfg *Config) { cfg.Hosted.Plans = &plans; cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	for index := 2; index < 30; index++ {
		f.createProject(t, fmt.Sprintf("Load project %d", index))
	}
	grantAppRunners(t, f)
	runners := make([]appTestRunner, 10)
	for index := range runners {
		runners[index] = enrollAppRunner(t, f, fmt.Sprintf("Load runner %d", index), "test")
	}
	warmReaderPools(t, f.service.database)
	f.service.stopHealthDetector()
	type latencySample struct {
		projects, heartbeat time.Duration
	}
	reader := f.service.database.reader
	defer func() { f.service.database.reader = reader }()
	measure := func(t *testing.T) latencySample {
		projectLatencies := make([]time.Duration, 60)
		for index := range projectLatencies {
			started := time.Now()
			response := f.page(t, "owner", browserHostedOrganizationBase+"/projects")
			requireNativeStatus(t, response, http.StatusOK)
			var projects []hostedProjectView
			decodeHubResponse(t, response, &projects)
			if len(projects) != 30 {
				t.Fatalf("project count=%d, want 30", len(projects))
			}
			projectLatencies[index] = time.Since(started)
		}
		slices.Sort(projectLatencies)
		projectP95 := projectLatencies[len(projectLatencies)*95/100]
		t.Logf("projects=30 runners=10 requests=60 project_list_p95=%s", projectP95)
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		start := make(chan struct{})
		var readers sync.WaitGroup
		var mu sync.Mutex
		var failures []string
		reads := 0
		for index := range 20 {
			readers.Go(func() {
				<-start
				path := browserHostedOrganizationBase + "/activity?project=" + f.project
				if index%2 == 0 {
					path = browserHostedOrganizationBase + "/reports?project=" + f.project + "&range=24h"
				}
				for ctx.Err() == nil {
					response := f.page(t, "owner", path)
					mu.Lock()
					reads++
					if response.Code != http.StatusOK {
						failures = append(failures, fmt.Sprintf("%s: %d %s", path, response.Code, response.Body.String()))
					}
					mu.Unlock()
				}
			})
		}
		close(start)
		var heartbeatLatencies []time.Duration
		for index := 0; ctx.Err() == nil; index++ {
			runner := runners[index%len(runners)]
			started := time.Now()
			response := performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/projects/"+f.project+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"capacity": 1, "version": "test"})
			if response.Code != http.StatusOK {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("heartbeat: %d %s", response.Code, response.Body.String()))
				mu.Unlock()
			}
			heartbeatLatencies = append(heartbeatLatencies, time.Since(started))
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		stop()
		readers.Wait()
		f.service.tokenUseWork.Wait()
		if len(heartbeatLatencies) == 0 || reads < 20 {
			t.Fatalf("insufficient load: heartbeats=%d reads=%d", len(heartbeatLatencies), reads)
		}
		if len(failures) > 0 {
			t.Fatalf("requests failed: %v; logs: %s", failures[:min(5, len(failures))], logs.String())
		}
		slices.Sort(heartbeatLatencies)
		heartbeatP95 := heartbeatLatencies[len(heartbeatLatencies)*95/100]
		stats := f.service.database.reader.Stats()
		t.Logf("concurrent_reads=20 reads=%d heartbeats=%d heartbeat_p95=%s reader_max=%d reader_open=%d reader_wait_count=%d reader_wait_duration=%s", reads, len(heartbeatLatencies), heartbeatP95, stats.MaxOpenConnections, stats.OpenConnections, stats.WaitCount, stats.WaitDuration)
		return latencySample{projects: projectP95, heartbeat: heartbeatP95}
	}

	var baseline, candidate latencySample
	t.Run("single writer baseline", func(t *testing.T) {
		f.service.database.reader = f.service.database.db
		baseline = measure(t)
	})
	t.Run("reader pool", func(t *testing.T) {
		f.service.database.reader = reader
		candidate = measure(t)
	})
	t.Logf("same_fixture_comparison project_list_p95 baseline=%s candidate=%s heartbeat_p95 baseline=%s candidate=%s", baseline.projects, candidate.projects, baseline.heartbeat, candidate.heartbeat)
	if candidate.projects >= 200*time.Millisecond {
		t.Errorf("project list p95=%s, want <200ms (same-run writer baseline=%s)", candidate.projects, baseline.projects)
	}
	if candidate.heartbeat >= 100*time.Millisecond {
		t.Errorf("heartbeat p95=%s, want <100ms (same-run writer baseline=%s)", candidate.heartbeat, baseline.heartbeat)
	}
}
