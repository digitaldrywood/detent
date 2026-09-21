package templates

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestINV13BoardCardContent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		view boardCardView
		card projectKanbanCard
		want string
	}{
		{name: "running", view: boardCardView{Running: true}, card: projectKanbanCard{CIStatus: "fail"}, want: "Running"},
		{name: "question", view: boardCardView{Facts: []cardFactView{{Name: "reason", Text: "waiting for a human reply · 4h"}}}, card: projectKanbanCard{Blockers: []string{"repo#1"}}, want: "Needs your reply · 4h"},
		{name: "blocked", card: projectKanbanCard{Blockers: []string{"repo#1"}}, want: "Blocked · 1"},
		{name: "dependency", view: boardCardView{DispatchStatus: "Waiting"}, card: projectKanbanCard{Blockers: []string{"digitaldrywood/pyroapex#2129 [native] (Rework)"}}, want: "Waiting on #2129"},
		{name: "CI red", card: projectKanbanCard{CIStatus: "fail"}, want: "CI failed"},
		{name: "scheduler dependency", view: boardCardView{DispatchStatus: "Waiting", ExtraText: "Waiting on owner/repo#2064 · observed yesterday"}, want: "Waiting on #2064"},
		{name: "CI running before sync", view: boardCardView{Work: workItemMetadata{SyncKey: "error", Sync: "Error"}}, card: projectKanbanCard{CIStatus: "pending"}, want: "CI running"},
		{name: "dependency recovery", view: boardCardView{DispatchStatus: "Waiting"}, card: projectKanbanCard{BlockedSource: telemetry.BlockedSourceDependency, BlockedReason: "Depends on owner/repo#5200"}, want: "Waiting on #5200"},
		{name: "idle"},
		{name: "unicode bound", view: boardCardView{MergeLaneStatus: strings.Repeat("界", 60)}, want: strings.Repeat("界", 47) + "…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			view := tt.view
			view.Project = "detent"
			view.Model = "gpt-6-astra"
			view.Effort = "low"
			view.Number = "#2805"
			view.Title = "A readable card"
			view.CreationOrigin = "operator"
			if view.ExtraText == "" {
				view.ExtraText = "scheduler evidence diagnostic"
			}
			view.TrackerSummary = "Tracker snapshot · 12m ago"
			view.AgeFooter = "4h"
			view.Signals = boardCardSignals(view, tt.card)
			rendered := renderBoardComponent(t, boardCardView2(view))
			for _, marker := range []string{"data-board-dispatch-evidence", "data-board-tracker-observation", "data-board-card-facts", "data-board-card-age-footer", "data-board-card-details", "data-board-card-expanded"} {
				if strings.Contains(rendered, marker) {
					t.Errorf("forbidden body element %s", marker)
				}
			}
			for _, marker := range []string{"data-board-card-identity", "data-board-card-title", "A readable card", "#2805", "detent"} {
				if !strings.Contains(rendered, marker) {
					t.Errorf("missing %s", marker)
				}
			}
			matches := regexp.MustCompile(`data-board-card-signal>([^<]*)</div>`).FindAllStringSubmatch(rendered, -1)
			if len(matches) > 1 {
				t.Fatalf("%d statuses", len(matches))
			}
			got := ""
			if len(matches) == 1 {
				got = html.UnescapeString(matches[0][1])
			}
			if utf8.RuneCountInString(got) > 48 {
				t.Errorf("status exceeds 48 characters: %q", got)
			}
			if got != tt.want {
				t.Errorf("status %q, want %q", got, tt.want)
			}
			visible := html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(rendered, " "))
			visible = strings.Join(strings.Fields(visible), " ")
			wantVisible := strings.TrimSpace("detent #2805 Operator gpt-6-astra low A readable card " + tt.want)
			if visible != wantVisible {
				t.Errorf("card body = %q, want only identity, title and status %q", visible, wantVisible)
			}
			if !strings.Contains(html.UnescapeString(rendered), view.ExtraText) {
				t.Error("hover loses diagnostic")
			}
		})
	}
}

func TestINV13SheetObservations(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 16, 0, 0, 0, time.UTC)
	data := DashboardData{Snapshot: telemetry.Snapshot{GeneratedAt: now, Tracker: telemetry.SnapshotSection{ObservedAt: now.Add(-time.Minute)}}}
	card := projectKanbanCard{ProjectID: "detent", IssueID: "2805", Identifier: "digitaldrywood/detent#2805", Stage: "Todo"}
	rendered := renderBoardComponent(t, BoardCardSheetCore(data, card, false))
	for _, want := range []string{"Tracker snapshot", "2026-09-16T15:59:00Z", "Current condition", "Scheduler evidence unavailable"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("sheet missing %q", want)
		}
	}
}

func TestBoardCardConfiguredIdentity(t *testing.T) {
	for _, tt := range []struct {
		name        string
		runtime     agentidentity.Identity
		wantModel   string
		wantDefault bool
	}{
		{name: "never dispatched", wantModel: "fleet-model", wantDefault: true},
		{name: "actual attempt", runtime: agentidentity.RuntimeUpdate("actual-model", "", "high", "", time.Time{}), wantModel: "actual-model"},
		{name: "attempt model unavailable", runtime: agentidentity.Identity{BackendID: "codex"}, wantModel: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := DashboardData{ConfiguredAgents: map[string]agentidentity.Identity{"project\x00issue": agentidentity.Configured("codex", "codex", "", "code", "fleet-model", "", "low", "", time.Time{})}}
			card := projectKanbanCard{ProjectID: "project", IssueID: "issue", RuntimeIdentity: tt.runtime}
			view := boardCardViewFromCard(data, projectKanbanLane{}, card, false, "", "")
			if view.Model != tt.wantModel || view.ModelDefault != tt.wantDefault || view.Effort != "low" {
				t.Fatalf("identity = %s/%s default=%v", view.Model, view.Effort, view.ModelDefault)
			}
			rendered := renderBoardComponent(t, boardCardView2(view))
			identityEnd := strings.Index(rendered, "data-board-card-title")
			// Both values remain in the identity DOM. INV-13 permits CSS to hide
			// effort at Compact; density.spec.js enforces that visibility contract.
			for _, marker := range []string{"data-board-card-model", "data-board-card-effort"} {
				at := strings.Index(rendered, marker)
				if at < 0 || at > identityEnd {
					t.Fatalf("%s is outside identity row", marker)
				}
			}
			if strings.Contains(rendered, ">default</span>") != tt.wantDefault {
				t.Fatalf("default label mismatch: %s", rendered)
			}
		})
	}
}
