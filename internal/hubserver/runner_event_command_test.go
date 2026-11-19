package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerEventSubmissionPublishesAndRollsBackAtomically(t *testing.T) {
	t.Parallel()
	first, second, repository, apiLog := newRunnerEventFixtures(t)
	if _, err := first.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_run_command_receipt() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.command_key='rollback' THEN RAISE EXCEPTION 'fixture publication rejection'; END IF; RETURN NEW; END$$; CREATE TRIGGER reject_run_command_receipt BEFORE INSERT ON native_commands FOR EACH ROW EXECUTE FUNCTION reject_run_command_receipt()`); err != nil {
		t.Fatal(err)
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
			scope := hubscope.NativeScope{Organization: org, Project: "prj_runner", Credential: credential}
			warm := fixture.data[index]
			warm.Sequence++
			warm.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
			if result, err := fixture.service.appendNativeRunEventTenant(t.Context(), scope, string(fixture.leases[index].WorkItemID), tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("observation_%d_phase", index)}, Type: "run.observed", SchemaVersion: 1, Data: warm}); err != nil || string(result) != `{"accepted":true}` {
				t.Fatalf("phase observation result=%s error=%v", result, err)
			}
			fixture.data[index] = warm
			history := 0
			for _, size := range []int{2} {
				for ; history < size; history++ {
					raw, err := json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("history_%d_%d", index, history), OrganizationID: org, ProjectID: scope.Project, AggregateType: "issue", AggregateID: fixture.leases[index].WorkItemID, Type: "issue.updated", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: time.Now().UTC()})
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
				ctx := t.Context()
				result, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), request)
				if err != nil || string(result) != `{"accepted":true}` {
					t.Fatalf("event result=%s error=%v", result, err)
				}
				var references, missingArchives, unchargedArchives int
				if err := fixture.admin.QueryRowContext(t.Context(), `WITH refs AS (
 SELECT payload_object_key,NULL::text AS payload_sha256 FROM native_attempts WHERE organization_id=$1 AND id=$4
 UNION ALL
 SELECT payload_object_key,payload_sha256 FROM (
  SELECT payload_object_key,payload_sha256 FROM collaboration_events
  WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND attempt_id=$4 AND type='run.observed'
  ORDER BY sequence DESC LIMIT 1
 ) history
 UNION ALL
 SELECT payload_object_key,payload_sha256 FROM native_commands WHERE organization_id=$1 AND command_key=$5
)
SELECT count(*),count(*) FILTER (WHERE a.id IS NULL),count(*) FILTER (WHERE a.collaboration_bytes<>a.compressed_size)
FROM refs r LEFT JOIN archive_objects a ON a.organization_id=$1 AND a.object_key=r.payload_object_key AND (r.payload_sha256 IS NULL OR a.sha256=r.payload_sha256)`, org, scope.Project, fixture.leases[index].WorkItemID, data.AttemptID, request.IdempotencyKey).Scan(&references, &missingArchives, &unchargedArchives); err != nil {
					t.Fatal(err)
				}
				if references != 3 || missingArchives != 0 || unchargedArchives != 0 {
					t.Fatalf("runner publication archives: references=%d missing=%d uncharged=%d, want 3/0/0", references, missingArchives, unchargedArchives)
				}
				if _, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), request); err != nil {
					t.Fatal(err)
				}

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
					if _, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), batch); err != nil {
						t.Fatal(err)
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
					if _, err := fixture.service.appendNativeRunEventTenant(ctx, scope, string(fixture.leases[index].WorkItemID), batch); err != nil {
						t.Fatal(err)
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
					httpRequest := newHubAPIRequest(t, http.MethodPost, fmt.Sprintf("/api/v2/organizations/%s/projects/prj_runner/runner-sessions", org), fixture.tokens[index], mutation).WithContext(ctx)
					response := httptest.NewRecorder()
					fixture.service.Handler().ServeHTTP(response, httpRequest)
					if response.Code != http.StatusOK {
						t.Logf("session diagnostics: %s", apiLog.String())
					}
					requireNativeStatus(t, response, http.StatusOK)
					if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
						t.Fatal(err)
					}
				}
				data.Usage = nil
				data.Sequence++
				httpEvent := request
				httpEvent.IdempotencyKey = fmt.Sprintf("http_%d_%d", index, size)
				httpEvent.Data = data
				path := fmt.Sprintf("/api/v2/organizations/%s/projects/prj_runner/work-items/%s/events", org, fixture.leases[index].WorkItemID)
				response := httptest.NewRecorder()
				httpRequest := newHubAPIRequest(t, http.MethodPost, path, fixture.tokens[index], httpEvent).WithContext(ctx)
				fixture.service.Handler().ServeHTTP(response, httpRequest)
				requireNativeStatus(t, response, http.StatusOK)
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
				reader, ok := r.(hubrecords.NativeRunEventReader)
				if !ok {
					t.Fatal("missing run event reader")
				}
				_, err = reader.RunEvent(t.Context(), scope.Project, fixture.leases[index].WorkItemID, nativeAttemptHistoryID(bad.Type, bad.Data))
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

func newRunnerEventFixtures(t *testing.T) (runnerPGFixture, runnerPGFixture, *postgres.Repository, *bytes.Buffer) {
	t.Helper()
	f := newRunnerPGFixture(t)
	config, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
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
	return first, second, repository, apiLog
}

func TestRunnerSessionMetadataReplaysSafely(t *testing.T) {
	t.Parallel()
	first, second, _, _ := newRunnerEventFixtures(t)
	for index, fixture := range []runnerPGFixture{first, second} {
		organization := []string{"org_startup", "org_second"}[index]
		t.Run(organization, func(t *testing.T) {
			requests := 0
			rejectNext := false
			loseNext := false
			client, err := api.New(api.Config{URL: "http://hub.test", TokenSource: func() string { return fixture.tokens[0] }, HTTPClient: &http.Client{Transport: progressPublicationTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				response := httptest.NewRecorder()
				if rejectNext && request.Method == http.MethodPost {
					rejectNext = false
					response.WriteHeader(http.StatusServiceUnavailable)
					return response.Result(), nil
				}
				fixture.service.Handler().ServeHTTP(response, request)
				if loseNext && request.Method == http.MethodPost {
					loseNext = false
					if response.Code != http.StatusOK {
						t.Fatalf("lost write did not commit: %d %s", response.Code, response.Body.String())
					}
					return nil, io.ErrUnexpectedEOF
				}
				return response.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(tracker.OrganizationID(organization), "prj_runner")
			if err != nil {
				t.Fatal(err)
			}
			ctx := api.WithMutationAuthority(t.Context(), native.Scope(), fixture.leases[0], nil)
			sessions := api.NewRunnerSessions(native)
			id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(fixture.leases[0].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"})
			if err != nil {
				t.Fatal(err)
			}
			provider := store.SessionProviderIdentity{ThreadID: "example-thread", SessionID: "example-session"}
			process := store.WorkerProcessRegistration{WorkerProcessIdentity: store.WorkerProcessIdentity{PID: 12345, GroupID: 12345, StartedAt: time.Now().UTC().Truncate(time.Microsecond)}, CleanupRoot: "/example/workspace", CleanupPath: "/example/workspace/scratch"}
			update := func(ctx context.Context) error {
				if err := sessions.UpdateSessionWorkerProcess(ctx, id, process); err != nil {
					return err
				}
				return sessions.UpdateSessionProviderIdentity(ctx, id, provider)
			}
			if err := update(ctx); err != nil {
				t.Fatal(err)
			}
			for _, events := range []int{2} {
				requests = 0
				for range events {
					if err := update(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if requests != 0 {
					t.Fatalf("unchanged metadata sent %d requests", requests)
				}
			}
			if err := update(t.Context()); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("cached metadata bypassed execution authority: %v", err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err := update(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("cached metadata ignored cancellation: %v", err)
			}
			stale := fixture.leases[0]
			stale.FencingToken++
			if err := update(api.WithMutationAuthority(t.Context(), native.Scope(), stale, nil)); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("cached metadata bypassed fencing: %v", err)
			}
			requests = 0
			if err := sessions.UpdateSessionProviderIdentity(ctx, id, store.SessionProviderIdentity{ThreadID: provider.ThreadID}); err != nil {
				t.Fatal(err)
			}
			if requests != 0 {
				t.Fatalf("partial unchanged provider identity consumed %d requests", requests)
			}
			for _, kind := range []string{"worker", "provider"} {
				originalProcess, originalProvider := process, provider
				if kind == "worker" {
					process.PID++
				} else {
					provider.SessionID += "-uncertain"
				}
				loseNext = true
				if err := update(ctx); !errors.Is(err, api.ErrUnavailable) {
					t.Fatalf("%s committed write did not lose response: %v", kind, err)
				}
				var committed runnersession.Snapshot
				if err := client.Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+fmt.Sprint(id), nil, &committed); err != nil {
					t.Fatal(err)
				}
				if committed.Worker == nil || committed.Worker.PID != process.PID || committed.Provider != provider {
					t.Fatalf("%s uncertain write was not committed: %+v", kind, committed)
				}
				process, provider = originalProcess, originalProvider
				requests = 0
				if err := update(ctx); err != nil {
					t.Fatal(err)
				}
				if requests == 0 {
					t.Fatalf("%s stale acknowledgement hid the requested value after a committed write lost its response", kind)
				}
				if err := client.Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+fmt.Sprint(id), nil, &committed); err != nil {
					t.Fatal(err)
				}
				if committed.Worker == nil || committed.Worker.PID != process.PID || committed.Provider != provider {
					t.Fatalf("%s requested value did not reach the Hub: %+v", kind, committed)
				}
			}
			for _, kind := range []string{"worker", "provider"} {
				if kind == "worker" {
					process.PID++
				} else {
					provider.SessionID += "-changed"
				}
				rejectNext = true
				if err := update(ctx); err == nil {
					t.Fatalf("%s refusal was hidden", kind)
				}
				if err := update(ctx); err != nil {
					t.Fatalf("%s update was lost after refusal: %v", kind, err)
				}
			}
			provider.SessionID = "example-next-session"
			process.PID++
			process.CleanupPath += "-next"
			if err := update(ctx); err != nil {
				t.Fatal(err)
			}
			var persisted runnersession.Snapshot
			if err := client.Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+fmt.Sprint(id), nil, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.Provider != provider || persisted.Worker == nil || persisted.Worker.PID != process.PID || persisted.Worker.CleanupPath != process.CleanupPath {
				t.Fatalf("changed metadata was lost: %+v", persisted)
			}
		})
	}
}
