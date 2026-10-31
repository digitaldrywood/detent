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
	grantAppRunners(t, f)
	runners := make([]appTestRunner, 10)
	for index := range runners {
		runners[index] = enrollAppRunner(t, f, fmt.Sprintf("Load runner %d", index), "test")
	}
	warmReaderPools(t, f.service.database)
	projectListP95 := func(want int) time.Duration {
		latencies := make([]time.Duration, 60)
		for index := range latencies {
			started := time.Now()
			response := f.page(t, "owner", browserHostedOrganizationBase+"/projects")
			requireNativeStatus(t, response, http.StatusOK)
			var projects []hostedProjectView
			decodeHubResponse(t, response, &projects)
			if len(projects) != want {
				t.Fatalf("project count=%d, want %d", len(projects), want)
			}
			latencies[index] = time.Since(started)
		}
		slices.Sort(latencies)
		return latencies[len(latencies)*95/100]
	}
	fixtureProjectP95 := projectListP95(2)
	for index := 2; index < 30; index++ {
		f.createProject(t, fmt.Sprintf("Load project %d", index))
	}
	projectP95 := projectListP95(30)
	t.Logf("runners=10 project_list_p95 projects=2:%s projects=30:%s target=<200ms", fixtureProjectP95, projectP95)

	heartbeatP95 := func(readers int) (time.Duration, int) {
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		start := make(chan struct{})
		var group sync.WaitGroup
		var mu sync.Mutex
		var failures []string
		reads := 0
		for index := range readers {
			group.Go(func() {
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
		var latencies []time.Duration
		for index := 0; ctx.Err() == nil; index++ {
			runner := runners[index%len(runners)]
			started := time.Now()
			response := performHubAPIRequest(t, f.service, http.MethodPost, browserHostedOrganizationBase+"/projects/"+f.project+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"capacity": 1, "version": "test"})
			if response.Code != http.StatusOK {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("heartbeat: %d %s", response.Code, response.Body.String()))
				mu.Unlock()
			}
			latencies = append(latencies, time.Since(started))
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		stop()
		group.Wait()
		if len(latencies) == 0 || reads < readers {
			t.Fatalf("insufficient load: heartbeats=%d reads=%d", len(latencies), reads)
		}
		if len(failures) > 0 {
			t.Fatalf("requests failed: %v; logs: %s", failures[:min(5, len(failures))], logs.String())
		}
		slices.Sort(latencies)
		return latencies[len(latencies)*95/100], reads
	}
	idleP95, _ := heartbeatP95(0)
	loadedP95, reads := heartbeatP95(20)
	stats := f.service.database.reader.Stats()
	t.Logf("heartbeat_p95 idle=%s concurrent_reads=20:%s reads=%d target=<100ms reader_max=%d reader_open=%d reader_wait_count=%d reader_wait_duration=%s", idleP95, loadedP95, reads, stats.MaxOpenConnections, stats.OpenConnections, stats.WaitCount, stats.WaitDuration)
}
