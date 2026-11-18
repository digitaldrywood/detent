package hubmaintenance

import (
	"errors"
	"testing"
)

type shareQueue struct {
	backlog int
	used    int
}

func (q *shareQueue) step(limit int) (int, error) {
	used := min(limit, q.backlog)
	q.backlog -= used
	q.used += used
	return used, nil
}

func endlessPages(used *int) PageSubstep {
	return func(limit int) (int, int, bool, error) {
		*used += limit
		return limit, limit, false, nil
	}
}

func TestShareItemsKeepsPagesAndBackfillProgressing(t *testing.T) {
	for _, test := range []struct {
		name                   string
		backfill, background   int
		steps                  int
		pagesIdle              bool
		wantBackfill, wantPage int
	}{
		{"continuous page arrivals still classify the reserve", 1 << 20, 1 << 20, 100, false, 200, 6000},
		{"continuous arrivals with idle conversion classify the whole slice", 1 << 20, 0, 100, false, 400, 6000},
		{"caught-up classification gives its reserve to conversion", 0, 1 << 20, 100, false, 0, 6000},
		{"caught-up background work gives the whole slice to pages", 0, 0, 100, false, 0, 6400},
		{"finite legacy backlog completes under continuous arrivals", 301, 1 << 20, 151, false, 301, 9060},
		{"idle pages hand leftover items to classification", 1 << 20, 0, 1, true, 64, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			backfill, background := &shareQueue{backlog: test.backfill}, &shareQueue{backlog: test.background}
			pageItems := 0
			pages := endlessPages(&pageItems)
			if test.pagesIdle {
				pages = func(int) (int, int, bool, error) { return 0, 0, true, nil }
			}
			for range test.steps {
				outcome, err := ShareItems(ItemLimit, backfill.step, background.step, pages)
				if err != nil {
					t.Fatal(err)
				}
				if outcome.Items > ItemLimit {
					t.Fatalf("step used %d items, budget %d", outcome.Items, ItemLimit)
				}
			}
			if backfill.used != test.wantBackfill || pageItems != test.wantPage {
				t.Fatalf("backfill=%d pages=%d want %d/%d", backfill.used, pageItems, test.wantBackfill, test.wantPage)
			}
			if test.background > 0 && test.steps > 0 && background.used < 2*test.steps && !test.pagesIdle {
				t.Fatalf("conversion starved: %d items over %d steps", background.used, test.steps)
			}
		})
	}
}

func TestShareItemsAccountsAndJoinsFailures(t *testing.T) {
	failure := errors.New("classification unavailable")
	pageItems := 0
	outcome, err := ShareItems(ItemLimit, func(int) (int, error) { return 0, failure }, (&shareQueue{backlog: 1}).step, endlessPages(&pageItems))
	if !errors.Is(err, failure) || pageItems != ItemLimit-1 || outcome.Items != ItemLimit || !outcome.Progress {
		t.Fatalf("failing backfill blocked other work: outcome=%+v pages=%d err=%v", outcome, pageItems, err)
	}
	if _, err := ShareItems(ItemLimit, func(limit int) (int, error) { return limit + 1, nil }, (&shareQueue{}).step, endlessPages(new(int))); err == nil {
		t.Fatal("substep overspending its limit was accepted")
	}
	idle, err := ShareItems(ItemLimit, (&shareQueue{}).step, (&shareQueue{}).step, func(int) (int, int, bool, error) { return 1, 0, true, nil })
	if err != nil || idle.Progress || idle.Items != 1 {
		t.Fatalf("idle step outcome=%+v err=%v", idle, err)
	}
}
