package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type nativeEventWriterProbeKey struct{}

type nativeEventWriterProbe struct {
	mu           sync.Mutex
	active       map[*pgx.Conn]int
	transactions int64
	statements   int
	maximum      int
	objectIO     int
}

func (p *nativeEventWriterProbe) trace(conn *pgx.Conn, query string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	statement := strings.ToUpper(strings.TrimSpace(query))
	if strings.HasPrefix(statement, "BEGIN") || strings.HasPrefix(statement, "START TRANSACTION") {
		if !strings.Contains(statement, "READ ONLY") {
			if p.active == nil {
				p.active = map[*pgx.Conn]int{}
			}
			p.active[conn] = 0
			p.transactions++
		}
	}
	if count, ok := p.active[conn]; ok {
		p.statements++
		p.active[conn] = count + 1
		p.maximum = max(p.maximum, count+1)
	}
}

func (p *nativeEventWriterProbe) finish(conn *pgx.Conn) {
	if conn.PgConn().TxStatus() == 'I' {
		p.mu.Lock()
		delete(p.active, conn)
		p.mu.Unlock()
	}
}

func observeNativeEventObjectIO(ctx context.Context) {
	if probe, ok := ctx.Value(nativeEventWriterProbeKey{}).(*nativeEventWriterProbe); ok {
		probe.mu.Lock()
		if len(probe.active) != 0 {
			probe.objectIO++
		}
		probe.mu.Unlock()
	}
}

type nativeEventWriterTrace struct{}

func (nativeEventWriterTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if probe, ok := ctx.Value(nativeEventWriterProbeKey{}).(*nativeEventWriterProbe); ok {
		probe.trace(conn, data.SQL)
	}
	return ctx
}

func (nativeEventWriterTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if probe, ok := ctx.Value(nativeEventWriterProbeKey{}).(*nativeEventWriterProbe); ok {
		probe.finish(conn)
	}
}

func (nativeEventWriterTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if probe, ok := ctx.Value(nativeEventWriterProbeKey{}).(*nativeEventWriterProbe); ok {
		for _, query := range data.Batch.QueuedQueries {
			probe.trace(conn, query.SQL)
		}
	}
	return ctx
}

func (nativeEventWriterTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (nativeEventWriterTrace) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	if probe, ok := ctx.Value(nativeEventWriterProbeKey{}).(*nativeEventWriterProbe); ok {
		probe.finish(conn)
	}
}

type nativeEventWriterDetails struct{ *memoryRunDetails }

func (s nativeEventWriterDetails) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	observeNativeEventObjectIO(ctx)
	return s.memoryRunDetails.StoreDetail(ctx, org, raw)
}

func (s nativeEventWriterDetails) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	observeNativeEventObjectIO(ctx)
	return s.memoryRunDetails.ReadDetail(ctx, org, ref)
}

func newNativeEventWriterFixture(t *testing.T, store *memoryRunDetails, now time.Time) nativeFixture {
	t.Helper()
	repository, _ := projectRecordPostgresWithClock(t, 4, func() time.Time { return now }, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, nativeEventWriterTrace{})
	})
	service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: nativeEventWriterDetails{store}, now: func() time.Time { return now }, SecretKeys: secretTestKeys(t, "1", "1")})
	primary := tracker.OrganizationID("org_first")
	for _, org := range []tracker.OrganizationID{primary + "_second", primary} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-")})
		key := bytes.Repeat([]byte{0x5a}, 32)
		envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: hubapi.NewID("hub"), CursorKey: key}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.database.hostedOrganization = primary
	service.localOrganization = string(primary)
	service.requestMetrics.Organization = primary
	return newNativeFixture(t, service, primary, "event-writer")
}

func TestNativeEventWriterBudget(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			store := &memoryRunDetails{}
			var uploaded atomic.Int64
			store.afterStore = func(attachment.ArchiveReference) { uploaded.Add(1) }
			f := newNativeEventWriterFixture(t, store, now)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			item := f.create(t, "Event writer budget")
			dependency := f.create(t, "Event prerequisite")
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			runner.enroll(t)
			reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, runner.redemption.DisplayName, runner.redemption.BackendIsolation, runner.redemption.Capacity)
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
			metrics, err := newPostgresTestMetrics(t, f.project.OrganizationID)
			if err != nil {
				t.Fatal(err)
			}
			f.service.requestMetrics = metrics
			t.Cleanup(func() {
				if err := f.service.Close(); err != nil {
					t.Error(err)
				}
			})
			f.service.echo.Use(f.service.hostedBoundary)
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
					probe := &nativeEventWriterProbe{}
					request := newHubAPIRequest(t, http.MethodPost, path, worker, event)
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), nativeEventWriterProbeKey{}, probe)))
					requireNativeStatus(t, response, http.StatusOK)
					var attempt leasestore.Attempt
					var history []eventstore.Event
					refs := []attachment.ArchiveReference{}
					var inline int
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						attempt, err = r.Attempt(t.Context(), event.Data.AttemptID)
						if err != nil {
							return err
						}
						history, err = r.(eventstore.AttemptEventPageReader).AttemptEvents(t.Context(), f.project.ID, item.WorkItemID, attempt.ID, 0, 100)
						if err != nil {
							return err
						}
						keys := []string{attempt.PayloadObjectKey}
						for _, row := range history {
							if row.PayloadFormatVersion != 2 {
								inline++
							}
							keys = append(keys, row.PayloadObjectKey)
						}
						for _, key := range keys {
							if key == "" {
								inline++
								continue
							}
							object, err := r.(hubrecords.AttemptArchiveReader).ArchiveObjectByKey(t.Context(), key)
							if err != nil {
								return err
							}
							if object.Reference.Validate(string(f.project.OrganizationID)) != nil || object.Reference.Key != key {
								inline++
							}
							refs = append(refs, object.Reference)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if inline != 0 {
						t.Fatalf("%d run payloads lack immediate object references", inline)
					}
					status := "running"
					if event.Type == "run.finished" {
						status = event.Data.Outcome
					}
					if attempt.Status != status {
						t.Fatalf("stored attempt status=%s, want%s", attempt.Status, status)
					}
					probe.mu.Lock()
					transactions, statements, maximumStatements, objectIO, active := probe.transactions, probe.statements, probe.maximum, probe.objectIO, len(probe.active)
					probe.mu.Unlock()
					if transactions == 0 || statements == 0 {
						t.Fatal("missing writer timing")
					}
					if objectIO != 0 || active != 0 {
						t.Fatalf("run object I/O inside write transaction=%d active_writers=%d", objectIO, active)
					}
					t.Logf("writer_txs=%d writer_statements=%d maximum_transaction_statements=%d", transactions, statements, maximumStatements)
					bound := 100 + 55*len(event.PriorEvents)
					if name == "finish" && outcome == "failed" {
						bound += 80
					}
					if event.Data.Disposition != nil {
						bound += 40 * len(event.Data.Disposition.BlockerEvidence)
					}
					uploads := uploaded.Load() - uploadsBefore
					wantTransactions := int64(1)
					if transactions != wantTransactions || statements > bound {
						t.Errorf("writer_txs=%d (want %d for %d uploads), writer_statements=%d (bound %d)", transactions, wantTransactions, uploads, statements, bound)
					}
					keys := []string{}
					rows, err := usagePostgresAdmin(t, f.service).QueryContext(t.Context(), "SELECT object_key FROM archive_objects WHERE organization_id=$1 ORDER BY object_key", f.project.OrganizationID)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var key string
						if err := rows.Scan(&key); err != nil {
							rows.Close()
							t.Fatal(err)
						}
						keys = append(keys, key)
					}
					if err := rows.Err(); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					if err := rows.Close(); err != nil {
						t.Fatal(err)
					}
					allRefs := []attachment.ArchiveReference{}
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						for _, key := range keys {
							object, err := r.(hubrecords.AttemptArchiveReader).ArchiveObjectByKey(t.Context(), key)
							if err != nil {
								return err
							}
							allRefs = append(allRefs, object.Reference)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					var pending int
					var publicationErr error
					for _, ref := range allRefs {
						if _, err := store.ReadDetail(t.Context(), string(f.project.OrganizationID), ref); err != nil {
							pending++
							publicationErr = err
						}
					}
					if pending != 0 {
						t.Fatalf("unpublished content=%d: %v", pending, publicationErr)
					}
					for index, row := range history {
						value, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), row)
						if err != nil {
							t.Fatal(err)
						}
						if run := value.Data.Run; run == nil || run.AttemptID != attempt.ID || run.RunID != attempt.RunID || run.Sequence != int64(index+1) {
							t.Fatalf("event history lost committed run order: %+v", value)
						}
					}
					if len(history) != int(attempt.Sequence) {
						t.Fatalf("run history=%d, want sequence%d", len(history), attempt.Sequence)
					}
					var run tracker.NativeRunData
					raw, err := store.ReadDetail(t.Context(), string(f.project.OrganizationID), refs[0])
					if err == nil {
						err = json.Unmarshal(raw, &run)
					}
					if err != nil || run.Sequence != attempt.Sequence || run.AttemptID != attempt.ID || run.RunID != attempt.RunID || run.LeaseID != lease.ID || run.FencingToken != lease.FencingToken {
						t.Fatalf("attempt object lost current committed identity: %+v err=%v", run, err)
					}
				})
			}
			var sequence, events int
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				attempt, err := r.Attempt(t.Context(), event.Data.AttemptID)
				sequence = int(attempt.Sequence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2", f.project.OrganizationID, event.Data.AttemptID).Scan(&events); err != nil {
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
	f := newNativeEventWriterFixture(t, store, now)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	requests := make([]*http.Request, 20)
	for i := range requests {
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		runner.enroll(t)
		reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, runner.redemption.DisplayName, runner.redemption.BackendIsolation, runner.redemption.Capacity)
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
	metrics, err := newPostgresTestMetrics(t, f.project.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.requestMetrics = metrics
	t.Cleanup(func() {
		if err := f.service.Close(); err != nil {
			t.Error(err)
		}
	})
	f.service.echo.Use(f.service.hostedBoundary)
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
	probes := make([]*nativeEventWriterProbe, len(requests))
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
		probes[i] = &nativeEventWriterProbe{}
		finished.Go(func() {
			ready.Done()
			<-start
			ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
			defer cancel()
			responses[i] = httptest.NewRecorder()
			f.service.Handler().ServeHTTP(responses[i], request.WithContext(context.WithValue(ctx, nativeEventWriterProbeKey{}, probes[i])))
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
			t.Fatal("requests did not prepare content concurrently before publication")
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
	var writers []int64
	for index, probe := range probes {
		probe.mu.Lock()
		count, statements, objectIO, active := probe.transactions, probe.statements, probe.objectIO, len(probe.active)
		probe.mu.Unlock()
		if objectIO != 0 || active != 0 {
			t.Fatalf("request%d writer_txs=%d statements=%d object_io_inside_write=%d active_writers=%d", index, count, statements, objectIO, active)
		}
		if count > 0 && statements > 0 {
			writers = append(writers, count)
		}
	}
	if len(writers) != len(requests) {
		t.Fatalf("writer timings=%d, want %d", len(writers), len(requests))
	}
	var transactions int64
	for _, count := range writers {
		transactions += count
	}
	t.Logf("runners=%d writer_txs=%d", len(requests), transactions)
	wantTransactions := int64(len(requests))
	if transactions != wantTransactions {
		t.Fatalf("writer_txs=%d, want %d for %d requests and %d uploads", transactions, wantTransactions, len(requests), uploaded.Load())
	}
}
