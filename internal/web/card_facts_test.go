package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBoardCardFactsAPI(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	count := int64(75)
	for _, tt := range []struct {
		name    string
		running bool
	}{{"no session", false}, {"running", true}} {
		t.Run(tt.name, func(t *testing.T) {
			issue := telemetry.Issue{ID: "i", ProjectID: "p", State: "Rework", AttemptsToday: &count, LaneReason: "workpad_status_invalid", LaneReasonAt: &at, PullRequest: &telemetry.PullRequest{HeadSHA: "head", HeadCommittedAt: &at, CIStatus: "success", MergeableState: "clean"}}
			snapshot := telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}, Pipeline: []telemetry.Issue{issue}}
			if tt.running {
				snapshot.Running = []telemetry.Running{{Issue: issue, StartedAt: at, Tokens: telemetry.Tokens{Total: 42}}}
			}
			response := boardResponse(snapshot)
			if len(response.Cards) != 1 {
				t.Fatalf("cards = %+v", response.Cards)
			}
			encoded, err := json.Marshal(response.Cards[0])
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]any{"head_sha": "head", "head_committed_at": "2026-09-14T12:00:00Z", "ci": "green", "mergeability": "clean", "attempts_today": float64(75), "lane_reason": "workpad_status_invalid", "lane_reason_at": "2026-09-14T12:00:00Z"} {
				if fields[key] != want {
					t.Errorf("%s = %v, want %v", key, fields[key], want)
				}
			}
			if (fields["session_started_at"] != nil) != tt.running {
				t.Fatalf("session = %v", fields["session_started_at"])
			}
		})
	}
}

type cardHistoryTestStore struct {
	store.Store
	calls int
}

func (s *cardHistoryTestStore) IssueCardHistory(_ context.Context, _ store.IssueIdentity, _ time.Time) (store.CardHistory, error) {
	s.calls++
	return store.CardHistory{AttemptsToday: 75, LaneReason: "merge_conflict"}, nil
}

func TestSnapshotCardHistoryRuntimeAndCopies(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"", "OPEN", "Rework"} {
		t.Run(state, func(t *testing.T) {
			backend := &cardHistoryTestStore{}
			server := &Server{store: backend, now: func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }}
			snapshot := telemetry.Snapshot{Project: telemetry.Project{ID: "p"}, Running: []telemetry.Running{{Issue: telemetry.Issue{ID: "i", State: state}}}}
			got := server.snapshotCardHistory(t.Context(), snapshot)
			if backend.calls != 1 || got.Running[0].AttemptsToday == nil || *got.Running[0].AttemptsToday != 75 {
				t.Fatalf("enrichment = %+v, calls %d", got.Running[0].Issue, backend.calls)
			}
			if snapshot.Running[0].AttemptsToday != nil || snapshot.Running[0].LaneReason != "" {
				t.Fatal("enrichment mutated original snapshot")
			}
			cards := boardResponse(got).Cards
			if len(cards) != 1 || cards[0].AttemptsToday == nil || *cards[0].AttemptsToday != 75 {
				t.Fatalf("API cards = %+v", cards)
			}
		})
	}
}

func TestCardHistoryConfiguredLanes(t *testing.T) {
	for _, tt := range []struct {
		lane string
		want int
	}{{"Production", 1}, {"Rework", 1}, {"Merging", 1}, {"Done", 0}} {
		t.Run(tt.lane, func(t *testing.T) {
			backend := &cardHistoryTestStore{}
			server := &Server{store: backend, now: time.Now}
			server.kanbanWorkflow.Tracker.ActiveStates = []string{"Todo", "Production"}
			snapshot := telemetry.Snapshot{Project: telemetry.Project{ID: "p"}, BoardIssues: []telemetry.Issue{{ID: "i", State: tt.lane}}}
			got := server.snapshotCardHistory(t.Context(), snapshot)
			if backend.calls != tt.want || len(boardResponse(got).Cards) != tt.want {
				t.Fatalf("calls=%d cards=%+v", backend.calls, boardResponse(got).Cards)
			}
		})
	}
}
