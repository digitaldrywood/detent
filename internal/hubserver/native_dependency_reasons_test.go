package hubserver

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDependencyReasonCAS(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	dependent, blocker := f.create(t, "Dependent"), f.create(t, "Blocker")
	edge := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "edge"}, ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, edge), http.StatusOK)
	current := readWorkItem(t, f, dependent.WorkItemID, "")
	neighbor := readWorkItem(t, f, blocker.WorkItemID, "")
	if len(current.Blockers) != 1 || current.Blockers[0].Title != "Blocker" || current.Blockers[0].Number != blocker.Number || len(neighbor.Dependents) != 1 || neighbor.Dependents[0].Title != "Dependent" {
		t.Fatalf("dependency summaries: %+v %+v", current.Blockers, neighbor.Dependents)
	}
	path := f.base + "/work-items/" + string(dependent.WorkItemID) + "/dependency-reasons"
	var beforeSequence int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT MAX(sequence) FROM native_board_deltas").Scan(&beforeSequence); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, basis string
		revision    int64
		want        int
	}{
		{"stale basis", "outdated", 0, http.StatusConflict},
		{"stale revision", current.BlockedReason.CurrentBasis, 1, http.StatusConflict},
		{"current", current.BlockedReason.CurrentBasis, 0, http.StatusOK},
		{"second writer", current.BlockedReason.CurrentBasis, 0, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: test.name}, WorkItemID: dependent.WorkItemID, Kind: "blocked", Text: "Waits for the shared foundation.", Basis: test.basis, ExpectedRevision: test.revision}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), test.want)
		})
	}
	current = readWorkItem(t, f, dependent.WorkItemID, "")
	if current.BlockedReason.Text != "Waits for the shared foundation." || current.Revision != 2 || current.State != dependent.State {
		t.Fatalf("reason changed issue state or revision: %+v", current)
	}
	dirty, err := nativePageIDs(t.Context(), f.service.database.reader, "SELECT work_item_id FROM native_board_dirty ORDER BY work_item_id")
	if err != nil || !slices.Equal(dirty, []string{string(dependent.WorkItemID)}) {
		t.Fatalf("reason invalidated other items: %v err=%v", dirty, err)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := range 2 {
		wg.Go(func() {
			request := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("concurrent-%d", i)}, WorkItemID: blocker.WorkItemID, Kind: "unblocks", Text: "Enables the dependent feature.", Basis: neighbor.UnblocksReason.CurrentBasis, ExpectedRevision: 0}
			statuses <- performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request).Code
		})
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent writers: %v", counts)
	}
	var afterSequence int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT MAX(sequence) FROM native_board_deltas").Scan(&afterSequence); err != nil || afterSequence != beforeSequence {
		t.Fatalf("reason write synchronously projected cards: before=%d after=%d err=%v", beforeSequence, afterSequence, err)
	}
	for remaining := 1; remaining >= 0; remaining-- {
		more, err := f.service.database.drainNativeBoardDirty(t.Context(), f.service.config.now())
		if err != nil || !more {
			t.Fatalf("background card refresh: more=%v err=%v", more, err)
		}
		dirty, err := nativePageIDs(t.Context(), f.service.database.reader, "SELECT work_item_id FROM native_board_dirty")
		if err != nil || len(dirty) != remaining {
			t.Fatalf("background transaction refreshed more than one item: dirty=%v err=%v", dirty, err)
		}
	}
	edge.IdempotencyKey = "remove"
	edge.ExpectedRevision = current.Revision
	edge.Operation = "remove"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, edge), http.StatusOK)
	current = readWorkItem(t, f, dependent.WorkItemID, "")
	neighbor = readWorkItem(t, f, blocker.WorkItemID, "")
	if current.BlockedReason.Text != "" || neighbor.UnblocksReason.Text != "" || len(neighbor.Dependents) != 0 || current.BlockedReason.Basis == current.BlockedReason.CurrentBasis {
		t.Fatalf("stale reasons visible after removal: %+v %+v", current.BlockedReason, neighbor.UnblocksReason)
	}
}

func TestDependencyReasonPlainText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		text string
		want bool
	}{
		{"Waits for the settings foundation.", true}, {"Provides the HTTP v1.2 handlers.", true}, {"Uses ftp://example.test/files.", false}, {strings.Repeat("界", 140), true}, {strings.Repeat("界", 141), false}, {"<script>alert(1)</script>", false}, {"[settings](https://example.test)", false}, {"**Settings**", false}, {"Two\nlines", false}, {"First sentence. Second sentence.", false}, {" trailing ", false},
	} {
		t.Run(fmt.Sprintf("%q", test.text), func(t *testing.T) {
			if got := validDependencyReason(test.text); got != test.want {
				t.Fatalf("valid=%v want %v", got, test.want)
			}
		})
	}
}
