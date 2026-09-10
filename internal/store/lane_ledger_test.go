package store

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
)

func TestLaneLedgerWithoutWorkerAttempt(t *testing.T) {
	t.Parallel()
	for _, result := range []string{"applied", "failed", "blocked"} {
		t.Run(result, func(t *testing.T) {
			t.Parallel()
			backend := openTestStore(t, t.Context())
			at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			identity := IssueIdentity{ProjectID: "project", IssueID: "issue"}
			if _, _, err := backend.LatestLaneWrite(t.Context(), identity); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing write: %v", err)
			}
			if _, err := backend.LaneObservation(t.Context(), identity); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing observation: %v", err)
			}
			first, err := backend.PrepareLaneWrite(t.Context(), "project", coordination.LaneWrite{Issue: "issue", From: "Backlog", To: "Todo", Reason: "admission", WrittenAt: at})
			if err != nil || first.InstanceIdentity == "" || first.FenceToken == 0 {
				t.Fatalf("prepare: %#v, %v", first, err)
			}
			if err := backend.ResolveLaneWrite(t.Context(), first.FenceToken, result); err != nil {
				t.Fatal(err)
			}
			latest, gotResult, err := backend.LatestLaneWrite(t.Context(), identity)
			if err != nil || latest != first || gotResult != result {
				t.Fatalf("latest: %#v, %s, %v", latest, gotResult, err)
			}
			second, err := backend.PrepareLaneWrite(t.Context(), "project", first)
			if err != nil || second.FenceToken <= first.FenceToken || second.InstanceIdentity != first.InstanceIdentity {
				t.Fatalf("monotonic token: %#v, %v", second, err)
			}
			observation := LaneObservation{State: "Todo", EnteredAt: at}
			if err := backend.SaveLaneObservation(t.Context(), identity, observation); err != nil {
				t.Fatal(err)
			}
			got, err := backend.LaneObservation(t.Context(), identity)
			if err != nil || got != observation {
				t.Fatalf("observation: %#v, %v", got, err)
			}
		})
	}
}
