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
			view.PriorityBadge = "High"
			view.RuntimeComfyText = "4 attempts · 320k tokens · last turn diagnostic"
			view.ParkSummary = "2 parked attempts"
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
			wantVisible := strings.Join(strings.Fields("detent #2805 Operator gpt-6-astra low A readable card "+tt.want+" High"), " ")
			if visible != wantVisible {
				t.Errorf("card body = %q, want only identity, title, status and priority %q", visible, wantVisible)
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
			data := DashboardData{ConfiguredAgents: map[string]agentidentity.Identity{"project:project:id:issue": agentidentity.Configured("codex", "codex", "", "code", "fleet-model", "", "low", "", time.Time{})}}
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

func TestBoardCardRetainsAttemptModel(t *testing.T) {
	for _, source := range []string{"queue", "blocked", "plan review rework"} {
		for _, retained := range []string{"tracker", "attempt", "unknown attempt"} {
			t.Run(source+"/"+retained, func(t *testing.T) {
				issue := telemetry.Issue{ProjectID: "project", ID: "issue", Identifier: "#1", State: "Todo"}
				role := "code"
				if source == "plan review rework" {
					issue.State = "Rework"
					issue.DispatchMode = "plan"
					role = "plan"
				}
				actual := agentidentity.RuntimeUpdate("actual-model", "", "high", "", time.Time{})
				data := DashboardData{ConfiguredAgents: map[string]agentidentity.Identity{"project:project:id:issue": agentidentity.Configured("codex", "codex", "", role, "new-default", "", "low", "", time.Time{})}}
				want := "actual-model"
				switch retained {
				case "tracker":
					tracker := issue
					tracker.RuntimeIdentity = actual
					data.Snapshot.BoardIssues = []telemetry.Issue{tracker}
				case "attempt", "unknown attempt":
					identity := actual
					if retained == "unknown attempt" {
						identity = agentidentity.Identity{}
						want = "unknown"
					}
					data.Snapshot.WorkAttempts = []telemetry.WorkAttempt{
						{AttemptID: 3, ProjectID: "other", IssueID: "issue", RuntimeIdentity: agentidentity.RuntimeUpdate("other-model", "", "", "", time.Time{})},
						{AttemptID: 2, ProjectID: "project", IssueID: "issue", RuntimeIdentity: identity},
						{AttemptID: 1, ProjectID: "project", IssueID: "issue", RuntimeIdentity: agentidentity.RuntimeUpdate("old-model", "", "", "", time.Time{})},
					}
				}
				if source != "blocked" {
					data.Snapshot.Queue = []telemetry.Queued{{Issue: issue}}
				} else {
					data.Snapshot.Blocked = []telemetry.Blocked{{Issue: issue}}
				}
				cards := projectKanbanIssues(data)
				if len(cards) != 1 {
					t.Fatalf("cards = %d", len(cards))
				}
				card := projectKanbanCard{ProjectID: "project", IssueID: "issue", RuntimeIdentity: cards[0].issue.RuntimeIdentity}
				view := boardCardViewFromCard(data, projectKanbanLane{}, card, false, "", "")
				if view.Model != want || view.ModelDefault || view.Effort != "low" {
					t.Fatalf("identity = %s/%s default=%v, want %s/low actual", view.Model, view.Effort, view.ModelDefault, want)
				}
			})
		}
	}
}

func TestBoardCardIdentifierIdentity(t *testing.T) {
	data := DashboardData{ConfiguredAgents: map[string]agentidentity.Identity{
		"project:project:identifier:repo#1": agentidentity.Configured("codex", "codex", "", "code", "first-default", "", "low", "", time.Time{}),
		"project:project:identifier:repo#2": agentidentity.Configured("codex", "codex", "", "code", "second-default", "", "medium", "", time.Time{}),
	}}
	data.Snapshot.WorkAttempts = []telemetry.WorkAttempt{
		{ProjectID: "project", Identifier: "repo#2", AttemptID: 2, RuntimeIdentity: agentidentity.RuntimeUpdate("actual-second", "", "high", "", time.Time{})},
		{ProjectID: "other", Identifier: "repo#1", AttemptID: 3, RuntimeIdentity: agentidentity.RuntimeUpdate("other-project", "", "high", "", time.Time{})},
	}
	for _, tt := range []struct {
		identifier, model, effort string
		defaultModel              bool
	}{
		{"repo#1", "first-default", "low", true},
		{"repo#2", "actual-second", "medium", false},
	} {
		t.Run(tt.identifier, func(t *testing.T) {
			card := projectKanbanCard{ProjectID: "project", Identifier: tt.identifier}
			view := boardCardViewFromCard(data, projectKanbanLane{}, card, false, "", "")
			if view.Model != tt.model || view.Effort != tt.effort || view.ModelDefault != tt.defaultModel {
				t.Fatalf("identity = %s/%s default=%v", view.Model, view.Effort, view.ModelDefault)
			}
		})
	}
}

// Catches wrong-stage defaults and mixed observed/configured model-effort pairs.
func TestSheetModelEffort(t *testing.T) {
	for _, tt := range []struct {
		name, state, role, model, effort, source string
		observedRole                             string
		running, attempted, activeAttempt        bool
	}{
		{name: "never attempted plan", state: "Todo", role: "plan", model: "configured-plan", effort: "low", source: "configured default"},
		{name: "queued build", state: "Todo", role: "code", model: "configured-code", effort: "high", source: "configured default"},
		{name: "running plan", state: "In Progress", role: "plan", running: true, observedRole: "plan", model: "observed-plan", effort: "medium", source: "current attempt"},
		{name: "running validation", state: "In Progress", role: "validator", running: true, observedRole: "validator", model: "observed-validator", effort: "medium", source: "current attempt"},
		{name: "active attempt without session row", state: "In Progress", role: "validator", attempted: true, activeAttempt: true, observedRole: "validator", model: "observed-validator", effort: "medium", source: "current attempt"},
		{name: "waiting after plan", state: "In Progress", role: "code", attempted: true, observedRole: "plan", model: "configured-code", effort: "high", source: "configured default"},
		{name: "next validation", state: "Human Review", role: "validator", attempted: true, observedRole: "code", model: "configured-validator", effort: "medium", source: "configured default"},
		{name: "previous build", state: "In Progress", role: "code", attempted: true, observedRole: "code", model: "observed-code", effort: "medium", source: "last attempt"},
		{name: "done retains last stage", state: "Done", role: "code", attempted: true, observedRole: "merge", model: "observed-merge", effort: "medium", source: "last attempt"},
		{name: "unknown values", state: "Blocked", source: "configured default", model: "unknown", effort: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := telemetry.Issue{ProjectID: "project", ID: "issue", Identifier: "owner/repo#1"}
			card := projectKanbanCard{ProjectID: issue.ProjectID, IssueID: issue.ID, Identifier: issue.Identifier, Stage: tt.state}
			key := BoardIssueKey(issue)
			configuredEffort := map[string]string{"plan": "low", "code": "high", "validator": "medium"}[tt.role]
			configuredModel := ""
			if tt.role != "" {
				configuredModel = "configured-" + tt.role
			}
			data := DashboardData{ConfiguredStageAgents: map[string]agentidentity.Identity{key: agentidentity.Configured("backend", "codex", "", tt.role, configuredModel, "", configuredEffort, "", time.Time{})}}
			observed := agentidentity.Configured("backend", "codex", "", tt.observedRole, "observed-"+tt.observedRole, "", "medium", "", time.Time{})
			if tt.attempted {
				data.Snapshot.WorkAttempts = []telemetry.WorkAttempt{{AttemptID: 1, ProjectID: issue.ProjectID, IssueID: issue.ID, RuntimeIdentity: observed}}
				if tt.activeAttempt {
					data.Snapshot.WorkAttempts[0].Status = "running"
				}
			}
			if tt.running {
				issue.RuntimeIdentity = observed
				data.Snapshot.Running = []telemetry.Running{{Issue: issue}}
			}
			stageRole := tt.role
			if tt.source == "last attempt" || tt.running || tt.activeAttempt {
				stageRole = tt.observedRole
			}
			stage := map[string]string{"plan": "Plan", "code": "Build", "validator": "Validate", "merge": "Merge"}[stageRole]
			if stage == "" {
				stage = "Build"
			}
			want := stage + ": " + tt.model + " · " + tt.effort + " (" + tt.source + ")"
			if got := sheetModelEffort(data, card); got != want {
				t.Fatalf("selection = %q, want %q", got, want)
			}
			rendered := renderBoardComponent(t, BoardCardSheetCore(data, card, false))
			stateAt := strings.Index(rendered, `data-sheet-row="State"`)
			rowAt := strings.Index(rendered, `data-sheet-row="Model / Effort"`)
			if stateAt < 0 {
				t.Fatal("sheet has no State row")
			}
			nextRow := strings.Index(rendered[stateAt+len(`data-sheet-row="State"`):], `data-sheet-row=`) + stateAt + len(`data-sheet-row="State"`)
			if rowAt < 0 || rowAt != nextRow || !strings.Contains(rendered, want) || strings.Contains(rendered, "Configured effort") {
				t.Fatalf("sheet selection row misplaced or duplicated: %s", rendered)
			}
		})
	}
}
