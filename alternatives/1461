package hubmaintenance

import (
	"errors"
	"testing"
)

func TestRefreshThenReplayStaysInsideItsAllocation(t *testing.T) {
	failure := errors.New("unavailable")
	for _, test := range []struct {
		name         string
		items        int
		refreshReady bool
		refreshErr   error
		events       int
		incompleteAt int
		replayErrAt  int
		wantItems    int
		wantReplays  int
		wantErr      error
	}{
		{name: "budget one refreshes only", items: 1, refreshReady: true, events: 5, wantItems: 1},
		{name: "budget two refreshes and replays one", items: 2, refreshReady: true, events: 5, wantItems: 2, wantReplays: 1},
		{name: "partially consumed budget", items: 3, refreshReady: true, events: 5, wantItems: 3, wantReplays: 2},
		{name: "full budget", items: ItemLimit, refreshReady: true, events: 100, wantItems: ItemLimit, wantReplays: ItemLimit - 1},
		{name: "not ready refresh stops", items: ItemLimit, events: 5, wantItems: 1},
		{name: "refresh error", items: ItemLimit, refreshErr: failure, events: 5, wantItems: 1, wantErr: failure},
		{name: "replay error after refresh", items: ItemLimit, refreshReady: true, events: 5, replayErrAt: 3, wantItems: 4, wantReplays: 2, wantErr: failure},
		{name: "partial replay", items: ItemLimit, refreshReady: true, events: 5, incompleteAt: 2, wantItems: 3, wantReplays: 1},
		{name: "nothing to replay", items: ItemLimit, refreshReady: true, wantItems: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			refreshes, limit, replays := 0, 0, 0
			outcome, err := RefreshThenReplay(test.items,
				func() (bool, error) {
					refreshes++
					return test.refreshReady, test.refreshErr
				},
				func(allowance int) ([]int, error) {
					limit = allowance
					events := make([]int, min(allowance, test.events))
					for i := range events {
						events[i] = i + 1
					}
					return events, nil
				},
				func(event int) (bool, error) {
					if event == test.replayErrAt {
						return false, failure
					}
					if event == test.incompleteAt {
						return false, nil
					}
					replays++
					return true, nil
				})
			if !errors.Is(err, test.wantErr) || outcome.Items != test.wantItems || replays != test.wantReplays {
				t.Fatalf("outcome=%+v replays=%d err=%v; want items=%d replays=%d err=%v", outcome, replays, err, test.wantItems, test.wantReplays, test.wantErr)
			}
			if outcome.Items > test.items || refreshes != 1 || refreshes+limit > test.items {
				t.Fatalf("allocation exceeded: items=%d refreshes=%d replay limit=%d budget=%d", outcome.Items, refreshes, limit, test.items)
			}
		})
	}
	if _, err := RefreshThenReplay(2, func() (bool, error) { return true, nil }, func(int) ([]int, error) { return []int{1, 2}, nil }, func(int) (bool, error) { return true, nil }); err == nil {
		t.Fatal("loader returning more events than the allowance was accepted")
	}
}
