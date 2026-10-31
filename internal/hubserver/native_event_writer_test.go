package hubserver

import (
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeEventWriterBudget(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	item := f.create(t, "Event writer budget")
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	worker := runner.redemption.Credential
	claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "event-budget", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimed, &lease)
	path := f.base + "/work-items/" + string(item.WorkItemID) + "/events"
	event := nativeStartedEvent(lease)
	f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
	metrics, err := openRequestMetrics(t.Context(), filepath.Join(t.TempDir(), "requests.sqlite"), f.project.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.requestMetrics = metrics
	f.service.echo.Use(f.service.hostedBoundary)
	var writer *writerTiming
	f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			writer = writerTimingFrom(c.Request().Context())
			return err
		}
	})
	for _, name := range []string{"start", "batch", "replay", "landing", "maximum batch", "finish"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "batch":
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "prior", 2
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}
				prior := event
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 3
				event.Data.Handoff = nativeTestCheckpoint()
				event.PriorEvents = []tracker.NativeRunEvent{prior}
			case "landing":
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "landing", 4
				event.Data.Handoff, event.PriorEvents = nil, nil
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: now, Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", ObservedAt: now}}
			case "maximum batch":
				event.PriorEvents = nil
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}
				for i := range 7 {
					prior := event
					prior.PriorEvents = nil
					prior.IdempotencyKey, prior.Data.Sequence = fmt.Sprintf("maximum-prior-%d", i), int64(5+i)
					event.PriorEvents = append(event.PriorEvents, prior)
				}
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "maximum-checkpoint", 12
				event.Data.Handoff = nativeTestCheckpoint()
			case "finish":
				event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish", 13, "succeeded"
				event.Data.Handoff, event.PriorEvents = nil, nil
			}
			f.service.tokenUseWork.Wait()
			statements := 0
			timing := &requestTiming{writerTrace: func(_ string, _ []driver.NamedValue) { statements++ }}
			request := newHubAPIRequest(t, http.MethodPost, path, worker, event)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			requireNativeStatus(t, response, http.StatusOK)
			if writer == nil {
				t.Fatal("missing writer timing")
			}
			t.Logf("writer_txs=%d writer_statements=%d", writer.txs.Load(), statements)
			bound := 70 + 55*len(event.PriorEvents)
			if writer.txs.Load() != 1 || statements > bound {
				t.Errorf("writer_txs=%d (want 1), writer_statements=%d (bound %d)", writer.txs.Load(), statements, bound)
			}
		})
	}
	var sequence, events int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempt_events WHERE attempt_id=?", event.Data.AttemptID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if sequence != 13 || events != 13 {
		t.Fatalf("sequence=%d events=%d, want 13/13", sequence, events)
	}
}

func TestNativeEventConcurrentWriterBudget(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	requests := make([]*http.Request, 20)
	for i := range requests {
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		runner.enroll(t)
		item := f.create(t, fmt.Sprintf("Concurrent event %d", i))
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: runner.binding.MachineID, SessionID: fmt.Sprintf("event-session-%d", i), TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
		requireNativeStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, response, &lease)
		event := nativeStartedEvent(lease)
		event.IdempotencyKey = fmt.Sprintf("event-%d", i)
		requests[i] = newHubAPIRequest(t, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", runner.redemption.Credential, event)
	}
	f.service.tokenUseWork.Wait()
	f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
	metrics, err := openRequestMetrics(t.Context(), filepath.Join(t.TempDir(), "requests.sqlite"), f.project.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.requestMetrics = metrics
	f.service.echo.Use(f.service.hostedBoundary)
	var writers []*writerTiming
	var writerMu sync.Mutex
	f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			if timing := writerTimingFrom(c.Request().Context()); timing != nil {
				writerMu.Lock()
				writers = append(writers, timing)
				writerMu.Unlock()
			}
			return err
		}
	})
	start := make(chan struct{})
	responses := make([]*httptest.ResponseRecorder, len(requests))
	var ready, finished sync.WaitGroup
	ready.Add(len(requests))
	for i, request := range requests {
		finished.Go(func() {
			ready.Done()
			<-start
			responses[i] = httptest.NewRecorder()
			f.service.Handler().ServeHTTP(responses[i], request)
		})
	}
	ready.Wait()
	close(start)
	finished.Wait()
	for _, response := range responses {
		requireNativeStatus(t, response, http.StatusOK)
	}
	if len(writers) != len(requests) {
		t.Fatalf("writer timings=%d, want %d", len(writers), len(requests))
	}
	var transactions int64
	for _, writer := range writers {
		transactions += writer.txs.Load()
	}
	t.Logf("runners=%d writer_txs=%d", len(requests), transactions)
	if transactions != int64(len(requests)) {
		t.Fatalf("writer_txs=%d, want %d", transactions, len(requests))
	}
}
