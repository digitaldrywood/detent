package hubserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatContentAccounting(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("limited=%t", limited), func(t *testing.T) {
			objects := &changeObjectFixture{}
			now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{Content: objects.call, now: func() time.Time { return now }})
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now,
				Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners"}}
			for i := range 4 {
				item := f.create(t, fmt.Sprintf("Accounting admission %d", i))
				view.Diagnostics.Admissions = append(view.Diagnostics.Admissions, runnerauth.DiagnosticAdmission{IssueID: string(item.WorkItemID), WorkItemRevision: int64(item.Revision), ObservedAt: now, Result: "skipped", Predicate: fmt.Sprintf("project_capacity_full_%d", i)})
			}
			f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
			f.service.database.hostedOrganization = f.project.OrganizationID
			limits := map[string]int64{"registered_runners": 100, "connected_runners": 100}
			var before int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if limited {
				limits["collaboration_bytes"] = before + 1
			}
			hostedTestPlans(t, f.service, limits)
			var hostname string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT hostname FROM machines WHERE id=?", runner.binding.MachineID).Scan(&hostname); err != nil {
				t.Fatal(err)
			}
			uploaded := make(map[string]int64)
			f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
				raw, err := objects.call(ctx, request)
				if err == nil && request.Write {
					uploaded[request.Reference.Key] = request.Reference.CompressedSize
					_, err = f.service.database.db.ExecContext(ctx, "UPDATE machines SET hostname='changed-runner' WHERE id=?", runner.binding.MachineID)
				}
				return raw, err
			}
			path := f.base + "/machines/" + string(runner.binding.MachineID) + "/heartbeat"
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "sprite_name": hostname, "project_configuration": view})
			if limited {
				if len(uploaded) != 0 {
					t.Errorf("consumption refusal uploaded %d objects", len(uploaded))
				}
				requireNativeStatus(t, response, http.StatusTooManyRequests)
				return
			}
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if len(uploaded) < len(view.Diagnostics.Admissions) {
				t.Fatalf("uploaded objects=%d, want at least %d", len(uploaded), len(view.Diagnostics.Admissions))
			}
			want := before
			for key, size := range uploaded {
				var registered int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT object_bytes FROM change_content_objects WHERE organization_id=? AND object_key=? AND billable=1", f.project.OrganizationID, key).Scan(&registered); err != nil || registered != size {
					t.Errorf("uploaded object registered bytes=%d want=%d err=%v", registered, size, err)
				}
				want += size
			}
			var used int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&used); err != nil || used != want {
				t.Fatalf("charged bytes=%d want=%d err=%v", used, want, err)
			}
		})
	}
}

func TestHeartbeatAndRenewWriterBudget(t *testing.T) {
	for _, test := range []struct {
		name       string
		admission  bool
		renew      bool
		hosted     bool
		concurrent bool
		statements int
	}{
		{name: "heartbeat", statements: 12},
		{name: "heartbeat admission", admission: true, statements: 30},
		{name: "heartbeat hosted admission", admission: true, hosted: true, statements: 50},
		{name: "heartbeat concurrent admission", admission: true, concurrent: true, statements: 40},
		{name: "renew", renew: true, statements: 12},
		{name: "hosted renew", renew: true, hosted: true, statements: 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			store := &memoryRunDetails{}
			f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			runner.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			item := f.create(t, "Writer budget")
			path := f.base + "/machines/" + string(runner.binding.MachineID) + "/heartbeat"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation}), http.StatusOK)
			var body any = map[string]any{"capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation}
			if test.admission {
				body = map[string]any{
					"capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation,
					"project_configuration": runnerauth.ProjectConfiguration{
						ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now,
						Diagnostics: &runnerauth.ProjectDiagnostics{
							ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners",
							Admissions: []runnerauth.DiagnosticAdmission{{IssueID: string(item.WorkItemID), WorkItemRevision: int64(item.Revision), ObservedAt: now, Result: "skipped", Predicate: "project_capacity_full"}},
						},
					},
				}
			}
			if test.renew {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, tracker.NativeClaim{
					PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "writer-budget",
					TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
				})
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				path = f.base + "/leases/" + string(lease.ID) + "/renew"
				body = tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}
			}
			f.service.tokenUseWork.Wait()
			if test.hosted {
				f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
				f.service.database.hostedOrganization = f.project.OrganizationID
				hostedTestPlans(t, f.service, nil)
				metrics, err := openRequestMetrics(t.Context(), filepath.Join(t.TempDir(), "requests.sqlite"), f.project.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				f.service.requestMetrics = metrics
				f.service.echo.Use(f.service.hostedBoundary)
			}
			var injected atomic.Bool
			if test.concurrent {
				store.mu.Lock()
				store.afterStore = func(ref attachment.ArchiveReference) {
					store.mu.Lock()
					raw := store.data[ref.Key]
					store.mu.Unlock()
					var payload storageJSONPayload
					var data tracker.CollaborationData
					if json.Unmarshal(raw, &payload) != nil || payload.Family != "scheduler.decision" || json.Unmarshal(payload.Data, &data) != nil || data.Decision == nil || !data.Decision.At.Equal(now) || !injected.CompareAndSwap(false, true) {
						return
					}
					inner := body.(map[string]any)
					view := inner["project_configuration"].(runnerauth.ProjectConfiguration)
					diagnostics := *view.Diagnostics
					diagnostics.Admissions = append([]runnerauth.DiagnosticAdmission{}, diagnostics.Admissions...)
					diagnostics.Admissions[0].ObservedAt = now.Add(-time.Second)
					diagnostics.Admissions[0].Predicate = "older_project_capacity_full"
					view.Diagnostics = &diagnostics
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "project_configuration": view}), http.StatusOK)
				}
				store.mu.Unlock()
			}
			var writer *writerTiming
			f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					err := next(c)
					writer = writerTimingFrom(c.Request().Context())
					return err
				}
			})
			statements := 0
			timing := &requestTiming{writerTrace: func(_ string, _ []driver.NamedValue) { statements++ }}
			request := newHubAPIRequest(t, http.MethodPost, path, runner.redemption.Credential, body)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			status := http.StatusOK
			if test.concurrent && !injected.Load() {
				t.Fatal("concurrent admission was not injected")
			}
			requireNativeStatus(t, response, status)
			if writer == nil {
				t.Fatal("missing writer timing")
			}
			t.Logf("writer_txs=%d writer_statements=%d", writer.txs.Load(), statements)
			wantTransactions := int64(1)
			if test.admission {
				wantTransactions++
			}
			if writer.txs.Load() != wantTransactions || statements > test.statements {
				t.Fatalf("writer_txs=%d (want %d), writer_statements=%d (bound %d)", writer.txs.Load(), wantTransactions, statements, test.statements)
			}
			if test.admission {
				var decisions int
				reason := "project_capacity_full"
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision' AND scheduler_source='native_runner_admission' AND scheduler_outcome='skipped' AND scheduler_reason=?", item.WorkItemID, reason).Scan(&decisions); err != nil {
					t.Fatal(err)
				}
				if decisions != 1 {
					t.Fatalf("admission decisions=%d, want 1", decisions)
				}
				if test.concurrent {
					var raw string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT "+collaborationPayloadColumn("")+" FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision' ORDER BY sequence DESC LIMIT 1", item.WorkItemID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var data tracker.CollaborationData
					if err := decodeRunHistory(f.service.runDetailContext(t.Context()), f.service.database.reader, raw, &data); err != nil {
						t.Fatal(err)
					}
					if decision := data.Decision; decision == nil || decision.Reason != reason || decision.Outcome != "skipped" || decision.RunnerID != runner.binding.RunnerID || !decision.At.Equal(now) {
						t.Fatalf("latest admission decision=%+v, want current outer observation for %q at %s", decision, reason, now)
					}
				}
			}
		})
	}
}
