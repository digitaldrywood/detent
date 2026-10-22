package hubserver

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantServesRunnersWhileReadsAreSlow(t *testing.T) {
	f := newNativeFixture(t, nil, "", "tenant-load")
	items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
	worker := f.worker(t, "worker")
	ctx := testenv.WaitContext(t, t.Context())
	held, err := f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	if _, err := held.ExecContext(ctx, "UPDATE projects SET name=name"); err != nil {
		t.Fatal(err)
	}
	var readers []*sql.Conn
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()
	for range f.service.database.reader.Stats().MaxOpenConnections {
		reader, err := f.service.database.reader.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, reader)
	}
	for _, token := range []string{f.token, worker} {
		credential, status, err := f.service.authenticateAPIToken(ctx, token, "", "")
		if err != nil || status != http.StatusOK || credential.ID == "" {
			t.Fatalf("authentication with occupied reader and writer pools: status=%d credential=%q error=%v", status, credential.ID, err)
		}
	}
	for _, reader := range readers {
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
	readers = nil
	for _, item := range items {
		for _, suffix := range []string{"comments", "runtime"} {
			request := newHubAPIRequest(t, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/"+suffix, f.token, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusOK)
		}
	}
}

func TestTenantRunnerWritesStayFlatWithHistory(t *testing.T) {
	f := newNativeFixture(t, nil, "", "tenant-writes")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "worker")
	f.service.config.Hosted = &HostedConfig{}
	f.service.database.hostedOrganization = f.project.OrganizationID
	hostedTestPlans(t, f.service, map[string]int64{"api_mutations": 1 << 40, "ingested_events": 1 << 40, "collaboration_bytes": 1 << 50, "history_records": 1 << 40,
		"concurrent_work": 10000, "connected_runners": 10000, "registered_runners": 10000, "unarchived_issues": 1 << 30})
	f.service.config.Hosted = nil
	items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
	var leases []tracker.NativeLease
	for index, item := range items {
		leases = append(leases, claimNativeAttempt(t, f, worker, fmt.Sprintf("machine-%d", index), fmt.Sprintf("session-%d", index), item.WorkItemID))
	}
	scanned := []string{"collaboration_events", "native_attempts", "attempt_usage", "leases", "analytics_hourly_rollups", "analytics_rollup_positions"}
	t.Run("history scan is rejected", func(t *testing.T) {
		statement := "SELECT sum(length(data_json)) FROM collaboration_events"
		violations := 0
		for _, detail := range explainQueryPlan(t, f.service.database.db, statement, nil) {
			if analyticsCommitPlanViolation(detail, sqlTableAliases(statement), scanned) != "" {
				violations++
			}
		}
		if violations == 0 {
			t.Fatal("plan check accepted a full history scan")
		}
	})
	counts := map[string]int{}
	previous := 0
	for _, history := range []int{20000, 200000} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
				for index, item := range items {
					var sequence int
					if err := tx.QueryRowContext(t.Context(), "SELECT event_sequence FROM issues WHERE organization_id=? AND project_id=? AND native_id=?", f.project.OrganizationID, f.project.ID, item.WorkItemID).Scan(&sequence); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT ? UNION ALL SELECT i+1 FROM n WHERE i < ?)
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT 'evt_history_'||?||'_'||i, ?, ?, ?, ?+i, 'comment.created', 1, '{"kind":"system"}', json_object('pad', ?), ? FROM n`,
						previous+1, history, index, f.project.OrganizationID, f.project.ID, item.WorkItemID, sequence-previous, strings.Repeat("h", 200), formatHubTime(f.service.config.now())); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET event_sequence=? WHERE organization_id=? AND project_id=? AND native_id=?", sequence+history-previous, f.project.OrganizationID, f.project.ID, item.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			})
			for index, item := range items {
				lease := leases[index]
				for _, operation := range []struct {
					name, path, token string
					payload           any
				}{
					{"renew", "/leases/" + string(lease.ID) + "/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}},
					{"validate", "/leases/" + string(lease.ID) + "/validate", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}},
					{"comment", "/work-items/" + string(item.WorkItemID) + "/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-%d-%d", history, index)}, Body: "progress"}},
				} {
					var mu sync.Mutex
					var statements []tracedStatement
					timing := &requestTiming{trace: func(query string, args []driver.NamedValue) {
						values := make([]any, 0, len(args))
						for _, arg := range args {
							if arg.Name != "" {
								values = append(values, sql.Named(arg.Name, arg.Value))
							} else {
								values = append(values, arg.Value)
							}
						}
						mu.Lock()
						statements = append(statements, tracedStatement{query, values})
						mu.Unlock()
					}}
					request := newHubAPIRequest(t, http.MethodPost, f.base+operation.path, operation.token, operation.payload).WithContext(withRequestTiming(t.Context(), timing))
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
					f.service.tokenUseWork.Wait()
					mu.Lock()
					traced := append([]tracedStatement(nil), statements...)
					mu.Unlock()
					key := fmt.Sprintf("%s/%d", operation.name, index)
					if len(traced) == 0 {
						t.Fatal("no request statements were traced")
					}
					if previous == 0 {
						counts[key] = len(traced)
					} else if len(traced) > counts[key] {
						t.Errorf("%s queries grew with history: %d -> %d", key, counts[key], len(traced))
					}
					for _, statement := range traced {
						head := strings.ToUpper(strings.TrimSpace(statement.query))
						if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") && !strings.HasPrefix(head, "INSERT") && !strings.HasPrefix(head, "UPDATE") && !strings.HasPrefix(head, "DELETE") {
							continue
						}
						tables := sqlTableAliases(statement.query)
						for _, table := range scanned {
							tables[table] = table
						}
						for _, detail := range explainQueryPlan(t, f.service.database.db, statement.query, statement.args) {
							if violation := analyticsCommitPlanViolation(detail, tables, scanned); violation != "" {
								t.Errorf("%s %s: %s\nstatement: %s", key, violation, detail, statement.query)
							}
						}
					}
					t.Logf("history=%d %s queries=%d", history, key, len(traced))
				}
			}
		})
		previous = history
	}
}

// warmReaderPools opens every reader connection once so the measurement sees
// a long-running Hub, not each connection's first schema parse.
func warmReaderPools(t *testing.T, d *database) {
	t.Helper()
	for _, pool := range []*sql.DB{d.reader, d.auth()} {
		connections := make([]*sql.Conn, 0, pool.Stats().MaxOpenConnections)
		for range cap(connections) {
			connection, err := pool.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := connection.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema").Scan(&count); err != nil {
				t.Fatal(err)
			}
			connections = append(connections, connection)
		}
		for _, connection := range connections {
			if err := connection.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
