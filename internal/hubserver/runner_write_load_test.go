package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// writeLoadSession is one claimed attempt streaming into its linked
// conversation, the shape a running agent session puts on the Hub.
type writeLoadSession struct {
	identity     map[string]any
	conversation string
	item         string
}

type writeLoadFixture struct {
	nativeFixture
	worker   string
	now      time.Time
	runners  []runnerFixture
	sessions []writeLoadSession
}

// newWriteLoadFixture builds a hosted tenant with the given history, runner
// count and streaming sessions.
func newWriteLoadFixture(t *testing.T, history, runners, sessions int) *writeLoadFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository, _ := projectRecordPostgresWithClock(t, 4, func() time.Time { return now }, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, writeLoadTrace{})
	})
	config := Config{Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour}, Clock: func() time.Time { return now }, SecretKeys: secretTestKeys(t, "1", "1")}
	service := openLocalPostgresHubTestService(t, repository, config)
	service.registerConversationRoutes(service.echo)
	primary := tracker.OrganizationID("org_first")
	for _, organization := range []tracker.OrganizationID{primary, primary + "_second"} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
		key := bytes.Repeat([]byte{0x5a}, 32)
		envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: key}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.database.hostedOrganization, service.localOrganization, service.requestMetrics.Organization = primary, string(primary), primary
	f := newNativeFixture(t, service, primary, "write-load")
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	d := f.service.database
	d.hostedOrganization = f.project.OrganizationID
	plans := hostedplans.CapacityPlans()
	plans.Base = hostedplans.PlanReference{ID: "scale", Version: 1}
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
		t.Fatal(err)
	}
	streamRunner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events)
	streamRunner.redemption.Capacity = max(sessions, 1)
	streamRunner.redemption.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
	streamRunner.enroll(t)
	fixture := &writeLoadFixture{nativeFixture: f, worker: streamRunner.redemption.Credential, now: now}
	capacity := max(sessions, 1)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(streamRunner.binding.MachineID)+"/heartbeat", fixture.worker,
		map[string]any{"display_name": "stream runner", "capacity": capacity, "version": "test", "backend_isolation": streamRunner.redemption.BackendIsolation,
			"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: now}}), http.StatusOK)
	seedWriteLoadHistory(t, f, history)
	for range runners {
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events)
		r.enroll(t)
		fixture.runners = append(fixture.runners, r)
	}
	owner := conversationFixturePrincipal(t, f, "write-load-owner")
	for index := range sessions {
		issue := f.create(t, fmt.Sprintf("write-load-%d", index))
		claim := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", fixture.worker, tracker.NativeClaim{PolicyID: policy.ID, WorkItemID: issue.WorkItemID, MachineID: streamRunner.binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
		requireNativeStatus(t, claim, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, claim, &lease)
		attempt, run := hubapi.NewID("attempt"), hubapi.NewID("run")
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: run, AttemptID: attempt, PolicyID: policy.ID}}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", fixture.worker, event), http.StatusOK)
		record := conversationRecord{
			ID: conversation.NewConversationID(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
			OwnerPrincipalID: owner.ID, OwnerSubject: "owner@example.test", Title: "load", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
			WorkItemID: string(issue.WorkItemID), LinkedAt: &now,
			Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now},
			CreatedAt: now, UpdatedAt: now,
		}
		record.Origin, record.Revision, record.Preferences = conversationOriginUser, 1, conversation.DefaultPreferences()
		if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			stored, err := w.CreateConversation(t.Context(), conversationRecordToStore(record))
			record = conversationRecordFromStore(stored)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		identity := map[string]any{"lease_id": lease.ID, "fencing_token": lease.FencingToken, "attempt_id": attempt}
		bind := map[string]any{"lease_id": lease.ID, "fencing_token": lease.FencingToken, "attempt_id": attempt, "run_id": run, "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/conversation/bind", fixture.worker, bind), http.StatusOK)
		session := writeLoadSession{identity: identity, conversation: record.ID, item: string(issue.WorkItemID)}
		requireLoadStatus(t, fixture.turnEvents(t, session, map[string]any{"type": "turn_started", "thread_id": "thread-" + record.ID, "turn_id": "turn-" + record.ID}), http.StatusAccepted)
		fixture.sessions = append(fixture.sessions, session)
	}
	return fixture
}

// seedWriteLoadHistory appends accumulated collaboration history to one
// issue, the history that grows with a tenant's age.
func seedWriteLoadHistory(t *testing.T, f nativeFixture, history int) {
	t.Helper()
	if history == 0 {
		return
	}
	issue := f.create(t, "write-load-history")
	content := eventstore.NewContent(f.service.config.RunDetails)
	now := f.service.config.Clock().UTC()
	for first := 0; first < history; first += 200 {
		batch := make([]eventstore.Event, 0, min(200, history-first))
		for range min(200, history-first) {
			value := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
				AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "issue.edited", SchemaVersion: 1, RecordedAt: now,
				Actor: tracker.Actor{Kind: "human", PrincipalID: "history"}, Data: tracker.CollaborationData{Revision: 1, ReasonDetail: strings.Repeat("history ", 64)}}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, raw)
			if err != nil {
				t.Fatal(err)
			}
			batch = append(batch, event)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			for _, event := range batch {
				if _, err := w.AppendEvent(t.Context(), event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if first == 0 {
			if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), `ANALYZE collaboration_events`); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stored int
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='issue.edited'`, f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != history {
		t.Fatalf("history events=%d, want %d", stored, history)
	}
}

func (f *writeLoadFixture) turnEvents(t *testing.T, session writeLoadSession, events ...map[string]any) *responseRecorderCode {
	t.Helper()
	body := map[string]any{}
	for key, value := range session.identity {
		body[key] = value
	}
	body["events"] = events
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+session.conversation+"/turn-events", f.worker, body)
	return &responseRecorderCode{Code: response.Code, Body: response.Body.String()}
}

type responseRecorderCode struct {
	Code int
	Body string
}

func requireLoadStatus(t *testing.T, response *responseRecorderCode, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body)
	}
}

// TestRunnerWritesStayBoundedAsTenantsGrow reproduces the 2026-10-08
// production write latency: heartbeats and streamed turn events whose cost
// grew with accumulated history and message length while they held the
// tenant's single writer.
func TestRunnerWritesStayBoundedAsTenantsGrow(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("runner write load reproduction")
	}
	const heartbeatRounds, turnBatches = 10, 40
	chunk := strings.Repeat("output ", 64)
	var measuring sync.Mutex
	for _, test := range []struct {
		name     string
		sessions int
		runners  int
	}{
		{name: "twelve sessions on two runners", sessions: 12, runners: 2},
		{name: "fifty sessions on eight runners", sessions: 50, runners: 8},
		{name: "one hundred sessions on sixteen runners", sessions: 100, runners: 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newWriteLoadFixture(t, 50000, test.runners, test.sessions)
			f.service.config.Deps().RequestTimeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
				return context.WithCancel(ctx)
			}
			measuring.Lock()
			defer measuring.Unlock()
			admin := usagePostgresAdmin(t, f.service)
			heartbeatReceipts := func() int {
				t.Helper()
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_usage_receipts WHERE organization_id=$1 AND metric='heartbeats'", f.project.OrganizationID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			receiptsBefore := heartbeatReceipts()
			type sample struct {
				route    string
				code     int
				failure  string
				work     writeLoadWorkCounts
				elapsed  time.Duration
				expected int
			}
			samples := make(chan sample, test.runners*heartbeatRounds+test.sessions*turnBatches)
			start := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(start) }) }
			var workers sync.WaitGroup
			defer func() {
				release()
				workers.Wait()
			}()
			for _, runner := range f.runners {
				workers.Go(func() {
					<-start
					for range heartbeatRounds {
						started := time.Now()
						response, work := f.measuredRequest(t, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential,
							map[string]any{"backend_isolation": runner.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []any{capacityReport(f.now)}})
						samples <- sample{route: "heartbeat", code: response.Code, failure: response.Body.String(), work: work, elapsed: time.Since(started), expected: http.StatusOK}
					}
				})
			}
			for index, session := range f.sessions {
				workers.Go(func() {
					<-start
					for range turnBatches {
						body := map[string]any{"events": []map[string]any{
							{"type": "delta", "provider_item_id": fmt.Sprintf("item-%d", index), "text": chunk},
							{"type": "delta", "provider_item_id": fmt.Sprintf("item-%d", index), "text": chunk},
						}}
						for key, value := range session.identity {
							body[key] = value
						}
						started := time.Now()
						response, work := f.measuredRequest(t, f.base+"/conversations/"+session.conversation+"/turn-events", f.worker, body)
						samples <- sample{route: "turn-events", code: response.Code, failure: response.Body.String(), work: work, elapsed: time.Since(started), expected: http.StatusAccepted}
					}
				})
			}
			release()
			workers.Wait()
			close(samples)
			counts := map[string]int{}
			worst := map[string]writeLoadWorkCounts{}
			latencies := map[string][]time.Duration{}
			for sample := range samples {
				counts[sample.route]++
				latencies[sample.route] = append(latencies[sample.route], sample.elapsed)
				if sample.code != sample.expected {
					t.Errorf("%s status=%d want %d: %s", sample.route, sample.code, sample.expected, sample.failure)
					continue
				}
				if sample.work.Commits == 0 || sample.work.ProbeErrors != 0 {
					t.Errorf("%s work probe commits=%d errors=%d", sample.route, sample.work.Commits, sample.work.ProbeErrors)
				}
				current := worst[sample.route]
				current.Statements = max(current.Statements, sample.work.Statements)
				current.Commits = max(current.Commits, sample.work.Commits)
				worst[sample.route] = current
			}
			if counts["heartbeat"] != test.runners*heartbeatRounds || counts["turn-events"] != test.sessions*turnBatches {
				t.Fatalf("requests heartbeat=%d turn-events=%d, want %d and %d", counts["heartbeat"], counts["turn-events"], test.runners*heartbeatRounds, test.sessions*turnBatches)
			}
			for _, route := range []string{"heartbeat", "turn-events"} {
				values := latencies[route]
				slices.Sort(values)
				t.Logf("sessions=%d runners=%d route=%s requests=%d max_statements=%d max_commits=%d diagnostic_p50=%s diagnostic_max=%s", test.sessions, test.runners, route, len(values), worst[route].Statements, worst[route].Commits, values[len(values)/2], values[len(values)-1])
				bound := writeLoadRequestBounds[route]
				if worst[route].Statements > bound.Statements || worst[route].Commits > bound.Commits {
					t.Errorf("%s concurrent request work=%+v exceeds the reviewed bound %+v", route, worst[route], bound)
				}
			}
			if got := heartbeatReceipts() - receiptsBefore; got != test.runners*heartbeatRounds {
				t.Errorf("durable heartbeat receipts=%d want %d", got, test.runners*heartbeatRounds)
			}
			probes := []struct {
				route    string
				path     string
				token    string
				body     map[string]any
				expected int
			}{
				{"heartbeat", f.base + "/machines/" + string(f.runners[0].binding.MachineID) + "/heartbeat", f.runners[0].redemption.Credential,
					map[string]any{"backend_isolation": f.runners[0].redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []any{capacityReport(f.now)}}, http.StatusOK},
				{"turn-events", f.base + "/conversations/" + f.sessions[0].conversation + "/turn-events", f.worker, map[string]any{"events": []map[string]any{
					{"type": "delta", "provider_item_id": "item-0", "text": chunk},
					{"type": "delta", "provider_item_id": "item-0", "text": chunk},
				}}, http.StatusAccepted},
			}
			maps.Copy(probes[1].body, f.sessions[0].identity)
			for _, probe := range probes {
				response, work := f.measuredRequestWith(t, &writeLoadWork{captureStatements: true}, probe.path, probe.token, probe.body)
				if response.Code != probe.expected || work.Commits == 0 || work.ProbeErrors != 0 {
					t.Fatalf("serial %s probe status=%d commits=%d probe_errors=%d: %s", probe.route, response.Code, work.Commits, work.ProbeErrors, response.Body.String())
				}
				t.Logf("sessions=%d runners=%d serial %s statements=%d commits=%d diagnostic_row_reads=%d diagnostic_relations=%v", test.sessions, test.runners, probe.route, work.Statements, work.Commits, work.RowReads, work.Relations)
				bound := writeLoadRequestBounds[probe.route]
				if work.Statements > bound.Statements || work.Commits > bound.Commits {
					t.Errorf("serial %s request work statements=%d commits=%d exceeds the reviewed bound %+v", probe.route, work.Statements, work.Commits, bound)
				}
				requireIndexedWriteLoadPlans(t, admin, probe.route, work.Captured)
			}
			if got := heartbeatReceipts() - receiptsBefore; got != test.runners*heartbeatRounds+1 {
				t.Errorf("durable heartbeat receipts after the serial probe=%d want %d", got, test.runners*heartbeatRounds+1)
			}
			var foreignReceipts, foreignMessages int
			if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_usage_receipts WHERE organization_id=$1),(SELECT count(*) FROM conversation_messages WHERE organization_id=$1)", f.project.OrganizationID+"_second").Scan(&foreignReceipts, &foreignMessages); err != nil {
				t.Fatal(err)
			}
			if foreignReceipts != 0 || foreignMessages != 0 {
				t.Errorf("load in %s wrote to the other tenant: receipts=%d messages=%d", f.project.OrganizationID, foreignReceipts, foreignMessages)
			}
			for _, session := range f.sessions {
				var stored int64
				if err := admin.QueryRowContext(t.Context(), "SELECT COALESCE(sum(text_bytes),0) FROM conversation_messages WHERE organization_id=$1 AND conversation_id=$2 AND role='assistant'", f.project.OrganizationID, session.conversation).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				want := int64(2 * turnBatches * len(chunk))
				if session.conversation == f.sessions[0].conversation {
					want += int64(2 * len(chunk))
				}
				if stored != want {
					t.Errorf("conversation %s stored assistant text=%d want %d", session.conversation, stored, want)
				}
			}
		})
	}
}

type writeLoadWorkCounts struct {
	Statements  int64
	RowReads    int64
	Commits     int64
	ProbeErrors int64
	Relations   map[string]int64
	Captured    []writeLoadStatement
}

var writeLoadRequestBounds = map[string]writeLoadWorkCounts{
	"heartbeat":   {Statements: 34, Commits: 4},
	"turn-events": {Statements: 89, Commits: 7},
}

type writeLoadWorkKey struct{}

type writeLoadWork struct {
	statements  atomic.Int64
	rowReads    atomic.Int64
	commits     atomic.Int64
	probeErrors atomic.Int64
	mu          sync.Mutex
	relations   map[string]int64

	captureStatements bool
	captured          []writeLoadStatement
}

type writeLoadBatchKey struct{}

type writeLoadStatement struct {
	SQL  string
	Args []any
}

func (w *writeLoadWork) capture(sql string, args []any) {
	if !w.captureStatements {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.captured = append(w.captured, writeLoadStatement{SQL: sql, Args: slices.Clone(args)})
}

func (w *writeLoadWork) addRelation(relation string, reads int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.relations == nil {
		w.relations = map[string]int64{}
	}
	w.relations[relation] += reads
}

func (f *writeLoadFixture) measuredRequest(t *testing.T, path, token string, payload any) (*httptest.ResponseRecorder, writeLoadWorkCounts) {
	t.Helper()
	return f.measuredRequestWith(t, &writeLoadWork{}, path, token, payload)
}

func (f *writeLoadFixture) measuredRequestWith(t *testing.T, work *writeLoadWork, path, token string, payload any) (*httptest.ResponseRecorder, writeLoadWorkCounts) {
	t.Helper()
	request := newHubAPIRequest(t, http.MethodPost, path, token, payload)
	ctx := context.WithValue(t.Context(), writeLoadWorkKey{}, work)
	request = request.WithContext(postgres.WithTransactionTiming(ctx, nil, func(time.Time, string) { work.statements.Add(1) }))
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	work.mu.Lock()
	relations, captured := maps.Clone(work.relations), slices.Clone(work.captured)
	work.mu.Unlock()
	return response, writeLoadWorkCounts{Statements: work.statements.Load(), RowReads: work.rowReads.Load(), Commits: work.commits.Load(), ProbeErrors: work.probeErrors.Load(), Relations: relations, Captured: captured}
}

func TestRunnerHostSummaryWriteCost(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("ten-runner heartbeat write measurement")
	}
	f := newWriteLoadFixture(t, 0, 10, 0)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	now := f.now
	for _, r := range f.runners {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 8, "version": "test", "backend_isolation": r.redemption.BackendIsolation}), http.StatusOK)
	}
	f.service.tokenUseWork.Wait()
	measure := func(name string, summary *hostmetrics.Summary) int64 {
		t.Helper()
		probe := new(writeLoadRowProbe)
		ctx := context.WithValue(t.Context(), writeLoadProbeKey{}, probe)
		var latencies []time.Duration
		for round := range 120 {
			for _, r := range f.runners {
				body := map[string]any{"display_name": "runner", "capacity": 8, "version": "test", "backend_isolation": r.redemption.BackendIsolation}
				if summary != nil && round == 0 {
					body["host_metrics"] = []hostmetrics.Summary{*summary}
				}
				started := time.Now()
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, body).WithContext(ctx)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				requireNativeStatus(t, response, http.StatusOK)
				latencies = append(latencies, time.Since(started))
			}
		}
		writes, err := probe.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(latencies)
		t.Logf("runners=10 mode=%s requests=1200 postgres_row_writes=%d heartbeat_p50=%s heartbeat_p95=%s", name, writes, latencies[len(latencies)/2], latencies[len(latencies)*95/100])
		return writes
	}
	baseline := measure("before hourly summary", nil)
	summary := testHostSummary(now.Add(-time.Hour))
	summary.Partial = false
	hourly := measure("completed hour", &summary)
	idle := measure("after acknowledged summary", nil)
	retry := measure("retry deduplicated", &summary)
	summary.SegmentID = now
	summary.Partial = true
	restart := measure("graceful restart segment", &summary)
	if hourly-baseline != 10 || restart-baseline != 10 || idle != baseline || retry != baseline {
		t.Fatalf("writes baseline=%d hourly=%d idle=%d retry=%d restart=%d", baseline, hourly, idle, retry, restart)
	}
}

type writeLoadProbeKey struct{}
type writeLoadProbeReadingKey struct{}
type writeLoadCommitCountKey struct{}
type writeLoadRowProbe struct {
	mu      sync.Mutex
	rows    int64
	commits int
	err     error
}

func (p *writeLoadRowProbe) snapshot() (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commits == 0 {
		return 0, errors.Join(p.err, errors.New("no PostgreSQL transaction commits observed"))
	}
	return p.rows, p.err
}

type writeLoadTrace struct{}

func (trace writeLoadTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if len(data.Batch.QueuedQueries) == 0 {
		return ctx
	}
	return trace.TraceQueryStart(context.WithValue(ctx, writeLoadBatchKey{}, true), conn, pgx.TraceQueryStartData{SQL: data.Batch.QueuedQueries[0].SQL})
}

func (writeLoadTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if work, ok := ctx.Value(writeLoadWorkKey{}).(*writeLoadWork); ok && ctx.Value(writeLoadProbeReadingKey{}) != true {
		work.capture(data.SQL, data.Args)
	}
}

func (writeLoadTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (writeLoadTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if work, ok := ctx.Value(writeLoadWorkKey{}).(*writeLoadWork); ok && ctx.Value(writeLoadProbeReadingKey{}) != true && ctx.Value(writeLoadBatchKey{}) != true {
		work.capture(data.SQL, data.Args)
	}
	if work, ok := ctx.Value(writeLoadWorkKey{}).(*writeLoadWork); ok && ctx.Value(writeLoadProbeReadingKey{}) != true && conn.PgConn().TxStatus() == 'I' {
		statement := strings.ToUpper(strings.TrimSpace(data.SQL))
		if statement == "BEGIN" || strings.HasPrefix(statement, "BEGIN ") || strings.HasPrefix(statement, "BEGIN;") {
			if _, err := conn.Exec(context.WithValue(ctx, writeLoadProbeReadingKey{}, true), "SELECT pg_stat_force_next_flush()"); err != nil {
				work.probeErrors.Add(1)
			}
		}
	}
	if work, ok := ctx.Value(writeLoadWorkKey{}).(*writeLoadWork); ok && ctx.Value(writeLoadProbeReadingKey{}) != true && strings.EqualFold(strings.TrimSpace(data.SQL), "COMMIT") && conn.PgConn().TxStatus() == 'T' {
		var reads int64
		rows, err := conn.Query(context.WithValue(ctx, writeLoadProbeReadingKey{}, true), `SELECT relname,(seq_tup_read+COALESCE(idx_tup_fetch,0))::bigint FROM pg_stat_xact_user_tables WHERE schemaname='public' AND seq_tup_read+COALESCE(idx_tup_fetch,0)>0`)
		if err == nil {
			for rows.Next() {
				var relation string
				var read int64
				if err = rows.Scan(&relation, &read); err != nil {
					break
				}
				reads += read
				work.addRelation(relation, read)
			}
			rows.Close()
			err = errors.Join(err, rows.Err())
		}
		if err != nil {
			work.probeErrors.Add(1)
		} else {
			work.rowReads.Add(reads)
			work.commits.Add(1)
		}
	}
	p, ok := ctx.Value(writeLoadProbeKey{}).(*writeLoadRowProbe)
	if !ok || ctx.Value(writeLoadProbeReadingKey{}) == true {
		return ctx
	}
	statement := strings.ToUpper(strings.TrimSpace(data.SQL))
	if (statement == "BEGIN" || strings.HasPrefix(statement, "BEGIN ") || strings.HasPrefix(statement, "BEGIN;")) && conn.PgConn().TxStatus() == 'I' {
		if _, err := conn.Exec(context.WithValue(ctx, writeLoadProbeReadingKey{}, true), "SELECT pg_stat_force_next_flush()"); err != nil {
			p.mu.Lock()
			p.err = errors.Join(p.err, err)
			p.mu.Unlock()
		}
		return ctx
	}
	if !strings.EqualFold(strings.TrimSpace(data.SQL), "COMMIT") || conn.PgConn().TxStatus() != 'T' {
		return ctx
	}
	var rows int64
	err := conn.QueryRow(context.WithValue(ctx, writeLoadProbeReadingKey{}, true), `SELECT COALESCE(sum(n_tup_ins+n_tup_upd+n_tup_del),0)::bigint FROM pg_stat_xact_user_tables WHERE schemaname='public'`).Scan(&rows)
	if err != nil {
		p.mu.Lock()
		p.err = errors.Join(p.err, err)
		p.mu.Unlock()
		return ctx
	}
	return context.WithValue(ctx, writeLoadCommitCountKey{}, rows)
}

func (writeLoadTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	rows, ok := ctx.Value(writeLoadCommitCountKey{}).(int64)
	if !ok || data.Err != nil || data.CommandTag.String() != "COMMIT" {
		return
	}
	p := ctx.Value(writeLoadProbeKey{}).(*writeLoadRowProbe)
	p.mu.Lock()
	p.rows += rows
	p.commits++
	p.mu.Unlock()
}

type explicitDeadline struct {
	context.Context
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (d *explicitDeadline) Err() error {
	if err := d.Context.Err(); err != nil && d.fired.Load() {
		return context.DeadlineExceeded
	}
	return d.Context.Err()
}

func (d *explicitDeadline) fire() {
	d.fired.Store(true)
	d.cancel()
}

// TestRunnerHeartbeatDeadlineReleasesItsTransaction fires the heartbeat's
// request deadline through the injected factory while the heartbeat waits on
// its runner identity row lock, so cancellation is exercised without waiting
// out the 15-second production deadline.
func TestRunnerHeartbeatDeadlineReleasesItsTransaction(t *testing.T) {
	t.Parallel()
	f := newWriteLoadFixture(t, 0, 1, 0)
	runner := f.runners[0]
	admin := usagePostgresAdmin(t, f.service)
	type deadlineProbeKey struct{}
	var deadlinesMu sync.Mutex
	var deadlines []*explicitDeadline
	fireDeadlines := func() int {
		deadlinesMu.Lock()
		defer deadlinesMu.Unlock()
		live := 0
		for _, deadline := range deadlines {
			if deadline.Context.Err() == nil {
				live++
			}
			deadline.fire()
		}
		return live
	}
	f.service.config.Deps().RequestTimeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		inner, cancel := context.WithCancel(ctx)
		if ctx.Value(deadlineProbeKey{}) == nil {
			return inner, cancel
		}
		deadline := &explicitDeadline{Context: inner, cancel: cancel}
		deadlinesMu.Lock()
		deadlines = append(deadlines, deadline)
		deadlinesMu.Unlock()
		return deadline, cancel
	}
	heartbeat := func(ctx context.Context) *httptest.ResponseRecorder {
		request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential,
			map[string]any{"backend_isolation": runner.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []any{capacityReport(f.now)}}).WithContext(ctx)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		return response
	}
	state := func() (receipts int, heartbeatAt time.Time) {
		t.Helper()
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_usage_receipts WHERE organization_id=$1 AND metric='heartbeats'", f.project.OrganizationID).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRowContext(t.Context(), "SELECT last_heartbeat_at FROM machines WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, string(runner.binding.MachineID)).Scan(&heartbeatAt); err != nil {
			t.Fatal(err)
		}
		return receipts, heartbeatAt
	}
	requireNativeStatus(t, heartbeat(t.Context()), http.StatusOK)
	receiptsBefore, heartbeatBefore := state()
	later := f.now.Add(time.Minute)
	f.service.config.Clock = func() time.Time { return later }

	holder, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	lock, err := holder.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			if err := lock.Rollback(); err != nil {
				t.Error(err)
			}
		})
	}
	defer release()
	var holderPID int
	if err := lock.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(t.Context(), "SELECT 1 FROM runner_identities WHERE organization_id=$1 AND runner_id=$2 FOR UPDATE", f.project.OrganizationID, runner.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	var requests sync.WaitGroup
	defer requests.Wait()
	defer release()
	requests.Go(func() { responses <- heartbeat(context.WithValue(t.Context(), deadlineProbeKey{}, true)) })
	defer func() { fireDeadlines() }()
	activity := func(query string, args ...any) *sql.Row {
		t.Helper()
		if _, err := lock.ExecContext(t.Context(), "SELECT pg_stat_clear_snapshot()"); err != nil {
			t.Fatal(err)
		}
		return lock.QueryRowContext(t.Context(), query, args...)
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		if err := activity(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("heartbeat is not yet waiting on the runner identity lock")
		}
		return nil
	})
	if live := fireDeadlines(); live != 1 {
		t.Fatalf("fired %d live request deadlines, want the waiting heartbeat's one", live)
	}
	response := <-responses
	requireNativeCode(t, response, http.StatusInternalServerError, "native_operation_failed")
	testenv.WaitUntil(t, t.Context(), func() error {
		var stuck int
		if err := activity(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND (wait_event_type='Lock' OR state LIKE 'idle in transaction%')`).Scan(&stuck); err != nil {
			return err
		}
		if stuck != 0 {
			return fmt.Errorf("%d sessions still hold or wait on a transaction after the deadline", stuck)
		}
		return nil
	})
	release()
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if receipts, heartbeatAt := state(); receipts != receiptsBefore || !heartbeatAt.Equal(heartbeatBefore) {
		t.Fatalf("cancelled heartbeat persisted effects: receipts=%d want %d heartbeat_at=%s want %s", receipts, receiptsBefore, heartbeatAt, heartbeatBefore)
	}
	for range 6 {
		requireNativeStatus(t, heartbeat(t.Context()), http.StatusOK)
	}
	if receipts, heartbeatAt := state(); receipts != receiptsBefore+6 || !heartbeatAt.Equal(later) {
		t.Fatalf("heartbeats after the deadline: receipts=%d want %d heartbeat_at=%s want %s", receipts, receiptsBefore+6, heartbeatAt, later)
	}
}

// requireIndexedWriteLoadPlans explains every statement a request executed,
// with its own arguments and sequential scans disabled. A statement whose
// plan still reads a relation sequentially has no index path, so its row
// work grows with the tenant's retained rows once tables outgrow toy size.
func requireIndexedWriteLoadPlans(t *testing.T, admin *sql.DB, route string, statements []writeLoadStatement) {
	t.Helper()
	conn, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	explained := 0
	err = conn.Raw(func(driverConnection any) error {
		native, ok := driverConnection.(*stdlib.Conn)
		if !ok {
			return errors.New("plan check requires a pgx connection")
		}
		pg := native.Conn()
		for _, statement := range statements {
			text := strings.TrimSpace(statement.SQL)
			upper := strings.ToUpper(text)
			if strings.HasPrefix(upper, "BEGIN") || upper == "COMMIT" || upper == "ROLLBACK" || strings.HasPrefix(upper, "SET ") || strings.Contains(upper, "SET_CONFIG(") {
				continue
			}
			tx, err := pg.Begin(t.Context())
			if err != nil {
				return err
			}
			var plan []byte
			_, err = tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off")
			if err == nil {
				err = tx.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+text, statement.Args...).Scan(&plan)
			}
			if rollbackErr := tx.Rollback(context.WithoutCancel(t.Context())); err == nil {
				err = rollbackErr
			}
			if err != nil {
				return fmt.Errorf("explain %s: %w", strings.Join(strings.Fields(text), " ")[:min(len(text), 160)], err)
			}
			explained++
			if scans := writeLoadSeqScans(plan); len(scans) != 0 {
				t.Errorf("%s statement reads %v sequentially: %s", route, scans, strings.Join(strings.Fields(text), " ")[:min(len(strings.Join(strings.Fields(text), " ")), 240)])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if explained == 0 {
		t.Fatalf("%s plan check explained no statements", route)
	}
	t.Logf("%s plan check explained %d statements with sequential scans disabled", route, explained)
}

func writeLoadSeqScans(raw []byte) []string {
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if json.Unmarshal(raw, &plans) != nil {
		return []string{"unparseable plan"}
	}
	var scans []string
	var walk func(json.RawMessage)
	walk = func(node json.RawMessage) {
		var value struct {
			NodeType string            `json:"Node Type"`
			Relation string            `json:"Relation Name"`
			Plans    []json.RawMessage `json:"Plans"`
		}
		if json.Unmarshal(node, &value) != nil {
			return
		}
		if value.NodeType == "Seq Scan" {
			scans = append(scans, value.Relation)
		}
		for _, child := range value.Plans {
			walk(child)
		}
	}
	for _, plan := range plans {
		walk(plan.Plan)
	}
	return scans
}
