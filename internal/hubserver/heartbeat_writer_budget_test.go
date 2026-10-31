package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
		{name: "heartbeat concurrent admission", admission: true, concurrent: true, statements: 30},
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
			if test.concurrent {
				status = http.StatusConflict
				if !injected.Load() {
					t.Fatal("concurrent admission was not injected")
				}
			}
			requireNativeStatus(t, response, status)
			if writer == nil {
				t.Fatal("missing writer timing")
			}
			t.Logf("writer_txs=%d writer_statements=%d", writer.txs.Load(), statements)
			if writer.txs.Load() != 1 || statements > test.statements {
				t.Fatalf("writer_txs=%d (want 1), writer_statements=%d (bound %d)", writer.txs.Load(), statements, test.statements)
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
			}
		})
	}
}
