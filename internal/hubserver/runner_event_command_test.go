package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerEventSubmissionWireBudget(t *testing.T) {
	first, second, repository, wire, apiLog := newRunnerEventWireFixtures(t)
	if _, err := first.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_run_command_receipt() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.command_key='rollback' THEN RAISE EXCEPTION 'fixture publication rejection'; END IF; RETURN NEW; END$$; CREATE TRIGGER reject_run_command_receipt BEFORE INSERT ON native_commands FOR EACH ROW EXECUTE FUNCTION reject_run_command_receipt()`); err != nil {
		t.Fatal(err)
	}
	bounds := map[string][2]int{}
	check := func(key string, statements, exchanges, transactions int) {
		t.Helper()
		if exchanges < 1 || transactions < 1 {
			t.Fatal("command escaped wire instrumentation")
		}
		if exchanges > 8 {
			t.Errorf("%s exchanges=%d,want <=8", key, exchanges)
		}
		value := [2]int{statements, exchanges}
		if previous, ok := bounds[key]; ok && previous != value {
			t.Errorf("%s grew with history: before=%v after=%v", key, previous, value)
		}
		bounds[key] = value
	}

	for tenantIndex, fixture := range []runnerPGFixture{first, second} {
		org := tracker.OrganizationID("org_startup")
		if tenantIndex == 1 {
			org = "org_second"
		}
		for index := range fixture.bindings {
			credential, status, err := fixture.service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(fixture.tokens[index]), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("authority status=%d error=%v", status, err)
			}
			scope := nativeScope{organization: org, project: "prj_runner", credential: credential}
			history := 0
			for _, size := range []int{2, 50} {
				for ; history < size; history++ {
					raw, err := json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("history_%d_%d", index, history), OrganizationID: org, ProjectID: scope.project, AggregateType: "issue", AggregateID: fixture.leases[index].WorkItemID, Type: "issue.updated", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: time.Now().UTC()})
					if err != nil {
						t.Fatal(err)
					}
					event, err := eventstore.NewContent(fixture.service.runDetailStore()).PrepareEvent(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
						t.Fatal(err)
					}
				}
				data := fixture.data[index]
				data.Sequence++
				data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
				request := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("observation_%d_%d", index, size)}, Type: "run.observed", SchemaVersion: 1, Data: data}
				wire.Lock()
				wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
				wire.Unlock()
				ctx := context.WithValue(t.Context(), claimRequestTraceKey{}, true)
				result, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), request)
				if err != nil || string(result) != `{"accepted":true}` {
					t.Fatalf("event result=%s error=%v", result, err)
				}
				wire.Lock()
				statements, exchanges, transactions := wire.statements, wire.exchanges, wire.transactions
				wire.Unlock()
				t.Logf("tenant=%s runner=%d history=%d command statements=%d exchanges=%d transactions=%d", org, index, size, statements, exchanges, transactions)
				check(fmt.Sprintf("%s/%d/event", org, index), statements, exchanges, transactions)
				wire.Lock()
				wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
				wire.Unlock()
				if _, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), request); err != nil {
					t.Fatal(err)
				}
				wire.Lock()
				replayStatements, replayExchanges, replayTransactions := wire.statements, wire.exchanges, wire.transactions
				wire.Unlock()
				check(fmt.Sprintf("%s/%d/replay", org, index), replayStatements, replayExchanges, replayTransactions)

				for _, count := range []int{8} {
					batch := request
					batch.IdempotencyKey = fmt.Sprintf("batch_%d_%d", index, size)
					batch.PriorEvents = nil
					for i := 0; i < count; i++ {
						next := request
						next.IdempotencyKey = ""
						data.Sequence++
						next.Data = data
						if i < count-1 {
							batch.PriorEvents = append(batch.PriorEvents, next)
						} else {
							batch.Data = data
						}
					}
					wire.Lock()
					wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
					wire.Unlock()
					if _, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), batch); err != nil {
						t.Fatal(err)
					}
					wire.Lock()
					batchStatements, batchExchanges, batchTransactions := wire.statements, wire.exchanges, wire.transactions
					wire.Unlock()
					check(fmt.Sprintf("%s/%d/batch", org, index), batchStatements, batchExchanges, batchTransactions)
					if batchExchanges != exchanges {
						t.Errorf("batch exchanges=%d, single=%d", batchExchanges, exchanges)
					}
				}
				for _, count := range []int{1, 8} {
					batch := request
					batch.IdempotencyKey = fmt.Sprintf("usage_%d_%d_%d", index, size, count)
					batch.PriorEvents = nil
					for i := 0; i < count; i++ {
						data.Sequence++
						data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: data.Sequence * 17, Output: data.Sequence * 3, BillingMode: "metered", Currency: "USD", To: time.Now().UTC()}}
						next := request
						next.IdempotencyKey = ""
						next.Data = data
						if i < count-1 {
							batch.PriorEvents = append(batch.PriorEvents, next)
						} else {
							batch.Data = data
						}
					}
					wire.Lock()
					wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
					wire.Unlock()
					if _, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), batch); err != nil {
						t.Fatal(err)
					}
					wire.Lock()
					statements, usageExchanges, transactions := wire.statements, wire.exchanges, wire.transactions
					wire.Unlock()
					t.Logf("tenant=%s history=%d usage=%d statements=%d exchanges=%d", org, size, count, statements, usageExchanges)
					check(fmt.Sprintf("%s/%d/usage/%d", org, index, count), statements, usageExchanges, transactions)
					if usageExchanges != exchanges {
						t.Errorf("usage exchanges=%d single=%d", usageExchanges, exchanges)
					}
				}
				start := runnersession.Mutation{Key: fmt.Sprintf("session_%d_%d", index, size), LeaseID: fixture.leases[index].ID, FencingToken: fixture.leases[index].FencingToken, ProcessKey: strings.Repeat("x", 32), Start: &store.SessionStart{IssueID: string(fixture.leases[index].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"}}
				var session runnersession.Snapshot
				for _, kind := range []string{"start", "progress"} {
					mutation := start
					if kind == "progress" {
						mutation.Start = nil
						mutation.Key += "_progress"
						mutation.SessionID = session.ID
						mutation.ExpectedRevision = session.Revision
						mutation.Progress = &store.SessionProgress{LastProgressAt: time.Now().UTC()}
					}
					httpRequest := newHubAPIRequest(t, http.MethodPost, "/", fixture.tokens[index], mutation).WithContext(ctx)
					response := httptest.NewRecorder()
					c := echo.New().NewContext(httpRequest, response)
					c.Set("native_scope", scope)
					wire.Lock()
					wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
					wire.Unlock()
					if err := fixture.service.mutateRunnerSession(c); err != nil {
						t.Fatal(err)
					}
					if response.Code != http.StatusOK {
						t.Logf("session diagnostics: %s", apiLog.String())
					}
					requireNativeStatus(t, response, http.StatusOK)
					if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
						t.Fatal(err)
					}
					wire.Lock()
					sessionStatements, sessionExchanges, sessionTransactions := wire.statements, wire.exchanges, wire.transactions
					wire.Unlock()
					t.Logf("tenant=%s history=%d session %s exchanges=%d", org, size, kind, sessionExchanges)
					check(fmt.Sprintf("%s/%d/session/%s", org, index, kind), sessionStatements, sessionExchanges, sessionTransactions)
				}
				data.Usage = nil
				data.Sequence++
				httpEvent := request
				httpEvent.IdempotencyKey = fmt.Sprintf("http_%d_%d", index, size)
				httpEvent.Data = data
				path := fmt.Sprintf("/api/v2/organizations/%s/projects/prj_runner/work-items/%s/events", org, fixture.leases[index].WorkItemID)
				wire.Lock()
				wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
				wire.Unlock()
				response := httptest.NewRecorder()
				httpRequest := newHubAPIRequest(t, http.MethodPost, path, fixture.tokens[index], httpEvent).WithContext(ctx)
				fixture.service.Handler().ServeHTTP(response, httpRequest)
				requireNativeStatus(t, response, http.StatusOK)
				wire.Lock()
				httpStatements, httpExchanges, httpTransactions := wire.statements, wire.exchanges, wire.transactions
				wire.Unlock()
				t.Logf("tenant=%s history=%d HTTP statements=%d exchanges=%d transactions=%d", org, size, httpStatements, httpExchanges, httpTransactions)
				check(fmt.Sprintf("%s/%d/http", org, index), httpStatements, httpExchanges, httpTransactions)
				fixture.data[index] = data
			}
			bad := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "rollback"}, Type: "run.observed", SchemaVersion: 1, Data: fixture.data[index]}
			bad.Data.Sequence++
			if _, err := fixture.service.appendNativeRunEventTenant(t.Context(), scope, string(fixture.leases[index].WorkItemID), bad); err == nil {
				t.Fatal("publication failure committed")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				attempt, err := r.Attempt(t.Context(), bad.Data.AttemptID)
				if err != nil {
					return err
				}
				if attempt.Sequence != bad.Data.Sequence-1 {
					t.Fatalf("failed publication advanced attempt to %d", attempt.Sequence)
				}
				reader, ok := r.(nativeRunEventReader)
				if !ok {
					t.Fatal("missing run event reader")
				}
				_, err = reader.RunEvent(t.Context(), scope.project, fixture.leases[index].WorkItemID, nativeAttemptHistoryID(bad.Type, bad.Data))
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("failed publication left history: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func newRunnerEventWireFixtures(t *testing.T) (runnerPGFixture, runnerPGFixture, *postgres.Repository, *heartbeatWire, *bytes.Buffer) {
	t.Helper()
	f := newRunnerPGFixture(t)
	config, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	wire := new(heartbeatWire)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := new(net.Dialer).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &heartbeatConn{Conn: conn, wire: wire, startup: true}, nil
	}
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		stream, ok := conn.PgConn().Conn().(*heartbeatConn)
		if !ok {
			return false, fmt.Errorf("missing wire wrapper: %T", conn.PgConn().Conn())
		}
		stream.wire = new(heartbeatWire)
		if ctx.Value(claimRequestTraceKey{}) == true {
			stream.wire = wire
		}
		return true, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, f.service.config.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	original := f.service.database.postgres
	t.Cleanup(func() { f.service.config.TenantRepository = original; f.service.database.postgres = original })
	f.service.config.TenantRepository = repository
	f.service.database.postgres = repository

	apiLog := new(bytes.Buffer)
	localService := openLocalPostgresHubTestService(t, repository, Config{RunDetails: f.service.runDetailStore(), SecretKeys: secretTestKeys(t, "1", "1"), Logger: slog.New(slog.NewTextHandler(apiLog, nil))})
	first := f
	first.service = localService
	second := seedRunnerEventTenant(t, localService, f, "org_second")
	return first, second, repository, wire, apiLog
}
