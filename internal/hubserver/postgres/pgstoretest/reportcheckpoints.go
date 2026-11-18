package pgstoretest

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportcheckpoints"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RefreshReportCheckpoints(t testing.TB, owner any, organization tracker.OrganizationID) int {
	t.Helper()
	refresher, ok := owner.(reportcheckpoints.Refresher)
	if !ok {
		t.Fatalf("%T does not refresh report checkpoints", owner)
	}
	var cursor reportcheckpoints.Cursor
	for steps := 0; ; steps++ {
		next, processed, err := refresher.RefreshReportCheckpoints(t.Context(), organization, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !processed {
			return steps
		}
		cursor = next
	}
}
