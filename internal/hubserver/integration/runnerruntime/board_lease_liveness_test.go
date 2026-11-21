package runnerruntime_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

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
	f := hubfixture.NewLeasedRunners(t)
	hosted := &hubfixture.Hosted{Hub: f.Hub, Provider: f.Hub.Config.Hosted.Provider.(*hubfixture.HostedProvider)}
	viewer := hosted.User(t, "viewer", "viewer", "viewer@example.test", "", "")
	hosted.Grant(t, viewer, "prj_runner", false, false)
	f.Hub.RefreshBoard(t, f.Organization)
	if _, err := f.Admin.ExecContext(t.Context(), "UPDATE leases SET expires_at=clock_timestamp() WHERE organization_id=$1 AND lease_id=$2", f.Organization, f.Leases[0].ID); err != nil {
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
			checkBoardLeaseStatus(t, board.name, card.Attempt, card.Execution, card.StatusLine, index != 0)
			if card.Issue.State != "Todo" {
				t.Errorf("%s lane=%q, want Todo", board.name, card.Issue.State)
			}
		}
	}

	var status []viewstore.BoardStatusRecord
	get("/board/status?item="+string(f.Leases[0].WorkItemID)+"&item="+string(f.Leases[1].WorkItemID), &status)
	if len(status) != len(f.Leases) {
		t.Fatalf("status returned %d items, want %d", len(status), len(f.Leases))
	}
	for _, row := range status {
		checkBoardLeaseStatus(t, "board status", row.Attempt, row.Execution, row.StatusLine, row.WorkItemID == f.Leases[1].WorkItemID)
	}
	var counts viewstore.BoardSummaryPage
	get("/board/counts", &counts)
	var work tracker.NativeIssuePage
	get("/work-items?include=work&limit=1", &work)
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
		if i == 0 {
			want = "interrupted"
		}
		if value.Attempt == nil || value.Attempt.Status != want || value.NativeRuntime == nil || (value.NativeRuntime.CurrentLease != nil) != (i != 0) {
			t.Errorf("explain %s attempt=%+v, want %s with matching current lease", lease.WorkItemID, value.Attempt, want)
		}
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
