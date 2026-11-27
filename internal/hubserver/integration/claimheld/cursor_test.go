package claimheld_test

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cursorPosition struct {
	lane, priority, aging   int64
	created                 time.Time
	state, label, unblocker int64
	rank, item              string
}

func (a cursorPosition) after(b cursorPosition) bool {
	for _, pair := range [][2]int64{{a.lane, b.lane}, {a.priority, b.priority}, {a.aging, b.aging}} {
		if pair[0] != pair[1] {
			return pair[0] > pair[1]
		}
	}
	if !a.created.Equal(b.created) {
		return a.created.After(b.created)
	}
	for _, pair := range [][2]int64{{a.state, b.state}, {a.label, b.label}, {a.unblocker, b.unblocker}} {
		if pair[0] != pair[1] {
			return pair[0] > pair[1]
		}
	}
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	return a.item > b.item
}

func (f heldFixture) cursor(t *testing.T, r hubfixture.Runner) (cursorPosition, bool) {
	t.Helper()
	var p cursorPosition
	err := f.Hub.Admin.QueryRowContext(t.Context(), `SELECT lane_rank,priority,aging_boost,created_at,state_rank,label_rank,unblocker_rank,rank,work_item_id FROM runner_selection_cursors WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, f.Organization(), r.Binding.RunnerID, f.Project.ID).
		Scan(&p.lane, &p.priority, &p.aging, &p.created, &p.state, &p.label, &p.unblocker, &p.rank, &p.item)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return p, true
}

func (f heldFixture) scanAs(t *testing.T, r hubfixture.Runner, session string) (tracker.NativeLease, *httptest.ResponseRecorder) {
	t.Helper()
	f.capacity(t, r)
	response := f.claimAs(t, r, "", session)
	var lease tracker.NativeLease
	if response.Code == http.StatusOK {
		hubfixture.Decode(t, response, &lease)
	} else if response.Code != http.StatusConflict {
		t.Fatalf("scan claim status=%d: %s", response.Code, response.Body.Bytes())
	}
	return lease, response
}

func (f heldFixture) unreviewedLandings(t *testing.T, count int) {
	t.Helper()
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", string(f.Hub.Config.InitialAdminToken), tracker.ApproveChangeReviewPolicy{
		Mutation: tracker.Mutation{IdempotencyKey: "require-review"}, Policy: tracker.ChangeReviewPolicy{PolicyID: hubfixture.Policy().ID, RequireReview: true},
	}), http.StatusOK)
	var linked []tracker.NativeWorkItemID
	for index := range count {
		issue := f.createIn(t, fmt.Sprintf("Unreviewed landing %d", index), 0, "Merging")
		linked = append(linked, issue.WorkItemID)
		if len(linked) < 32 && index+1 < count {
			continue
		}
		path := f.Base + "/work-items/" + string(linked[0]) + "/changes"
		response := f.Request(t, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change-" + string(linked[0])}, Title: "Pending review", LinkedIssues: linked[1:]})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var change tracker.ChangeRequest
		hubfixture.Decode(t, response, &change)
		c := hubfixture.Change{Native: f.Native, Change: change, Path: path + "/" + change.ID}
		c.Publish(t, "version-"+change.ID, "")
		linked = linked[:0]
	}
}

func TestScanCursorProgressesPastHydrationRefusals(t *testing.T) {
	t.Parallel()
	const refused = 230
	h := hubfixture.Open(t, hubfixture.Config{})
	f := newHeldProject(t, h, "", hubserver.HostedProjectStates())
	f.unreviewedLandings(t, refused)
	target := f.create(t, "Eligible after the landing prefix", 3)
	other := f.enroll(t)
	pages := (refused + 99) / 100

	var previous cursorPosition
	var claimed tracker.NativeLease
	for attempt := 1; attempt <= pages+1; attempt++ {
		if attempt == 2 {
			reopened := h.Reopen(t)
			f.Hub = reopened
		}
		lease, _ := f.scanAs(t, f.runner, fmt.Sprintf("progress-%d", attempt))
		if lease.ID != "" {
			claimed = lease
			if attempt < pages {
				t.Fatalf("claim %d reached the eligible item before scanning %d refused pages", attempt, pages-1)
			}
			break
		}
		position, found := f.cursor(t, f.runner)
		if !found || attempt > 1 && !position.after(previous) {
			t.Fatalf("claim %d cursor=%+v found=%v did not advance past %+v", attempt, position, found, previous)
		}
		previous = position
	}
	if claimed.WorkItemID != target.WorkItemID {
		t.Fatalf("eligible item not reached within %d claims: %+v", pages+1, claimed)
	}
	if position, _ := f.cursor(t, f.runner); position.item == "" {
		t.Fatal("successful claim lost the runner cursor")
	}

	before, _ := f.cursor(t, f.runner)
	if lease, _ := f.scanAs(t, other, "other-runner"); lease.ID != "" {
		t.Fatalf("second runner skipped its own prefix: %+v", lease)
	}
	if after, _ := f.cursor(t, f.runner); after != before {
		t.Fatalf("second runner moved the first runner's cursor: %+v -> %+v", before, after)
	}
	tenant := newHeldProject(t, h, hubfixture.Secondary, hubserver.HostedProjectStates())
	tenant.unreviewedLandings(t, 5)
	tenant.scanAs(t, tenant.runner, "tenant-runner")
	if after, _ := f.cursor(t, f.runner); after != before {
		t.Fatalf("another tenant moved the cursor: %+v -> %+v", before, after)
	}

	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(claimed.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: claimed.FencingToken, Reason: "released"}), http.StatusNoContent)
	var wrapped tracker.NativeLease
	for attempt := 1; attempt <= pages+2 && wrapped.ID == ""; attempt++ {
		wrapped, _ = f.scanAs(t, f.runner, fmt.Sprintf("wrap-%d", attempt))
	}
	if wrapped.WorkItemID != target.WorkItemID || wrapped.ID == claimed.ID {
		t.Fatalf("cursor did not wrap back to the eligible item: %+v", wrapped)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(wrapped.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: wrapped.FencingToken, Reason: "released"}), http.StatusNoContent)

	f.capacity(t)
	explicit := f.claimAs(t, f.runner, target.WorkItemID, "explicit-race")
	hubfixture.RequireStatus(t, explicit, http.StatusOK)
	var owner tracker.NativeLease
	hubfixture.Decode(t, explicit, &owner)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(owner.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: owner.FencingToken, Reason: "released"}), http.StatusNoContent)
	start := make(chan struct{})
	responses := make([]*httptest.ResponseRecorder, 2)
	var workers sync.WaitGroup
	for index := range responses {
		workers.Go(func() {
			<-start
			responses[index] = f.claimAs(t, f.runner, target.WorkItemID, fmt.Sprintf("race-%d", index))
		})
	}
	close(start)
	workers.Wait()
	leases := 0
	for _, response := range responses {
		if response.Code == http.StatusOK {
			leases++
		} else if response.Code != http.StatusConflict {
			t.Fatalf("race status=%d: %s", response.Code, response.Body.Bytes())
		}
	}
	if leases != 1 {
		t.Fatalf("concurrent claims granted %d leases", leases)
	}
}
