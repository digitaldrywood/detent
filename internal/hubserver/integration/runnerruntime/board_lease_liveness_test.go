package runnerruntime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
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
	if renewDuringProjection {
		checkBoardWorkPlan(t, f)
		return
	}
	hosted := &hubfixture.Hosted{Hub: f.Hub, Provider: f.Hub.Config.Hosted.Provider.(*hubfixture.HostedProvider)}
	viewer := hosted.User(t, "viewer", "viewer", "viewer@example.test", "", "")
	hosted.Grant(t, viewer, "prj_runner", false, false)
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
		if i == 0 {
			var recovery tracker.NativeRecovery
			get("/work-items/"+string(lease.WorkItemID)+"?view=recovery", &recovery)
			if len(recovery.Attempts) != 1 || !recovery.Attempts[0].Current || recovery.Attempts[0].Status != "running" {
				t.Errorf("recovery for %s: attempts=%+v, want one current running attempt", lease.WorkItemID, recovery.Attempts)
			}
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
	response = hosted.Request(t, viewer, http.MethodGet, hubfixture.OrganizationBase(f.Organization)+"/work-board?project=prj_runner", nil)
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
	for _, filter := range []string{"", "&open=true", "&open=true&state=In+Progress", "&q=Example", "&open=true&state=In+Progress&q=Example"} {
		var moved tracker.NativeIssuePage
		get("/work-items?include=work&limit=1"+filter, &moved)
		checkFirstWorkItem(t, "moved work list"+filter, moved.Work, item)
		if moved.Work != nil && len(moved.Work.Items) == 1 && moved.Work.Items[0].State != want.State {
			t.Errorf("work item before refresh (%q): lane=%q, want %q", filter, moved.Work.Items[0].State, want.State)
		}
		if strings.Contains(filter, "q=") {
			if strings.Contains(filter, "state=") && (len(moved.Items) != 1 || moved.Items[0].WorkItemID != item || moved.Items[0].State != want.State) {
				t.Errorf("filtered search items before refresh (%q): want %s in %q", filter, item, want.State)
			}
			continue
		}
		var workLane tracker.NativeWorkLane
		for _, lane := range moved.Work.Lanes {
			if lane.State == want.State {
				workLane = lane
			}
			if lane.State == "Todo" && (lane.Total != 1 || lane.Running != 0) {
				t.Errorf("work source lane before refresh (%q): %+v", filter, lane)
			}
		}
		if workLane != want {
			t.Errorf("work destination lane before refresh (%q): %+v, want %+v", filter, workLane, want)
		}
	}
}

func checkBoardWorkPlan(t *testing.T, f hubfixture.LeasedRunners) {
	t.Helper()
	const project = "prj_runner"
	const seed = `INSERT INTO native_board_items (
 organization_id,project_id,work_item_id,source_revision,source_event_sequence,source_project_revision,
 state,archived,terminal,running,dispatchable,lane_rank,priority_rank,activity_missing,identifier,
 execution_fence,actor_principal_id,issue_type,verification_state,created_at,updated_at,payload_object_key,
 payload_sha256,payload_bytes,payload_content_bytes,issue_number,page_ready,list_ready,list_organization_id,list_project_id,list_work_item_id)
 SELECT organization_id,project_id,'wi_idle_'||n,source_revision,source_event_sequence,source_project_revision,
 state,false,false,false,true,lane_rank,priority_rank,activity_missing,'idle#'||n,
 0,actor_principal_id,issue_type,verification_state,created_at,updated_at,payload_object_key,
 payload_sha256,payload_bytes,payload_content_bytes,n,true,true,organization_id,project_id,'wi_idle_'||n
 FROM native_board_items CROSS JOIN generate_series(3,5002) n
 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`
	if _, err := f.Admin.ExecContext(t.Context(), seed, f.Organization, project, f.Leases[1].WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Admin.ExecContext(t.Context(), "ANALYZE native_board_items"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []tracker.NativeWorkItemID{f.Leases[0].WorkItemID, "wi_idle_5002"} {
		var servingSQL string
		ctx := pgcore.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
			if strings.Contains(query, "AS page_part") {
				servingSQL = query
			}
		})
		selection := viewstore.BoardSelection{ProjectID: project, Archived: new(false), Terminal: new(false), Compact: true, WorkOrder: true, Limit: 1}
		if err := f.Hub.Repository.ReadTenantSnapshot(ctx, f.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.(interface {
				BoardModelPages(context.Context, viewstore.BoardSelection, *viewstore.BoardSelection, accessstore.Authority) (viewstore.BoardModelPage, error)
			}).BoardModelPages(ctx, selection, nil, accessstore.Authority{})
			if err == nil && (len(page.Records) != 1 || page.Records[0].WorkItemID != want) {
				t.Errorf("work page: got %d rows, want only %s", len(page.Records), want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if servingSQL == "" {
			t.Fatal("work page did not record its serving statement")
		}
		var raw string
		if err := f.Admin.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+servingSQL, f.Organization, project, false, false, 1).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []struct{ Plan boardWorkPlanNode }
		if err := json.Unmarshal([]byte(raw), &plans); err != nil {
			t.Fatal(err)
		}
		if visited := plans[0].Plan.boardRowsVisited(); visited > 64 {
			t.Errorf("limit-one work page for %s visited %d board rows with 5,000 idle items", want, visited)
		} else {
			t.Logf("limit-one work page for %s visited %d board rows with 5,000 idle items", want, visited)
		}
		if want == f.Leases[0].WorkItemID {
			if _, err := f.Admin.ExecContext(ctx, "UPDATE leases SET expires_at=clock_timestamp() WHERE organization_id=$1 AND lease_id=$2", f.Organization, f.Leases[0].ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type boardWorkPlanNode struct {
	Relation string              `json:"Relation Name"`
	Rows     int                 `json:"Actual Rows"`
	Removed  int                 `json:"Rows Removed by Filter"`
	Loops    int                 `json:"Actual Loops"`
	Plans    []boardWorkPlanNode `json:"Plans"`
}

func (p boardWorkPlanNode) boardRowsVisited() int {
	var visited int
	if p.Relation == "native_board_items" {
		visited = (p.Rows + p.Removed) * p.Loops
	}
	for _, child := range p.Plans {
		visited += child.boardRowsVisited()
	}
	return visited
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
