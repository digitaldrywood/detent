package hubserver

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimSelectionStaysOffTheWriter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	open := func(t *testing.T) (*Service, int64, int64) {
		t.Helper()
		service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
		repositoryID, first := seedProjection(t, service.database.db)
		if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = ? WHERE id = ?", formatHubTime(now), repositoryID); err != nil {
			t.Fatal(err)
		}
		inserted, err := service.database.db.ExecContext(t.Context(), "INSERT INTO issues (repository_id, workflow_state_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) SELECT repository_id, workflow_state_id, 'I_second', 2, 'Second', 'https://example.test/2', github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at FROM issues WHERE id = ?", first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := inserted.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return service, first, second
	}
	query := claimCandidateQuery{Repositories: []string{"digitaldrywood/detent"}}
	claim := func(service *Service, machine tracker.MachineID, session string) (tracker.Lease, error) {
		return service.database.claimNext(t.Context(), tracker.ClaimRequest{MachineID: machine, SessionID: session, TTL: time.Minute}, query, time.Hour)
	}

	tests := []struct {
		name string
		run  func(t *testing.T, service *Service, first, second int64)
	}{
		{name: "a refusal with no candidate never takes the writer", run: func(t *testing.T, service *Service, first, second int64) {
			seedHubMachine(t, service, "machine-a", now)
			seedHubMachine(t, service, "machine-b", now)
			seedHubMachine(t, service, "machine-c", now)
			if _, err := claim(service, "machine-a", "session-a"); err != nil {
				t.Fatal(err)
			}
			if _, err := claim(service, "machine-b", "session-b"); err != nil {
				t.Fatal(err)
			}
			var before int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before); err != nil {
				t.Fatal(err)
			}
			writer, err := service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, claimErr := claim(service, "machine-c", "session-c")
				done <- claimErr
			}()
			select {
			case claimErr := <-done:
				if !errors.Is(claimErr, ErrNoClaimableWork) {
					t.Fatalf("refused claim error = %v, want no claimable work", claimErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("refused claim waited for the held writer")
			}
			if err := writer.Rollback(); err != nil {
				t.Fatal(err)
			}
			var after int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after); err != nil || after != before {
				t.Fatalf("refused claim wrote %d rows, err=%v", after-before, err)
			}
		}},
		{name: "concurrent claims never oversubscribe one host slot", run: func(t *testing.T, service *Service, first, second int64) {
			seedHubMachine(t, service, "machine-a", now)
			const contenders = 8
			var wait sync.WaitGroup
			errs := make(chan error, contenders)
			for index := range contenders {
				wait.Add(1)
				go func() {
					defer wait.Done()
					_, claimErr := claim(service, "machine-a", fmt.Sprintf("session-%d", index))
					errs <- claimErr
				}()
			}
			wait.Wait()
			close(errs)
			won := 0
			for claimErr := range errs {
				if claimErr == nil {
					won++
				} else if !errors.Is(claimErr, ErrNoClaimableWork) {
					t.Fatalf("claim error = %v", claimErr)
				}
			}
			var active int
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM leases WHERE machine_id = 'machine-a' AND released_at IS NULL").Scan(&active); err != nil {
				t.Fatal(err)
			}
			if won != 1 || active != 1 {
				t.Fatalf("winners = %d active leases = %d, want 1 and 1 for a capacity-1 host", won, active)
			}
		}},
		{name: "a selected candidate taken before commit moves to the next one", run: func(t *testing.T, service *Service, first, second int64) {
			seedHubMachine(t, service, "machine-a", now)
			seedHubMachine(t, service, "machine-b", now)
			var revision int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT revision FROM issues WHERE id = ?", first).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			taken, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(first), MachineID: "machine-a", SessionID: "session-a", TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.database.claimNextOn(t.Context(), service.database.db, nil, claimMode{hint: &claimSelected{id: tracker.WorkItemID(first), revision: revision}}, tracker.ClaimRequest{MachineID: "machine-b", SessionID: "session-b", TTL: time.Minute}, query, time.Hour)
			if !errors.Is(err, errClaimHintStale) {
				t.Fatalf("stale hint error = %v, want stale hint", err)
			}
			lease, err := claim(service, "machine-b", "session-b")
			if err != nil || lease.WorkItemID != tracker.WorkItemID(second) || taken.WorkItemID != tracker.WorkItemID(first) {
				t.Fatalf("claim after stale hint = %+v, %v; want the second work item", lease, err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, first, second := open(t)
			test.run(t, service, first, second)
		})
	}
}
