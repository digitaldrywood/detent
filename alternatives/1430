package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatTransactionTiming(t *testing.T) {
	for _, durable := range []bool{true, false} {
		t.Run(fmt.Sprintf("durable=%t", durable), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "heartbeat-timing")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			if !durable {
				if _, err := f.service.database.db.ExecContext(t.Context(), "PRAGMA synchronous=OFF"); err != nil {
					t.Fatal(err)
				}
			}
			f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
			f.service.database.hostedOrganization = f.project.OrganizationID
			hostedTestPlans(t, f.service, nil)
			now := f.service.config.now()
			descriptor := hubTestPolicy()
			descriptor.Configuration = &policy.Configuration{Behavior: json.RawMessage(`{"enabled":true}`), Prompt: strings.Repeat("Example workflow instructions. ", 2280)}
			descriptor = descriptor.WithID()
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, SelectedPolicy: &descriptor, EffectivePolicy: &descriptor, LocalBindingPolicy: &descriptor, RestrictedBindingPolicy: &descriptor, Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners"}}
			for i := range 32 {
				view.Diagnostics.Records = append(view.Diagnostics.Records, runnerauth.DiagnosticAttempt{Key: fmt.Sprintf("local:%d", i+1), IssueID: fmt.Sprintf("wi_example_%d", i), Membership: []string{"running"}, LeaseExpiresAt: now.Add(time.Hour), LatestError: "private diagnostic error"})
				view.Diagnostics.Admissions = append(view.Diagnostics.Admissions, runnerauth.DiagnosticAdmission{IssueID: fmt.Sprintf("wi_example_%d", i), WorkItemRevision: 1, ObservedAt: now, Result: "skipped", Predicate: "project_capacity_full"})
			}
			if err := view.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("configuration_bytes=%d records=%d admissions=%d", len(raw), len(view.Diagnostics.Records), len(view.Diagnostics.Admissions))
			var writer *writerTiming
			f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					err := next(c)
					writer = writerTimingFrom(c.Request().Context())
					return err
				}
			})
			f.service.echo.Use(f.service.hostedBoundary)
			build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			update := runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Running: build, ObservedAt: now}
			update.Revision = update.BuildRevision()
			capacity := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: now}
			admission := tracker.NativeAdmissionObservation{RunnerRevision: 1, Context: tracker.NativeAdmissionContext{PolicyID: descriptor.ID, ObservedAt: now}}
			checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: "passed", Provider: "passed", ObservedAt: now}
			reportedSkills := []skills.ProviderSkill{{Name: "review", Description: "Review this project", Path: "/project/.agents/skills/review/SKILL.md", Scope: "project", Enabled: true, UserInvocable: true, Invocation: "$review"}}
			var holds, dbs []int64
			gaps := map[string]time.Duration{}
			var txs int64
			var commit time.Duration
			for i := range 31 {
				timing := &requestTiming{}
				last, previousDB := time.Now(), int64(0)
				timing.trace = func(query string, _ []driver.NamedValue) {
					current := time.Now()
					db := timing.db.Load()
					key := strings.Join(strings.Fields(query), " ")
					if len(key) > 100 {
						key = key[:100]
					}
					if i > 0 {
						gaps[key] += current.Sub(last) - time.Duration(db-previousDB)
					}
					last, previousDB = current, db
				}
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": build.Version, "os": build.OS, "architecture": build.Architecture, "protocol_major": 2, "change_cursor": "", "update": update, "capacity_configuration": capacity, "admission": admission, "local_checks": checks, "skills": reportedSkills, "provider_reports": []providercapacity.Report{capacityReport(now)}, "host_metrics": []hostmetrics.Summary{testHostSummary(now)}, "backend_isolation": r.redemption.BackendIsolation, "project_configuration": view})
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
				requireNativeStatus(t, response, http.StatusOK)
				if writer == nil {
					t.Fatal("missing writer timing")
				}
				if i > 0 {
					for _, span := range timing.spans {
						if span.name == "tx_commit" {
							commit += span.duration
						}
					}
					holds = append(holds, writer.hold.Load())
					dbs = append(dbs, timing.db.Load())
					txs += writer.txs.Load()
				}
			}
			t.Logf("commit_mean_ms=%.3f", float64(commit)/30e6)
			slices.Sort(holds)
			var holdTotal, dbTotal int64
			for i := range holds {
				holdTotal += holds[i]
				dbTotal += dbs[i]
			}
			t.Logf("heartbeats=%d hold_mean_ms=%.3f hold_p50_ms=%.3f hold_p90_ms=%.3f db_mean_ms=%.3f writer_txs=%.1f", len(holds), float64(holdTotal)/30e6, float64(holds[15])/1e6, float64(holds[27])/1e6, float64(dbTotal)/30e6, float64(txs)/30)
			type gap struct {
				query    string
				duration time.Duration
			}
			var ordered []gap
			for query, duration := range gaps {
				ordered = append(ordered, gap{query, duration})
			}
			slices.SortFunc(ordered, func(a, b gap) int {
				if a.duration > b.duration {
					return -1
				}
				if a.duration < b.duration {
					return 1
				}
				return 0
			})
			for _, item := range ordered[:min(8, len(ordered))] {
				t.Logf("non_db_before_ms=%.3f query=%s", float64(item.duration)/30e6, item.query)
			}
		})
	}
}

func TestHeartbeatPreparationBeforeWriter(t *testing.T) {
	f := newNativeFixture(t, nil, "", "heartbeat-preparation")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 2, "version": "test"}), http.StatusOK)
	f.service.tokenUseWork.Wait()
	var writer *writerTiming
	f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			writer = writerTimingFrom(c.Request().Context())
			return err
		}
	})
	now := f.service.config.now()
	valid := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now}
	badPolicy := hubTestPolicy()
	badPolicy.ID = "invalid"
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"invalid policy digest", "project_configuration", func() runnerauth.ProjectConfiguration { view := valid; view.SelectedPolicy = &badPolicy; return view }()},
		{"foreign configuration", "project_configuration", func() runnerauth.ProjectConfiguration { view := valid; view.ProjectID = "prj_foreign"; return view }()},
		{"invalid diagnostics", "project_configuration", func() runnerauth.ProjectConfiguration {
			view := valid
			view.Diagnostics = &runnerauth.ProjectDiagnostics{}
			return view
		}()},
		{"invalid admission", "admission", tracker.NativeAdmissionObservation{}},
		{"invalid capacity", "capacity_configuration", runnerauth.CapacityConfig{}},
		{"invalid local checks", "local_checks", runnerauth.LocalChecks{}},
		{"invalid skills", "skills", []skills.ProviderSkill{{Name: ""}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", test.field: test.value})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if writer == nil || writer.txs.Load() != 0 {
				t.Fatalf("invalid preparation acquired writer: %+v", writer)
			}
		})
	}
}
