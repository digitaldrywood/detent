package hubserver

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
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
