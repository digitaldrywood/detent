package hubserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestHeartbeatScaleBenchmark(t *testing.T) {
	f := newNativeFixture(t, nil, "", "heartbeat-scale")
	projects := []nativeFixture{f}
	for i := 1; i < 4; i++ {
		projects = append(projects, newNativeFixture(t, f.service, f.project.OrganizationID, fmt.Sprintf("heartbeat-scale-%d", i)))
	}
	runners := make([]runnerFixture, 3)
	for i := range runners {
		runners[i] = prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
		runners[i].enroll(t)
	}
	r := runners[0]
	now := f.service.config.now()
	at := formatHubTime(now)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 1500 {
		state := "Done"
		if i < 50 {
			state = "Todo"
		}
		result, err := tx.ExecContext(t.Context(), `INSERT INTO issues(workflow_state_id,native_id,organization_id,project_id,number,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,native_created_at,native_updated_at) VALUES((SELECT id FROM workflow_states WHERE project_id=? AND source_name=?),?,?,?,?,?,'','open','v1',?,?,?,?,?,?)`, f.project.ID, state, fmt.Sprintf("wi_scale_%d", i), f.project.OrganizationID, f.project.ID, i+1, "Example issue", at, at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		if i >= 300 {
			continue
		}
		issue, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("scale-attempt-%d", i)
		lease, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, issue, r.binding.MachineID, id, at, at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		fence, err := lease.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, id, r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,1,'interrupted','{}',?,?,?)`, id, f.project.OrganizationID, f.project.ID, fmt.Sprintf("wi_scale_%d", i), id, fence, id, `{"worktree_state":"dirty","resume":"checkpoint","ref":"refs/heads/example","sha":"`+strings.Repeat("a", 40)+`"}`, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, runner := range runners {
		for _, project := range projects {
			view := runnerauth.ProjectConfiguration{ProjectID: string(project.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now}
			response := performHubAPIRequest(t, f.service, http.MethodPost, project.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"capacity": 4, "version": "test", "project_configuration": view})
			requireNativeStatus(t, response, http.StatusOK)
		}
	}
	f.service.tokenUseWork.Wait()
	var writer *writerTiming
	f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			writer = writerTimingFrom(c.Request().Context())
			return err
		}
	})
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	totals := map[string]time.Duration{}
	var hold, db time.Duration
	var count int
	result := testing.Benchmark(func(b *testing.B) {
		totals = map[string]time.Duration{}
		hold, db, count = 0, 0, 0
		for b.Loop() {
			timing := &requestTiming{}
			previous, last := "", time.Now()
			timing.trace = func(_ string, _ []driver.NamedValue) {
				current := time.Now()
				if previous != "" {
					totals[previous] += current.Sub(last)
				}
				pcs := make([]uintptr, 48)
				n := runtime.Callers(2, pcs)
				frames := runtime.CallersFrames(pcs[:n])
				previous = "transaction_other"
				for {
					frame, more := frames.Next()
					for _, name := range []string{"readRunnerRoutingSnapshot", "mergeRunnerHostMetrics", "readRunnerClaimState", "readHeartbeatChanges", "runnerProjectConfiguration", "readGitHubBatchTask", "runnerDispatchFingerprint", "hostedConsumption", "checkHostedGrowth", "requireCredentialAuthority", "updateRunnerHeartbeat", "storeEncodedRunner", "updateRunnerProblems", "updateProviderReports", "storeRunnerUpdateObservation"} {
						if strings.Contains(frame.Function, name) {
							previous = name
						}
					}
					if !more {
						break
					}
				}
				last = time.Now()
			}
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 4, "version": "test", "change_cursor": "", "backend_isolation": r.redemption.BackendIsolation})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 300 {
				t.Fatalf("released executions = %+v", snapshot.ClaimState)
			}
			if writer == nil {
				t.Fatal("missing writer timing")
			}
			hold += time.Duration(writer.hold.Load())
			db += time.Duration(timing.db.Load())
			count++
		}
	})
	t.Logf("benchmark=%s issues=1500 nonterminal=50 released=300 runners=3 projects=4 writer_mean_ms=%.3f db_mean_ms=%.3f", result.String(), float64(hold)/float64(count)/1e6, float64(db)/float64(count)/1e6)
	names := make([]string, 0, len(totals))
	for name := range totals {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		if totals[a] > totals[b] {
			return -1
		}
		if totals[a] < totals[b] {
			return 1
		}
		return 0
	})
	for _, name := range names {
		t.Logf("operation=%s mean_ms=%.3f", name, float64(totals[name])/float64(count)/1e6)
	}
}

func TestHeartbeatResponseReaderSnapshot(t *testing.T) {
	for _, test := range []struct {
		name     string
		capacity int
		failRead bool
	}{{name: "committed observation", capacity: 2}, {name: "committed observation survives response failure", capacity: 1, failRead: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "heartbeat-reader")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 0, "version": "test"}), http.StatusOK)
			f.service.tokenUseWork.Wait()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := false
			timing := &requestTiming{}
			timing.trace = func(query string, _ []driver.NamedValue) {
				if observed || !strings.HasPrefix(query, "SELECT m.capacity, min(r.capacity_limit, r.reported_capacity)") {
					return
				}
				observed = true
				if f.service.database.db.Stats().InUse != 0 {
					t.Fatal("heartbeat response still holds the writer")
				}
				var capacity int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT reported_capacity FROM runner_identities WHERE id=?", r.binding.RunnerID).Scan(&capacity); err != nil {
					t.Fatal(err)
				}
				if capacity != test.capacity {
					t.Fatalf("committed capacity=%d want=%d", capacity, test.capacity)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET reported_capacity=0 WHERE id=?", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if test.failRead {
					cancel()
				}
			}
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": test.capacity, "version": "test"})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(ctx, timing)))
			if !observed {
				t.Fatal("response did not read claim state")
			}
			if test.failRead {
				if response.Code < 400 {
					t.Fatalf("canceled response status=%d", response.Code)
				}
			} else {
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				if snapshot.ClaimState == nil || snapshot.ClaimState.RunnerCapacity != test.capacity {
					t.Fatalf("response did not preserve reader snapshot: %+v", snapshot.ClaimState)
				}
			}
			var capacity int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT reported_capacity FROM runner_identities WHERE id=?", r.binding.RunnerID).Scan(&capacity); err != nil {
				t.Fatal(err)
			}
			if capacity != 0 {
				t.Fatalf("concurrent writer capacity=%d want=0", capacity)
			}
		})
	}
}
