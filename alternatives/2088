package hubserver

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestNativeEventWriterBudget(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			store := &memoryRunDetails{}
			var uploaded atomic.Int64
			store.afterStore = func(attachment.ArchiveReference) { uploaded.Add(1) }
			f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
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
			for _, name := range []string{"start", "batch", "replay", "landing", "maximum batch", "finish", "finish replay"} {
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
						event.Data.Handoff.HeadSHA = strings.Repeat("b", 40)
					case "finish":
						event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish", 13, "succeeded"
						event.Data.Handoff, event.PriorEvents = nil, nil
						reference := fmt.Sprintf("%s#%d", f.project.ID, dependency.Number)
						event.Data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: reference, Owner: workpad.BlockerOwnerOrchestrator, Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: reference, States: []string{"open"}}}}}
						if outcome != "succeeded" {
							event.Data.Outcome, event.Data.Disposition = outcome, nil
							result := independentRepairReview(tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)}}, now).Validator.Commands[0]
							result.Stage, result.TreeSHA, result.DurationNS, result.StartedAt = gate.StageSourceFinalization, strings.Repeat("c", 40), int64(time.Second), now.Add(-time.Second)
							event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now, Validation: &result}
						}
					case "finish replay":
						event.IdempotencyKey = "finish-replayed"
					}
					f.service.tokenUseWork.Wait()
					uploadsBefore := uploaded.Load()
					var statements, transactionStatements, maximumStatements int
					var transactionEntered atomic.Bool
					recordUpload := store.afterStore
					store.afterStore = func(ref attachment.ArchiveReference) {
						recordUpload(ref)
						if !transactionEntered.Load() {
							t.Error("run event uploaded content before its writer transaction")
						}
					}
					defer func() { store.afterStore = recordUpload }()
					timing := &requestTiming{
						writerTrace: func(_ string, _ []driver.NamedValue) {
							statements++
							transactionStatements++
						},
						writerTransaction: func(active bool) {
							if active {
								transactionEntered.Store(true)
								transactionStatements = 0
							} else {
								maximumStatements = max(maximumStatements, transactionStatements)
							}
						},
					}
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
					t.Logf("writer_txs=%d writer_statements=%d maximum_transaction_statements=%d", writer.txs.Load(), statements, maximumStatements)
					bound := 100 + 55*len(event.PriorEvents)
					if name == "finish" && outcome == "failed" {
						bound += 80
					}
					if event.Data.Disposition != nil {
						bound += 40 * len(event.Data.Disposition.BlockerEvidence)
					}
					uploads := uploaded.Load() - uploadsBefore
					wantTransactions := int64(2)
					if name == "replay" || name == "finish replay" {
						wantTransactions = 1
					}
					if writer.txs.Load() != wantTransactions || statements > bound {
						t.Errorf("writer_txs=%d (want %d for %d uploads), writer_statements=%d (bound %d)", writer.txs.Load(), wantTransactions, uploads, statements, bound)
					}
					var pending int
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM attachments WHERE pending_content IS NOT NULL)+(SELECT count(*) FROM change_content_objects WHERE pending_content IS NOT NULL)`).Scan(&pending); err != nil || pending != 0 {
						t.Fatalf("unpublished content=%d: %v", pending, err)
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
	var uploaded atomic.Int64
	start := make(chan struct{})
	preparing := make(chan struct{}, len(requests))
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	store.afterStore = func(attachment.ArchiveReference) {
		uploaded.Add(1)
		select {
		case <-release:
			return
		default:
		}
		preparing <- struct{}{}
		<-release
	}
	responses := make([]*httptest.ResponseRecorder, len(requests))
	requestErrors := make([]error, len(requests))
	completed := make(chan int, len(requests))
	var ready, finished sync.WaitGroup
	t.Cleanup(func() {
		unblock()
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
			t.Fatal("requests did not upload content concurrently after commit")
		}
	}
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
		transactions += writer.txs.Load()
	}
	t.Logf("runners=%d writer_txs=%d", len(requests), transactions)
	wantTransactions := int64(2 * len(requests))
	if transactions != wantTransactions {
		t.Fatalf("writer_txs=%d, want %d for %d requests and %d uploads", transactions, wantTransactions, len(requests), uploaded.Load())
	}
}
