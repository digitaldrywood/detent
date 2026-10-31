package hubserver

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	drainTestNativeBoard(t, f.service)
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
	var cursor string
	for remaining := 1; remaining >= 0; remaining-- {
		more, id, err := f.service.database.drainNativeBoardDirtyAfter(t.Context(), f.service.config.now(), cursor)
		if err != nil || !more {
			t.Fatalf("background card refresh: more=%v err=%v", more, err)
		}
		cursor = id
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

func TestNativeDependencyBatchReadCost(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	blocker := f.create(t, "Shared foundation")
	dependent := f.create(t, "Dependent feature")
	if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE copies(n) AS (
 SELECT 3 UNION ALL SELECT n+1 FROM copies WHERE n<2000)
 INSERT INTO issues(native_id,organization_id,project_id,number,workflow_state_id,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at)
 SELECT 'wi_batch_' || n,organization_id,project_id,n,workflow_state_id,'Dependent feature ' || n,'','open','1',source_updated_at,synchronized_at,created_at,updated_at
 FROM issues,copies WHERE native_id=?`, dependent.WorkItemID); err != nil {
		t.Fatal(err)
	}
	now := formatHubTime(time.Now())
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issue_dependencies(blocker_issue_id,dependent_issue_id,provenance,created_at,updated_at)
 SELECT b.id,d.id,'native',?,? FROM issues b JOIN issues d ON d.project_id=b.project_id AND d.id<>b.id WHERE b.native_id=?`, now, now, blocker.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issue_relationships(first_issue_id,second_issue_id,relationship_type,created_at)
 SELECT b.id,d.id,'related',? FROM issues b JOIN issues d ON d.project_id=b.project_id AND d.id>b.id WHERE b.native_id=?`, now, blocker.WorkItemID); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	for _, size := range []int{2, 2000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			issues := make([]*tracker.NativeIssue, size)
			issues[0] = &tracker.NativeIssue{NativeReference: blocker.NativeReference}
			issues[1] = &tracker.NativeIssue{NativeReference: dependent.NativeReference}
			for index := 2; index < size; index++ {
				issues[index] = &tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_batch_%d", index+1)), ProjectID: f.project.ID}}
			}
			q := &runtimeReadQuery{nativeQueryer: f.service.database.reader}
			start := time.Now()
			if err := readNativeDependencyDetailsBatch(t.Context(), q, scope, issues); err != nil {
				t.Fatal(err)
			}
			t.Logf("hydrated %d issues in %s with %d queries", size, time.Since(start), len(q.statements))
			if len(q.statements) != 3 {
				t.Fatalf("dependency query count scales with cards: %d", len(q.statements))
			}
			if len(issues[0].Dependents) != 1999 || !issues[0].DependencyReady || len(issues[0].Blockers) != 0 {
				t.Fatalf("blocker hydration: dependents=%d ready=%v", len(issues[0].Dependents), issues[0].DependencyReady)
			}
			for _, issue := range issues[1:] {
				if len(issue.Blockers) != 1 || issue.Blockers[0].ID != blocker.WorkItemID || issue.Blockers[0].Title != "Shared foundation" || issue.DependencyReady || len(issue.Related) != 1 || issue.Related[0].ID != blocker.WorkItemID {
					t.Fatalf("dependent hydration: %+v", issue)
				}
			}
		})
	}
}
