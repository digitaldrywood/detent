package hubserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectListAndRunnerReadContention(t *testing.T) {
	t.Parallel()
	var logs hostedLogBuffer
	plans := hostedplans.CapacityPlans()
	plans.Base = hostedplans.PlanReference{ID: "scale", Version: 1}
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, func(cfg *Config) { cfg.Hosted.Plans = &plans; cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	grantAppRunners(t, f)
	runners := make([]appTestRunner, 10)
	for index := range runners {
		runners[index] = enrollAppRunner(t, f, fmt.Sprintf("Load runner %d", index), "test")
	}
	repository := &tenantReadLoadRepository{Repository: f.service.database.postgres}
	f.service.config.TenantRepository, f.service.config.AccessRepository = repository, repository
	f.service.tokenUseWork.Wait()
	warmReaderPools(t, repository.Repository, f.service.accessOrganization())
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
		var failures []string
		reads := 0
		latencies := make([]time.Duration, 0, 250)
		for index := range 250 {
			func() {
				ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
				var group sync.WaitGroup
				release := make(chan struct{})
				var releaseOnce sync.Once
				resume := func() { releaseOnce.Do(func() { close(release) }) }
				defer func() {
					resume()
					cancel()
					group.Wait()
				}()
				held := &tenantReadLoadGate{entered: make(chan struct{}), release: release}
				readResults := make(chan *httptest.ResponseRecorder, readers)
				firstResult := make(chan *httptest.ResponseRecorder, 1)
				start := make(chan struct{})
				var ready sync.WaitGroup
				for reader := range readers {
					path := browserHostedOrganizationBase + "/activity?project=" + f.project
					if reader%2 == 0 {
						path = browserHostedOrganizationBase + "/reports?project=" + f.project + "&range=24h"
					}
					request := httptest.NewRequest(http.MethodGet, f.server.URL+path, nil).WithContext(ctx)
					request.AddCookie(f.cookies["owner"])
					if reader == 0 {
						request = request.WithContext(context.WithValue(ctx, tenantReadLoadGateKey{}, held))
					}
					ready.Add(1)
					group.Go(func() {
						ready.Done()
						<-start
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, request)
						readResults <- response
						if reader == 0 {
							firstResult <- response
						}
					})
				}
				ready.Wait()
				close(start)
				if readers > 0 {
					select {
					case <-held.entered:
					case response := <-firstResult:
						t.Fatalf("read bypassed PostgreSQL contention boundary: status=%d body=%s", response.Code, response.Body.String())
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if repository.Stats().AcquiredConns() == 0 {
						t.Fatal("read contention did not hold a physical PostgreSQL connection")
					}
				}
				runnerIndex := index % len(runners)
				runner := runners[runnerIndex]
				request := newHubAPIRequest(t, http.MethodPost, browserHostedOrganizationBase+"/projects/"+f.project+"/machines/"+string(runner.MachineID)+"/heartbeat", runner.Credential, map[string]any{"display_name": fmt.Sprintf("Load runner %d", runnerIndex), "capacity": 1, "version": "test"}).WithContext(ctx)
				heartbeat := &tenantReadLoadGate{entered: make(chan struct{})}
				request = request.WithContext(context.WithValue(ctx, tenantReadLoadGateKey{}, heartbeat))
				heartbeatResult := make(chan *httptest.ResponseRecorder, 1)
				started := time.Now()
				group.Go(func() {
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					heartbeatResult <- response
				})
				var response *httptest.ResponseRecorder
				select {
				case <-heartbeat.entered:
				case response = <-heartbeatResult:
					select {
					case <-heartbeat.entered:
					default:
						t.Fatalf("heartbeat bypassed current authority owner: status=%d body=%s", response.Code, response.Body.String())
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				resume()
				if response == nil {
					response = <-heartbeatResult
				}
				latencies = append(latencies, time.Since(started))
				if response.Code != http.StatusOK {
					failures = append(failures, fmt.Sprintf("heartbeat: %d %s", response.Code, response.Body.String()))
				}
				for range readers {
					response := <-readResults
					reads++
					if response.Code != http.StatusOK {
						failures = append(failures, fmt.Sprintf("read: %d %s", response.Code, response.Body.String()))
					}
				}
			}()
		}
		if len(latencies) != 250 || reads != readers*250 {
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
	stats := repository.Stats()
	t.Logf("heartbeat_p95 idle=%s concurrent_reads=20:%s reads=%d target=<100ms reader_max=%d reader_open=%d reader_wait_count=%d reader_wait_duration=%s", idleP95, loadedP95, reads, stats.MaxConns(), stats.TotalConns(), stats.EmptyAcquireCount(), stats.AcquireDuration())
}

type tenantReadLoadGateKey struct{}

type tenantReadLoadGate struct {
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
}

func (g *tenantReadLoadGate) arrive(ctx context.Context) {
	g.once.Do(func() {
		close(g.entered)
		if g.release != nil {
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		}
	})
}

type tenantReadLoadRepository struct{ *postgres.Repository }

func (r *tenantReadLoadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		if gate, ok := ctx.Value(tenantReadLoadGateKey{}).(*tenantReadLoadGate); ok && gate.release != nil {
			gate.arrive(ctx)
		}
		return read(reader)
	})
}

func (r *tenantReadLoadRepository) ReadRunnerAuthority(ctx context.Context, identity tenantauth.RequestIdentity, now func() time.Time) (tenantauth.Scope, int, error) {
	if gate, ok := ctx.Value(tenantReadLoadGateKey{}).(*tenantReadLoadGate); ok && gate.release == nil {
		gate.arrive(ctx)
	}
	return r.Repository.ReadRunnerAuthority(ctx, identity, now)
}
