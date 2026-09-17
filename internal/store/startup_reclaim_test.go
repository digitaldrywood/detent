package store

import (
	"testing"
	"time"
)

func TestStartupTimeoutWorkAttemptsScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, filter string
		want         int
	}{
		{name: "instance includes removed projects", want: 2},
		{name: "project restart stays scoped", filter: "configured", want: 1},
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
				if attempt.IssueID != "running" || attempt.TerminalState != WorkAttemptTerminalTimedOut || attempt.ErrorClass != "lease_expired" {
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
	}{
		{name: "live lease", host: "local"},
		{name: "expired lease", host: "local", expired: true, terminal: true},
		{name: "confirmed gone", host: "local", gone: true, terminal: true},
		{name: "legacy local", gone: true, terminal: true},
		{name: "remote expired", host: "remote", expired: true},
		{name: "remote evidence cannot override host", host: "remote", gone: true},
		{name: "running exclusion wins", host: "local", expired: true, gone: true, excluded: true},
		{name: "deferred completion wins", host: "local", phase: "completion_deferred", expired: true, gone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := openTestStore(t, t.Context())
			now := time.Now().UTC()
			lease := now.Add(time.Hour)
			if tc.expired {
				lease = now.Add(-time.Hour)
			}
			id, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{
				ProjectID: "removed", WorkerType: "agent", WorkerHost: tc.host, Phase: tc.phase,
				StartedAt: now.Add(-2 * time.Hour), LeaseExpiresAt: lease,
			})
			if err != nil {
				t.Fatal(err)
			}
			attrs := WorkAttemptTimeout{Now: now, WorkerHost: "local"}
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
