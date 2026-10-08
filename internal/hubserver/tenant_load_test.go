package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestTenantServesRunnersWhileReadsAreSlow reproduces the 2026-10-08
// production collapse: slow dashboard reads held every reader connection,
// claims held the writer, and runner traffic queued behind both.
func TestTenantServesRunnersWhileReadsAreSlow(t *testing.T) {
	if testing.Short() {
		t.Skip("tenant load reproduction")
	}
	f := newNativeFixture(t, nil, "", "tenant-load")
	items := make([]tracker.NativeIssue, 0, 1000)
	for index := range cap(items) {
		items = append(items, f.create(t, fmt.Sprintf("load-%d", index)))
	}
	tests := []struct {
		name        string
		clients     int
		slowReaders int
		maxP95      time.Duration
	}{
		{name: "two runners with dashboards open", clients: 12, slowReaders: 8, maxP95: 250 * time.Millisecond},
		{name: "one hundred runners", clients: 100, slowReaders: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
			defer stop()
			var background sync.WaitGroup
			for range test.slowReaders {
				background.Go(func() {
					for ctx.Err() == nil {
						tx, err := f.service.database.reader.BeginTx(context.Background(), nil)
						if err != nil {
							return
						}
						var count int
						_ = tx.QueryRow("SELECT count(*) FROM issues").Scan(&count)
						select {
						case <-ctx.Done():
						case <-time.After(2 * time.Second):
						}
						_ = tx.Rollback()
					}
				})
			}
			background.Go(func() {
				for ctx.Err() == nil {
					tx, err := f.service.database.db.BeginTx(context.Background(), nil)
					if err != nil {
						return
					}
					time.Sleep(300 * time.Millisecond)
					_ = tx.Commit()
					time.Sleep(200 * time.Millisecond)
				}
			})
			var mu sync.Mutex
			var latencies []time.Duration
			var failures []string
			var clients sync.WaitGroup
			for client := range test.clients {
				clients.Go(func() {
					random := rand.New(rand.NewPCG(uint64(client), 1))
					for ctx.Err() == nil {
						item := items[random.IntN(len(items))]
						path := f.base + "/work-items/" + string(item.WorkItemID) + "/comments"
						if random.IntN(2) == 0 {
							path = f.base + "/work-items/" + string(item.WorkItemID) + "/runtime"
						}
						started := time.Now()
						response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
						elapsed := time.Since(started)
						mu.Lock()
						latencies = append(latencies, elapsed)
						if response.Code != http.StatusOK {
							failures = append(failures, fmt.Sprintf("%s %d", path, response.Code))
						}
						mu.Unlock()
						time.Sleep(50 * time.Millisecond)
					}
				})
			}
			clients.Wait()
			stop()
			background.Wait()
			if len(latencies) == 0 {
				t.Fatal("no runner requests completed")
			}
			slices.Sort(latencies)
			p50, p95, maxLatency := latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1]
			t.Logf("clients=%d slow_readers=%d requests=%d p50=%s p95=%s max=%s failures=%d", test.clients, test.slowReaders, len(latencies), p50, p95, maxLatency, len(failures))
			if len(failures) != 0 {
				t.Fatalf("runner requests failed: %v", failures[:min(len(failures), 5)])
			}
			if test.maxP95 != 0 && p95 > test.maxP95 {
				t.Fatalf("runner request p95 %s exceeds %s while slow reads hold readers", p95, test.maxP95)
			}
		})
	}
}

// TestTenantRunnerWritesStayFlatWithHistory drives the routine runner writes
// (lease renewal, lease validation, comments) under hosted plan accounting and
// requires how long a trivial write waits for the writer to stay flat as the
// tenant's history grows tenfold; route latencies are logged. Each session
// renews once a second, about thirty times a production runner's cadence.
func TestTenantRunnerWritesStayFlatWithHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("tenant write load")
	}
	type result struct{ renew, validate, comment, probe time.Duration }
	run := func(t *testing.T, sessions, history int) result {
		f := newNativeFixture(t, nil, "", "tenant-writes")
		approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
		worker := f.worker(t, "worker")
		f.service.config.Hosted = &HostedConfig{}
		f.service.database.hostedOrganization = f.project.OrganizationID
		hostedTestPlans(t, f.service, map[string]int64{"api_mutations": 1 << 40, "ingested_events": 1 << 40, "collaboration_bytes": 1 << 50, "history_records": 1 << 40,
			"concurrent_work": 10000, "connected_runners": 10000, "registered_runners": 10000, "unarchived_issues": 1 << 30})
		f.service.config.Hosted = nil
		leases := make([]tracker.NativeLease, 0, sessions)
		items := make([]tracker.NativeIssue, 0, sessions)
		for index := range sessions {
			issue := f.create(t, fmt.Sprintf("session-%d", index))
			items = append(items, issue)
			leases = append(leases, claimNativeAttempt(t, f, worker, fmt.Sprintf("machine-%d", index), fmt.Sprintf("session-%d", index), issue.WorkItemID))
		}
		pad := strings.Repeat("h", 200)
		if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT 'evt_history_'||i, ?, ?, ?, 1000000+i, 'comment.created', 1, '{"kind":"system"}', json_object('pad', ?), ? FROM n`,
			history, f.project.OrganizationID, f.project.ID, items[0].WorkItemID, pad, formatHubTime(time.Now())); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		warmReaderPools(t, f.service.database)
		ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
		defer stop()
		var mu sync.Mutex
		latency := map[string][]time.Duration{}
		var failures []string
		record := func(kind string, elapsed time.Duration, code, want int) {
			mu.Lock()
			defer mu.Unlock()
			latency[kind] = append(latency[kind], elapsed)
			if code != want {
				failures = append(failures, fmt.Sprintf("%s %d", kind, code))
			}
		}
		var work sync.WaitGroup
		for index, lease := range leases {
			work.Go(func() {
				for round := 0; ctx.Err() == nil; round++ {
					started := time.Now()
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90})
					record("renew", time.Since(started), response.Code, http.StatusOK)
					started = time.Now()
					response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/validate", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken})
					record("validate", time.Since(started), response.Code, http.StatusOK)
					if round%4 == 0 {
						started = time.Now()
						response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(items[index].WorkItemID)+"/comments", f.token,
							tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("load-%d-%d", index, round)}, Body: "progress"})
						record("comment", time.Since(started), response.Code, http.StatusOK)
					}
					time.Sleep(time.Second)
				}
			})
		}
		work.Go(func() {
			for ctx.Err() == nil {
				started := time.Now()
				tx, err := f.service.database.db.BeginTx(context.Background(), nil)
				if err == nil {
					err = tx.Commit()
				}
				record("probe", time.Since(started), map[bool]int{true: 0, false: 1}[err == nil], 0)
				time.Sleep(20 * time.Millisecond)
			}
		})
		work.Wait()
		if len(failures) != 0 {
			t.Fatalf("requests failed: %v", failures[:min(len(failures), 5)])
		}
		p95 := func(kind string) time.Duration {
			values := latency[kind]
			if len(values) == 0 {
				t.Fatalf("no %s requests completed", kind)
			}
			slices.Sort(values)
			return values[len(values)*95/100]
		}
		got := result{renew: p95("renew"), validate: p95("validate"), comment: p95("comment"), probe: p95("probe")}
		t.Logf("sessions=%d history=%d requests renew=%d validate=%d comment=%d p95 renew=%s validate=%s comment=%s writer_probe=%s",
			sessions, history, len(latency["renew"]), len(latency["validate"]), len(latency["comment"]), got.renew, got.validate, got.comment, got.probe)
		return got
	}
	for _, sessions := range []int{12, 50, 100} {
		t.Run(fmt.Sprintf("%d sessions", sessions), func(t *testing.T) {
			small := run(t, sessions, 20000)
			large := run(t, sessions, 200000)
			if large.probe > 3*small.probe+100*time.Millisecond {
				t.Errorf("writer wait p95 grew with history: %s at 20k events, %s at 200k", small.probe, large.probe)
			}
		})
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
