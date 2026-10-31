package hubserver

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeEventWriterBudget(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	item := f.create(t, "Event writer budget")
	dependency := f.create(t, "Event prerequisite")
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
	f.service.database.hostedOrganization = f.project.OrganizationID
	hostedTestPlans(t, f.service, map[string]int64{"concurrent_work": 100, "registered_runners": 100, "connected_runners": 100})
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
				reference := fmt.Sprintf("%s#%d", f.project.ID, dependency.Number)
				event.Data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: reference, Owner: workpad.BlockerOwnerOrchestrator, Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: reference, States: []string{"open"}}}}}
			}
			f.service.tokenUseWork.Wait()
			statements := 0
			timing := &requestTiming{writerTrace: func(_ string, _ []driver.NamedValue) { statements++ }}
			request := newHubAPIRequest(t, http.MethodPost, path, worker, event)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			requireNativeStatus(t, response, http.StatusOK)
			var inline int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT
				(SELECT count(*) FROM native_attempts WHERE id=? AND (payload_object_key='' OR data_json<>'{}')) +
				(SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type LIKE 'run.%' AND (payload_version<>1 OR payload_object_key='' OR data_json<>'{}'))`, event.Data.AttemptID, item.WorkItemID).Scan(&inline); err != nil {
				t.Fatal(err)
			}
			if inline != 0 {
				t.Fatalf("%d run payloads lack immediate object references", inline)
			}
			if writer == nil {
				t.Fatal("missing writer timing")
			}
			t.Logf("writer_txs=%d writer_statements=%d", writer.txs.Load(), statements)
			bound := 90 + 55*len(event.PriorEvents)
			if event.Data.Disposition != nil {
				bound += 40 * len(event.Data.Disposition.BlockerEvidence)
			}
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

func TestNativeEventChangedPayloadPreparesAfterRollback(t *testing.T) {
	t.Parallel()
	for _, uploadFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("upload_fails=%t", uploadFails), func(t *testing.T) {
			store := &memoryRunDetails{}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store})
			f.start(t)
			var writing, rolledBack, reprepared atomic.Bool
			changed := false
			store.afterStore = func(attachment.ArchiveReference) {
				if writing.Load() {
					t.Error("payload uploaded inside the writer transaction")
				}
				if rolledBack.Load() {
					reprepared.Store(true)
				}
				if !changed {
					changed = true
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", f.issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			}
			timing := &requestTiming{writerTransaction: func(active bool) {
				writing.Store(active)
				if !active {
					rolledBack.Store(true)
					if uploadFails {
						store.mu.Lock()
						store.fail = true
						store.mu.Unlock()
					}
				}
			}}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "changed-finish"}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
			request := newHubAPIRequest(t, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			status, sequence := http.StatusOK, 2
			if uploadFails {
				status, sequence = http.StatusServiceUnavailable, 1
			} else if !reprepared.Load() {
				t.Fatal("changed finish payload was not prepared after rollback")
			}
			requireNativeStatus(t, response, status)
			var gotSequence, inline int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence,payload_object_key='' OR data_json<>'{}' FROM native_attempts WHERE id=?", f.attempt).Scan(&gotSequence, &inline); err != nil {
				t.Fatal(err)
			}
			if gotSequence != sequence || inline != 0 {
				t.Fatalf("sequence=%d inline=%d, want %d/0", gotSequence, inline, sequence)
			}
			var finished, referenced int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*),coalesce(sum(payload_version=1 AND payload_object_key<>'' AND data_json='{}'),0) FROM collaboration_events WHERE work_item_id=? AND type='run.finished'", f.issue.WorkItemID).Scan(&finished, &referenced); err != nil {
				t.Fatal(err)
			}
			want := 1
			if uploadFails {
				want = 0
			}
			if finished != want || referenced != want {
				t.Fatalf("finish events=%d referenced=%d, want %d/%d", finished, referenced, want, want)
			}
		})
	}
}

func TestNativeEventConcurrentWriterBudget(t *testing.T) {
	now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	store := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
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
	f.service.database.hostedOrganization = f.project.OrganizationID
	hostedTestPlans(t, f.service, map[string]int64{"concurrent_work": 100, "registered_runners": 100, "connected_runners": 100})
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
	preparing := make(chan struct{}, len(requests))
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	store.afterStore = func(attachment.ArchiveReference) {
		select {
		case <-release:
			return
		default:
		}
		preparing <- struct{}{}
		<-release
	}
	connection, err := f.service.database.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	responses := make([]*httptest.ResponseRecorder, len(requests))
	requestErrors := make([]error, len(requests))
	completed := make(chan int, len(requests))
	connectionClosed := false
	var ready, finished sync.WaitGroup
	t.Cleanup(func() {
		unblock()
		if !connectionClosed {
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		}
		finished.Wait()
	})
	ready.Add(len(requests))
	for i, request := range requests {
		finished.Go(func() {
			ready.Done()
			<-start
			ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
			defer cancel()
			responses[i] = httptest.NewRecorder()
			f.service.Handler().ServeHTTP(responses[i], request.WithContext(ctx))
			requestErrors[i] = ctx.Err()
			completed <- i
		})
	}
	ready.Wait()
	close(start)
	for range requests {
		select {
		case <-preparing:
		case i := <-completed:
			t.Fatalf("request %d ended before all requests prepared: status=%d error=%v", i, responses[i].Code, requestErrors[i])
		case <-t.Context().Done():
			t.Fatal("requests did not prepare content outside the occupied writer")
		}
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	connectionClosed = true
	unblock()
	finished.Wait()
	for i, response := range responses {
		requireNativeStatus(t, response, http.StatusOK)
		if requestErrors[i] != nil {
			t.Errorf("request %d cancelled during writer wait: %v", i, requestErrors[i])
		}
	}
	if len(writers) != len(requests) {
		t.Fatalf("writer timings=%d, want %d", len(writers), len(requests))
	}
	var transactions int64
	for _, writer := range writers {
		if writer.txs.Load() != 1 {
			t.Errorf("request writer_txs=%d, want 1", writer.txs.Load())
		}
		transactions += writer.txs.Load()
	}
	t.Logf("runners=%d writer_txs=%d", len(requests), transactions)
	if transactions != int64(len(requests)) {
		t.Fatalf("writer_txs=%d, want %d", transactions, len(requests))
	}
}
