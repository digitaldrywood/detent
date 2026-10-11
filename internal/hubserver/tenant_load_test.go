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
	type load struct {
		fixture nativeFixture
		worker  string
		leases  []tracker.NativeLease
		items   []tracker.NativeIssue
		history int
	}
	prepare := func(t *testing.T, sessions, history int) load {
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
		return load{fixture: f, worker: worker, leases: leases, items: items, history: history}
	}
	run := func(t *testing.T, sessions, sample int, loads [2]load) [2]result {
		t.Logf("sessions=%d sample=%d paired load starting", sessions, sample)
		ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
		defer stop()
		var mu sync.Mutex
		latency := [2]map[string][]time.Duration{{}, {}}
		var failures []string
		record := func(history int, kind string, elapsed time.Duration, code, want int) {
			mu.Lock()
			defer mu.Unlock()
			latency[history][kind] = append(latency[history][kind], elapsed)
			if code != want {
				failures = append(failures, fmt.Sprintf("history=%d %s %d", loads[history].history, kind, code))
			}
		}
		var work sync.WaitGroup
		for index := range sessions {
			work.Go(func() {
				for round := 0; ctx.Err() == nil; round++ {
					for offset := range loads {
						history := (index + round + sample + offset) % len(loads)
						load := loads[history]
						f, lease := load.fixture, load.leases[index]
						started := time.Now()
						response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", load.worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90})
						record(history, "renew", time.Since(started), response.Code, http.StatusOK)
						started = time.Now()
						response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/validate", load.worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken})
						record(history, "validate", time.Since(started), response.Code, http.StatusOK)
						if round%4 == 0 {
							started = time.Now()
							response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(load.items[index].WorkItemID)+"/comments", f.token,
								tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("load-%d-%d-%d", sample, index, round)}, Body: "progress"})
							record(history, "comment", time.Since(started), response.Code, http.StatusOK)
						}
					}
					time.Sleep(time.Second)
				}
			})
		}
		work.Go(func() {
			for pair := 0; ctx.Err() == nil; pair++ {
				for offset := range loads {
					history := (pair + sample + offset) % len(loads)
					started := time.Now()
					tx, err := loads[history].fixture.service.database.db.BeginTx(context.Background(), nil)
					if err == nil {
						err = tx.Commit()
					}
					record(history, "probe", time.Since(started), map[bool]int{true: 0, false: 1}[err == nil], 0)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
		work.Wait()
		if len(failures) != 0 {
			t.Fatalf("requests failed: %v", failures[:min(len(failures), 5)])
		}
		var results [2]result
		for history, load := range loads {
			p95 := func(kind string) time.Duration {
				values := latency[history][kind]
				if len(values) == 0 {
					t.Fatalf("history=%d no %s requests completed", load.history, kind)
				}
				slices.Sort(values)
				return values[len(values)*95/100]
			}
			got := result{renew: p95("renew"), validate: p95("validate"), comment: p95("comment"), probe: p95("probe")}
			t.Logf("sessions=%d history=%d sample=%d requests renew=%d validate=%d comment=%d probe=%d p95 renew=%s validate=%s comment=%s writer_probe=%s",
				sessions, load.history, sample, len(latency[history]["renew"]), len(latency[history]["validate"]), len(latency[history]["comment"]), len(latency[history]["probe"]), got.renew, got.validate, got.comment, got.probe)
			results[history] = got
		}
		return results
	}
	for _, test := range []struct {
		name        string
		sessions    int
		scanHistory bool
	}{
		{name: "12 sessions", sessions: 12},
		{name: "50 sessions", sessions: 50},
		{name: "100 sessions", sessions: 100},
		{name: "history scan holds writer", sessions: 12, scanHistory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			loads := [2]load{prepare(t, test.sessions, 20000), prepare(t, test.sessions, 200000)}
			if test.scanHistory {
				for _, load := range loads {
					if _, err := load.fixture.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER tenant_load_history_scan AFTER UPDATE ON leases
BEGIN SELECT sum(length(data_json)) FROM collaboration_events; END`); err != nil {
						t.Fatal(err)
					}
				}
			}
			var smallSamples, largeSamples [3]time.Duration
			for sample := range smallSamples {
				results := run(t, test.sessions, sample, loads)
				smallSamples[sample], largeSamples[sample] = results[0].probe, results[1].probe
			}
			smallP95, largeP95, grew := tenantWriterWaitComparison(smallSamples, largeSamples)
			t.Logf("sessions=%d paired_writer_probe_p95 small=%v large=%v median_small=%s median_large=%s", test.sessions, smallSamples, largeSamples, smallP95, largeP95)
			if test.scanHistory {
				if !grew {
					t.Errorf("writer wait comparison missed history scans: %s at 20k events, %s at 200k", smallP95, largeP95)
				}
			} else if grew {
				t.Errorf("writer wait p95 grew with history: %s at 20k events, %s at 200k", smallP95, largeP95)
			}
		})
	}
}

func tenantWriterWaitComparison(small, large [3]time.Duration) (time.Duration, time.Duration, bool) {
	slices.Sort(small[:])
	slices.Sort(large[:])
	return small[1], large[1], large[1] > 3*small[1]+100*time.Millisecond
}

func TestTenantWriterWaitComparison(t *testing.T) {
	for _, test := range []struct {
		name         string
		small, large [3]time.Duration
		grew         bool
	}{
		{name: "flat", small: [3]time.Duration{10, 12, 11}, large: [3]time.Duration{11, 13, 12}},
		{name: "one quiet baseline", small: [3]time.Duration{1, 100, 110}, large: [3]time.Duration{320, 300, 310}},
		{name: "one slow large window", small: [3]time.Duration{10, 11, 12}, large: [3]time.Duration{5000, 10, 11}},
		{name: "sustained history growth", small: [3]time.Duration{10, 11, 12}, large: [3]time.Duration{140, 150, 160}, grew: true},
		{name: "one slow baseline cannot hide growth", small: [3]time.Duration{1000, 10, 11}, large: [3]time.Duration{200, 201, 202}, grew: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for index := range test.small {
				test.small[index] *= time.Millisecond
				test.large[index] *= time.Millisecond
			}
			_, _, grew := tenantWriterWaitComparison(test.small, test.large)
			if grew != test.grew {
				t.Fatalf("writer wait growth=%t, want %t", grew, test.grew)
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
