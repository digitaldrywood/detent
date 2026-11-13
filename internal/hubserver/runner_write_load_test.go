package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
	config := Config{Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour}, now: func() time.Time { return now }, SecretKeys: secretTestKeys(t, "1", "1")}
	service := openLocalPostgresHubTestService(t, repository, config)
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
	plans := capacityHostedPlans()
	plans.Base = PlanReference{ID: "scale", Version: 1}
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
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
	now := f.service.config.now().UTC()
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
	}
	var stored int
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='issue.edited'`, f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != history {
		t.Fatalf("history events=%d, want %d", stored, history)
	}
}

func (f *writeLoadFixture) heartbeat(t *testing.T, runner runnerFixture) int {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential,
		map[string]any{"backend_isolation": runner.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []any{capacityReport(f.now)}}).Code
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
	if testing.Short() {
		t.Skip("runner write load reproduction")
	}
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
			f := newWriteLoadFixture(t, 50000, test.runners, test.sessions)
			const turnBatches = 40
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			var mu sync.Mutex
			latencies := map[string][]time.Duration{}
			var failures []string
			record := func(route string, elapsed time.Duration, failure string) {
				mu.Lock()
				defer mu.Unlock()
				latencies[route] = append(latencies[route], elapsed)
				if failure != "" {
					failures = append(failures, failure)
				}
			}
			var heartbeats, workers, ready sync.WaitGroup
			ready.Add(len(f.runners))
			start := make(chan struct{})
			for _, runner := range f.runners {
				heartbeats.Go(func() {
					first := true
					for ctx.Err() == nil {
						started := time.Now()
						code := f.heartbeat(t, runner)
						failure := ""
						if code != http.StatusOK {
							failure = fmt.Sprintf("heartbeat %d", code)
						}
						record("heartbeat", time.Since(started), failure)
						if first {
							ready.Done()
							<-start
							first = false
						}
					}
				})
			}
			ready.Wait()
			for index, session := range f.sessions {
				workers.Go(func() {
					<-start
					chunk := strings.Repeat("output ", 64)
					for range turnBatches {
						started := time.Now()
						response := f.turnEvents(t, session,
							map[string]any{"type": "delta", "provider_item_id": fmt.Sprintf("item-%d", index), "text": chunk},
							map[string]any{"type": "delta", "provider_item_id": fmt.Sprintf("item-%d", index), "text": chunk})
						failure := ""
						if response.Code != http.StatusAccepted {
							failure = fmt.Sprintf("turn-events %d %s", response.Code, response.Body)
						}
						record("turn-events", time.Since(started), failure)
					}
				})
			}
			close(start)
			workers.Wait()
			stop()
			heartbeats.Wait()
			if len(latencies["turn-events"]) != test.sessions*turnBatches {
				t.Fatalf("turn-event requests=%d, want %d", len(latencies["turn-events"]), test.sessions*turnBatches)
			}
			for _, route := range []string{"heartbeat", "turn-events"} {
				values := latencies[route]
				if len(values) == 0 {
					t.Fatalf("no %s requests completed", route)
				}
				slices.Sort(values)
				t.Logf("sessions=%d runners=%d route=%s requests=%d p50=%s p95=%s max=%s", test.sessions, test.runners, route, len(values), values[len(values)/2], values[len(values)*95/100], values[len(values)-1])
			}
			if len(failures) != 0 {
				t.Fatalf("requests failed: %v", failures[:min(len(failures), 5)])
			}
		})
	}
}

func TestRunnerHostSummaryWriteCost(t *testing.T) {
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
	return trace.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{SQL: data.Batch.QueuedQueries[0].SQL})
}

func (writeLoadTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (writeLoadTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (writeLoadTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
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
