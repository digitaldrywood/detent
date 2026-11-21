package runnerruntime_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresBoardAndExplainUseCurrentLease(t *testing.T) {
	for _, name := range []string{"renewal before projection snapshot", "expiry before work limit and dirty lane counts"} {
		t.Run(name, func(t *testing.T) {
			testPostgresBoardLeaseReads(t, name == "renewal before projection snapshot")
		})
	}
}

func testPostgresBoardLeaseReads(t *testing.T, renewDuringProjection bool) {
	cfg := hubfixture.StartupConfig()
	var renew atomic.Bool
	var f hubfixture.LeasedRunners
	cfg.Clock = func() time.Time {
		at := time.Now().UTC()
		if renew.CompareAndSwap(true, false) {
			var renewed time.Time
			err := f.Admin.QueryRowContext(t.Context(), "UPDATE leases SET renewed_at=clock_timestamp(), expires_at=clock_timestamp()+interval '2 hours' WHERE organization_id=$1 AND lease_id=$2 AND fencing_token=$3 AND released_at IS NULL AND expires_at>clock_timestamp() RETURNING renewed_at", f.Organization, f.Leases[0].ID, f.Leases[0].FencingToken).Scan(&renewed)
			if err != nil || !renewed.After(at) {
				t.Errorf("same-fence renewal after refresh clock read: renewed=%v sampled=%v err=%v", renewed, at, err)
			}
		}
		return at
	}
	f = hubfixture.SeedLeasedRunners(t, hubfixture.OpenStartup(t, cfg), hubfixture.StartupOrganization)
	hosted := &hubfixture.Hosted{Hub: f.Hub, Provider: f.Hub.Config.Hosted.Provider.(*hubfixture.HostedProvider)}
	viewer := hosted.User(t, "viewer", "viewer", "viewer@example.test", "", "")
	hosted.Grant(t, viewer, "prj_runner", false, false)
	f.Hub.RefreshPages(t, f.Organization)
	renew.Store(renewDuringProjection)
	f.Hub.RefreshBoard(t, f.Organization)
	if renew.Load() {
		t.Fatal("refresh did not reach renewal barrier")
	}
	if renewDuringProjection {
		if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.BoardItem(t.Context(), "prj_runner", f.Leases[0].WorkItemID)
			if err == nil && (!row.Running || row.ExecutionFence != int64(f.Leases[0].FencingToken) || row.Display.Attempt == nil || row.Display.Attempt.Status != "running") {
				t.Errorf("renewed projection: running=%v fence=%d attempt=%+v", row.Running, row.ExecutionFence, row.Display.Attempt)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Admin.ExecContext(t.Context(), "UPDATE leases SET expires_at=clock_timestamp() WHERE organization_id=$1 AND lease_id=$2", f.Organization, f.Leases[1].ID); err != nil {
		t.Fatal(err)
	}
	base := hubfixture.ProjectBase(f.Organization, "prj_runner")
	get := func(path string, out any) {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, base+path, f.Tokens[0], nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hubfixture.Decode(t, response, out)
	}

	var page tracker.NativeIssuePage
	get("/work-items?include=board", &page)
	var hostedPage struct {
		Cards    []tracker.NativeBoardCard
		Projects []struct{ Work *tracker.NativeWorkSummary }
	}
	response := hosted.Request(t, viewer, http.MethodGet, hubfixture.OrganizationBase(f.Organization)+"/work-board?project=prj_runner", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &hostedPage)
	for _, board := range []struct {
		name  string
		cards []tracker.NativeBoardCard
	}{{"project board", page.Cards}, {"hosted board", hostedPage.Cards}} {
		cards := make(map[tracker.NativeWorkItemID]tracker.NativeBoardCard, len(board.cards))
		for _, card := range board.cards {
			cards[card.Issue.WorkItemID] = card
		}
		for index, lease := range f.Leases {
			card := cards[lease.WorkItemID]
			checkBoardLeaseStatus(t, board.name, card.Attempt, card.Execution, card.StatusLine, index == 0)
			if card.Issue.State != "Todo" {
				t.Errorf("%s lane=%q, want Todo", board.name, card.Issue.State)
			}
		}
	}

	for i, lease := range f.Leases {
		var attempt tracker.NativeAttempt
		get("/work-items/"+string(lease.WorkItemID)+"/attempts/"+f.Data[i].AttemptID, &attempt)
		want := "running"
		if i == 1 {
			want = "interrupted"
		}
		if attempt.Current != (i == 0) || attempt.Status != want {
			t.Errorf("attempt detail for %s: current=%v status=%q", lease.WorkItemID, attempt.Current, attempt.Status)
		}
	}

	var status []viewstore.BoardStatusRecord
	get("/board/status?item="+string(f.Leases[0].WorkItemID)+"&item="+string(f.Leases[1].WorkItemID), &status)
	if len(status) != len(f.Leases) {
		t.Fatalf("status returned %d items, want %d", len(status), len(f.Leases))
	}
	for _, row := range status {
		checkBoardLeaseStatus(t, "board status", row.Attempt, row.Execution, row.StatusLine, row.WorkItemID == f.Leases[0].WorkItemID)
	}
	var counts viewstore.BoardSummaryPage
	get("/board/counts", &counts)
	var work tracker.NativeIssuePage
	get("/work-items?include=work&limit=1", &work)
	checkFirstWorkItem(t, "work list", work.Work, f.Leases[0].WorkItemID)
	var ranked tracker.NativeIssuePage
	get("/work-items?include=work&limit=1&q=Example", &ranked)
	checkFirstWorkItem(t, "ranked work list", ranked.Work, f.Leases[0].WorkItemID)
	var hostedRunning, countRunning, workRunning int
	for _, row := range counts.Rows {
		if row.Kind == "lane" {
			countRunning += int(row.Running)
		}
	}
	for _, project := range hostedPage.Projects {
		for _, lane := range project.Work.Lanes {
			hostedRunning += lane.Running
		}
	}
	for _, lane := range work.Work.Lanes {
		workRunning += lane.Running
	}
	if hostedRunning != 1 || countRunning != 1 || workRunning != 1 {
		t.Errorf("running counts: hosted=%d endpoint=%d work list=%d, want one on each", hostedRunning, countRunning, workRunning)
	}

	var member hubtest.SeededMember
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member.Member, err = r.AccessMember(t.Context(), viewer.Identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	token := hubtest.Credential(t, f.Hub.Repository, f.Organization, hubtest.CredentialOptions{Member: &member, KeyScope: "read", Projects: []tracker.ProjectID{"prj_runner"}}).Token
	call := hubfixture.MCP(t, f.Hub.Handler(), hubfixture.MCPPath(f.Organization), token)
	for i, lease := range f.Leases {
		reply := call("tools/call", operatortool.ExplainItem, map[string]any{"project_id": "prj_runner", "reference": strconv.Itoa(i + 1)})
		var value explain.IssueExplanation
		if err := json.Unmarshal(hubfixture.MCPData(t, reply, false), &value); err != nil {
			t.Fatal(err)
		}
		want := "running"
		if i == 1 {
			want = "interrupted"
		}
		if value.Attempt == nil || value.Attempt.Status != want || value.NativeRuntime == nil || (value.NativeRuntime.CurrentLease != nil) != (i == 0) {
			t.Errorf("explain %s attempt=%+v, want %s with matching current lease", lease.WorkItemID, value.Attempt, want)
		}
	}
	if !renewDuringProjection {
		item := f.Leases[0].WorkItemID
		if err := f.Hub.Repository.WriteTenant(t.Context(), f.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), "prj_runner", item)
			if err != nil {
				return err
			}
			row.WorkflowStateName = new("In Progress")
			_, err = w.UpdateWorkItem(t.Context(), row)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.BoardItem(t.Context(), "prj_runner", item)
			if err == nil && row.Position.State != "Todo" {
				t.Errorf("fixture projected the move before the read: %s", row.Position.State)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		response := hosted.Request(t, viewer, http.MethodGet, hubfixture.OrganizationBase(f.Organization)+"/work-board?project=prj_runner", nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hostedPage.Cards, hostedPage.Projects = nil, nil
		hubfixture.Decode(t, response, &hostedPage)
		var lane string
		for _, card := range hostedPage.Cards {
			if card.Issue.WorkItemID == item {
				lane = card.Issue.State
			}
		}
		if lane != "In Progress" {
			t.Errorf("moved card lane=%q, want In Progress", lane)
		}
		get("/board/counts", &counts)
		want := tracker.NativeWorkLane{State: "In Progress", Total: 1, Running: 1}
		var projectLane, hostedLane tracker.NativeWorkLane
		for _, row := range counts.Rows {
			if row.Kind == "lane" && row.State == want.State {
				projectLane = tracker.NativeWorkLane{State: row.State, Total: int(row.Total), Running: int(row.Running)}
			}
		}
		for _, project := range hostedPage.Projects {
			for _, lane := range project.Work.Lanes {
				if lane.State == want.State {
					hostedLane = lane
				}
				if lane.State == "Todo" && (lane.Total != 1 || lane.Running != 0) {
					t.Errorf("hosted source lane after move: %+v", lane)
				}
			}
		}
		if projectLane != want || hostedLane != want {
			t.Errorf("destination lane before refresh: project=%+v hosted=%+v want=%+v", projectLane, hostedLane, want)
		}
	}
}

func checkFirstWorkItem(t *testing.T, name string, work *tracker.NativeWorkSummary, want tracker.NativeWorkItemID) {
	t.Helper()
	if work == nil || len(work.Items) != 1 {
		t.Errorf("%s: want one result", name)
	} else if got := work.Items[0].WorkItemID; got != want {
		t.Errorf("%s: first = %s, want %s", name, got, want)
	}
}

func checkBoardLeaseStatus(t *testing.T, read string, attempt *tracker.NativeBoardAttempt, execution *tracker.NativeBoardExecution, line string, running bool) {
	t.Helper()
	wantStatus, wantLine := "interrupted", "Interrupted"
	if running {
		wantStatus, wantLine = "running", "Running"
	}
	if attempt == nil || attempt.Status != wantStatus || (execution != nil && execution.Running) != running || line != wantLine {
		t.Errorf("%s: attempt=%+v execution=%+v line=%q, want %s and %q", read, attempt, execution, line, wantStatus, wantLine)
	}
}
