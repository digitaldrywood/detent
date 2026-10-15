package hubserver

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDependencyReasonCAS(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	dependent, blocker := f.create(t, "Dependent"), f.create(t, "Blocker")
	edge := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "edge"}, ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, edge), http.StatusOK)
	current := readWorkItem(t, f, dependent.WorkItemID, "")
	neighbor := readWorkItem(t, f, blocker.WorkItemID, "")
	if len(current.Blockers) != 1 || current.Blockers[0].Title != "Blocker" || current.Blockers[0].Number != blocker.Number || len(neighbor.Dependents) != 1 || neighbor.Dependents[0].Title != "Dependent" {
		t.Fatalf("dependency summaries: %+v %+v", current.Blockers, neighbor.Dependents)
	}
	path := f.base + "/work-items/" + string(dependent.WorkItemID) + "/dependency-reasons"
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
