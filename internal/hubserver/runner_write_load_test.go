package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
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

func newWriteLoadFixture(t *testing.T, runners, sessions int) *writeLoadFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository, _ := projectRecordPostgresWithClock(t, 4, func() time.Time { return now })
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

func TestConcurrentRunnerWritesPreserveTenantData(t *testing.T) {
	t.Parallel()
	const heartbeatRounds, turnBatches = 2, 3
	chunk := strings.Repeat("output ", 64)
	for _, test := range []struct {
		name     string
		sessions int
		runners  int
	}{
		{name: "two sessions on two runners", sessions: 2, runners: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newWriteLoadFixture(t, test.runners, test.sessions)
			f.service.config.deps.RequestTimeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
				return context.WithCancel(ctx)
			}
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
						response := f.sendRequest(t, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential,
							map[string]any{"backend_isolation": runner.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []any{capacityReport(f.now)}})
						samples <- sample{route: "heartbeat", code: response.Code, failure: response.Body.String(), expected: http.StatusOK}
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
						response := f.sendRequest(t, f.base+"/conversations/"+session.conversation+"/turn-events", f.worker, body)
						samples <- sample{route: "turn-events", code: response.Code, failure: response.Body.String(), expected: http.StatusAccepted}
					}
				})
			}
			release()
			workers.Wait()
			close(samples)
			counts := map[string]int{}
			for sample := range samples {
				counts[sample.route]++
				if sample.code != sample.expected {
					t.Errorf("%s status=%d want %d: %s", sample.route, sample.code, sample.expected, sample.failure)
					continue
				}
			}
			if counts["heartbeat"] != test.runners*heartbeatRounds || counts["turn-events"] != test.sessions*turnBatches {
				t.Fatalf("requests heartbeat=%d turn-events=%d, want %d and %d", counts["heartbeat"], counts["turn-events"], test.runners*heartbeatRounds, test.sessions*turnBatches)
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
				response := f.sendRequest(t, probe.path, probe.token, probe.body)
				if response.Code != probe.expected {
					t.Fatalf("serial %s status=%d, want %d: %s", probe.route, response.Code, probe.expected, response.Body.String())
				}
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

func (f *writeLoadFixture) sendRequest(t *testing.T, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	request := newHubAPIRequest(t, http.MethodPost, path, token, payload)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
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
	f := newWriteLoadFixture(t, 1, 0)
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
	f.service.config.deps.RequestTimeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
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
