package store

import (
	"testing"
	"time"
)

func TestReclaimActiveWorkAttemptsScope(t *testing.T) {
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
					_, err := backend.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: projectID, IssueID: phase, WorkerType: "agent", StartedAt: now.Add(-24 * time.Hour), Phase: phase})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			reclaimed, err := backend.ReclaimActiveWorkAttempts(t.Context(), WorkAttemptReclaim{ProjectID: tc.filter, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if len(reclaimed) != tc.want {
				t.Fatalf("reclaimed = %d, want %d", len(reclaimed), tc.want)
			}
			for _, attempt := range reclaimed {
				if attempt.IssueID != "running" || attempt.TerminalState != WorkAttemptTerminalAbandoned || attempt.ErrorClass != "service_restart" {
					t.Fatalf("unexpected reclaimed attempt: %+v", attempt)
				}
			}
			again, err := backend.ReclaimActiveWorkAttempts(t.Context(), WorkAttemptReclaim{ProjectID: tc.filter, Now: now})
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
