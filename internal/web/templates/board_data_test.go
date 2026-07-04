package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func boardTestData() DashboardData {
	now := time.Date(2026, 7, 4, 16, 42, 7, 0, time.UTC)
	startedAt := now.Add(-3*time.Minute - 39*time.Second)
	blockedAt := now.Add(-12 * time.Minute)
	return DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Counts:      telemetry.Counts{Running: 1, Queue: 0, Blocked: 1, Completed: 3},
			Running: []telemetry.Running{
				{
					Issue: telemetry.Issue{
						ID:         "issue-185",
						Identifier: "gopherguides/gopher-ai#185",
						ProjectID:  "gopher-ai",
						Title:      "refactor(tmux-start): extract inline bash to scripts",
						State:      "In Progress",
					},
					StartedAt: startedAt,
				},
			},
			Blocked: []telemetry.Blocked{
				{
					Issue: telemetry.Issue{
						ID:         "issue-92",
						Identifier: "digitaldrywood/detent#92",
						ProjectID:  "detent",
						URL:        "https://github.com/digitaldrywood/detent/issues/92",
						Title:      "feat(events): SSE reconnect with resume token",
						State:      "Blocked",
					},
					Error:     "needs operator approval",
					BlockedAt: &blockedAt,
				},
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "issue-176",
					Identifier: "gopherguides/gopher-ai#176",
					ProjectID:  "gopher-ai",
					Title:      "docs: agent session lifecycle diagram",
					State:      "Backlog",
				},
				{
					ID:         "issue-168",
					Identifier: "gopherguides/gopher-ai#168",
					ProjectID:  "gopher-ai",
					Title:      "fix(worktree): stale lock cleanup on crash",
					State:      "Done",
				},
			},
		},
		Kanban: KanbanData{
			States:         []string{"Backlog", "In Progress", "Blocked", "Human Review", "Done"},
			TerminalStates: []string{"Done"},
		},
	}
}

func TestBoardViewLanes(t *testing.T) {
	view := boardViewFromDashboard(boardTestData())

	if view.Total == 0 {
		t.Fatalf("expected lanes, got none")
	}
	lanes := make(map[string]boardLaneView, len(view.Lanes))
	for _, lane := range view.Lanes {
		lanes[lane.Title] = lane
	}

	inProgress, ok := lanes["In Progress"]
	if !ok {
		t.Fatalf("missing In Progress lane; lanes: %v", laneTitles(view))
	}
	if !inProgress.Live {
		t.Fatalf("In Progress lane with a running card should be live")
	}
	if len(inProgress.Cards) != 1 {
		t.Fatalf("In Progress cards = %d, want 1", len(inProgress.Cards))
	}
	card := inProgress.Cards[0]
	if !card.Running {
		t.Fatalf("running card should be marked running")
	}
	if card.Number != "#185" {
		t.Fatalf("card number = %q, want #185", card.Number)
	}
	if card.MetaRight == "" {
		t.Fatalf("running card should show elapsed time")
	}
	if card.DomID != "card-gopher-ai-185" {
		t.Fatalf("card dom id = %q", card.DomID)
	}

	done, ok := lanes["Done"]
	if !ok {
		t.Fatalf("missing Done lane")
	}
	if len(done.Cards) != 1 || !done.Cards[0].Done {
		t.Fatalf("Done lane should carry one done-styled card, got %+v", done.Cards)
	}

	backlog := lanes["Backlog"]
	if backlog.Cards[0].ExtraText != "" {
		t.Fatalf("backlog card should carry no extra signal, got %q", backlog.Cards[0].ExtraText)
	}
	if !backlog.DefaultVisible {
		t.Fatalf("populated lane should be visible by default")
	}

	if human, ok := lanes["Human Review"]; ok && human.DefaultVisible {
		t.Fatalf("empty lane should be hidden by default")
	}
}

func laneTitles(view boardView) []string {
	titles := make([]string, 0, len(view.Lanes))
	for _, lane := range view.Lanes {
		titles = append(titles, lane.Title)
	}
	return titles
}

func TestBoardFigures(t *testing.T) {
	data := boardTestData()
	figures := boardFigures(data.Snapshot)
	if len(figures) != 4 {
		t.Fatalf("expected 4 figures, got %d", len(figures))
	}
	byID := map[string]primitives.Figure{}
	for _, figure := range figures {
		byID[figure.ID] = figure
	}
	if byID["fig-blocked"].Value != "1" || !byID["fig-blocked"].Err {
		t.Fatalf("blocked figure should be err-colored when > 0: %+v", byID["fig-blocked"])
	}
	if byID["fig-queued"].Err {
		t.Fatalf("zero queued figure must stay neutral")
	}

	data.Snapshot.Counts.Blocked = 0
	data.Snapshot.Blocked = nil
	figures = boardFigures(data.Snapshot)
	for _, figure := range figures {
		if figure.Err {
			t.Fatalf("no figure should be err-colored when nothing is blocked: %+v", figure)
		}
	}
}

func TestBoardExceptions(t *testing.T) {
	data := boardTestData()
	exceptions := boardExceptions(data.Snapshot)
	if len(exceptions) != 1 {
		t.Fatalf("expected one exception, got %d", len(exceptions))
	}
	exception := exceptions[0]
	if exception.Kind != primitives.KindErr {
		t.Fatalf("blocked sessions map to err kind, got %q", exception.Kind)
	}
	if exception.Ref != "#92" {
		t.Fatalf("exception ref = %q, want #92", exception.Ref)
	}
	if !strings.Contains(exception.Rest, "needs operator approval") {
		t.Fatalf("exception detail should carry the error, got %q", exception.Rest)
	}
	if !strings.Contains(exception.Rest, "waiting 12m") {
		t.Fatalf("exception detail should carry waiting duration, got %q", exception.Rest)
	}
	if exception.ActionHref == "" || exception.ActionLabel != "Review" {
		t.Fatalf("exception should link to review target, got %+v", exception)
	}

	data.Snapshot.Blocked = nil
	if got := boardExceptions(data.Snapshot); len(got) != 0 {
		t.Fatalf("healthy snapshot should produce no exceptions, got %d", len(got))
	}
}

func TestBoardCardSlug(t *testing.T) {
	tests := []struct {
		project string
		number  string
		want    string
	}{
		{project: "gopher-ai", number: "#185", want: "gopher-ai-185"},
		{project: "My Project", number: "#9", want: "my-project-9"},
		{project: "", number: "#12", want: "12"},
	}
	for _, tt := range tests {
		if got := boardCardSlug(tt.project, tt.number); got != tt.want {
			t.Fatalf("boardCardSlug(%q, %q) = %q, want %q", tt.project, tt.number, got, tt.want)
		}
	}
}

func TestBoardScopeLabel(t *testing.T) {
	if got := boardScopeLabel(DashboardData{}); got != "All projects" {
		t.Fatalf("fleet scope label = %q", got)
	}
	if got := boardScopeLabel(DashboardData{ProjectID: "gopher-ai", ProjectName: "gopher-ai"}); got != "gopher-ai" {
		t.Fatalf("project scope label = %q", got)
	}
}

func renderBoardComponent(t *testing.T, component templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}

func TestBoardSnapshotRenders(t *testing.T) {
	data := boardTestData()
	html := renderBoardComponent(t, BoardSnapshot(data))

	for _, want := range []string{
		`id="board-lanes"`,
		`id="card-gopher-ai-185"`,
		`id="fig-running"`,
		`id="fig-blocked"`,
		"Session blocked",
		"needs operator approval",
		"data-board-lane-picker",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("board snapshot missing %q", want)
		}
	}
	if strings.Contains(html, "#0B0D10") || strings.Contains(html, "#14171C") {
		t.Fatalf("board snapshot must not contain raw hex colors")
	}
}
