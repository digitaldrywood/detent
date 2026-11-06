package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupTimeoutWorkAttemptsScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, filter string
		want         int
	}{
		{name: "instance includes removed projects", want: 4},
		{name: "project restart stays scoped", filter: "configured", want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := openTestStore(t, t.Context())
			now := time.Now().UTC()
			for _, projectID := range []string{"configured", "removed"} {
				for _, phase := range []string{"running", "completion_deferred"} {
					_, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: projectID, IssueID: phase, WorkerType: "agent", StartedAt: now.Add(-24 * time.Hour), Phase: phase, LeaseExpiresAt: now.Add(-time.Hour)})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			reclaimed, err := backend.TimeoutExpiredWorkAttempts(t.Context(), WorkAttemptTimeout{ProjectID: tc.filter, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if len(reclaimed) != tc.want {
				t.Fatalf("reclaimed = %d, want %d", len(reclaimed), tc.want)
			}
			for _, attempt := range reclaimed {
				if attempt.TerminalState != WorkAttemptTerminalTimedOut || attempt.ErrorClass != "lease_expired" {
					t.Fatalf("unexpected reclaimed attempt: %+v", attempt)
				}
			}
			again, err := backend.TimeoutExpiredWorkAttempts(t.Context(), WorkAttemptTimeout{ProjectID: tc.filter, Now: now})
			if err != nil || len(again) != 0 {
				t.Fatalf("second reclaim = %+v, %v", again, err)
			}
			active, err := backend.ListActiveWorkAttempts(t.Context(), WorkAttemptQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 4-tc.want {
				t.Fatalf("active = %d", len(active))
			}
		})
	}
}

func TestTimeoutWorkAttemptsProcessEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, host, phase                 string
		expired, gone, excluded, terminal bool
		missingLease                      bool
	}{
		{name: "live lease", host: "local"},
		{name: "expired lease", host: "local", expired: true, terminal: true},
		{name: "confirmed gone", host: "local", gone: true, terminal: true},
		{name: "legacy local", gone: true, terminal: true},
		{name: "pool expired", host: "pool-a", expired: true, terminal: true},
		{name: "pool confirmed gone", host: "pool-a", gone: true, terminal: true},
		{name: "running exclusion wins", host: "local", expired: true, gone: true, excluded: true},
		{name: "expired deferred completion", host: "local", phase: "completion_deferred", expired: true, terminal: true},
		{name: "live deferred completion with gone worker", host: "local", phase: "completion_deferred", gone: true},
		{name: "expired deferred completion with gone worker", host: "local", phase: "completion_deferred", gone: true, expired: true, terminal: true},
		{name: "live deferred completion", host: "local", phase: "completion_deferred"},
		{name: "legacy completion without lease", host: "local", phase: "completion_deferred", missingLease: true, terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := openTestStore(t, t.Context())
			now := time.Now().UTC()
			lease := now.Add(time.Hour)
			if tc.expired {
				lease = now.Add(-time.Hour)
			}
			if tc.missingLease {
				lease = time.Time{}
			}
			id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{
				ProjectID: "removed", WorkerType: "agent", WorkerHost: tc.host, Phase: tc.phase,
				StartedAt: now.Add(-2 * time.Hour), LeaseExpiresAt: lease,
			})
			if err != nil {
				t.Fatal(err)
			}
			attrs := WorkAttemptTimeout{Now: now}
			if tc.gone {
				attrs.ConfirmedGoneAttemptIDs = []int64{id}
			}
			if tc.excluded {
				attrs.ExcludeAttemptIDs = []int64{id}
			}
			rows, err := backend.TimeoutExpiredWorkAttempts(t.Context(), attrs)
			if err != nil {
				t.Fatal(err)
			}
			if (len(rows) == 1) != tc.terminal {
				t.Fatalf("recovered %d attempts, want terminal=%v", len(rows), tc.terminal)
			}
		})
	}
}

func TestTimeoutWorkAttemptsCombinedParameterBoundary(t *testing.T) {
	for _, tc := range []struct {
		name           string
		confirmed      int
		wantBoundError bool
	}{
		{name: "exactly 30000 parameters", confirmed: 9990},
		{name: "30001 parameters reject without updates", confirmed: 9991, wantBoundError: true},
		{name: "30010 parameters reject without updates", confirmed: 10000, wantBoundError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "store.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Error(err)
				}
			})
			backend.matchProcess = func(int, string) (bool, error) { return true, nil }
			now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			if _, err := backend.db.ExecContext(t.Context(), `WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<10000)
INSERT INTO worker_generations(gen,pid,process_start,version,state,started_at,updated_at)
SELECT n,n,'example','test','active',?,? FROM ids`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			ids := make([]int64, 5)
			for i := range ids {
				project := "first"
				if i == 4 {
					project = "second"
				}
				id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: project, IssueID: "example", WorkerType: "agent", StartedAt: now.Add(-time.Hour), LeaseExpiresAt: now.Add(-time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
			}
			if _, err := backend.db.ExecContext(t.Context(), "UPDATE work_attempts SET owner_generation=1 WHERE id=?", ids[3]); err != nil {
				t.Fatal(err)
			}
			if _, err := backend.db.ExecContext(t.Context(), "UPDATE work_attempts SET lease_expires_at=NULL WHERE id=?", ids[0]); err != nil {
				t.Fatal(err)
			}
			excluded := make([]int64, 10000)
			confirmed := make([]int64, tc.confirmed)
			excluded[0], confirmed[0] = ids[1], ids[0]
			got, err := backend.TimeoutExpiredWorkAttempts(t.Context(), WorkAttemptTimeout{ProjectID: "first", Now: now, ExcludeAttemptIDs: excluded, ConfirmedGoneAttemptIDs: confirmed})
			if tc.wantBoundError {
				if err == nil || !strings.Contains(err.Error(), "query parameter bound") {
					t.Fatalf("error=%v, want query parameter bound", err)
				}
			} else if err != nil || len(got) != 2 {
				t.Fatalf("timed out=%v error=%v, want two attempts", got, err)
			}
			for i, id := range ids {
				var terminal bool
				if err := backend.db.QueryRowContext(t.Context(), "SELECT completed_at IS NOT NULL FROM work_attempts WHERE id=?", id).Scan(&terminal); err != nil {
					t.Fatal(err)
				}
				want := !tc.wantBoundError && (i == 0 || i == 2)
				if terminal != want {
					t.Errorf("attempt %d terminal=%v, want %v", i, terminal, want)
				}
			}
		})
	}
}
