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
	if card.Identity != "gopherguides/gopher-ai#185" {
		t.Fatalf("card identity = %q, want full identifier", card.Identity)
	}
	if card.DomID != "card-gopherguides-gopher-ai-185" {
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
	exceptions := boardExceptions(data, true)
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
	if exception.ActionLabel != "Review" {
		t.Fatalf("exception should carry the Review action, got %+v", exception)
	}
	if got := exception.ActionAttrs["hx-get"]; got != "/api/v1/board/card?actions=board&issue=digitaldrywood%2Fdetent%2392&project=detent" {
		t.Fatalf("board exception review target = %v", got)
	}

	// From a non-board surface (Fleet/Overview) the Review sheet must not
	// carry the board-actions flag, so its Move/Remove stay hidden.
	fleetExceptions := boardExceptions(data, false)
	if got, _ := fleetExceptions[0].ActionAttrs["hx-get"].(string); strings.Contains(got, "actions=board") {
		t.Fatalf("fleet exception review target should omit board actions, got %v", got)
	}

	data.Snapshot.Blocked = nil
	if got := boardExceptions(data, true); len(got) != 0 {
		t.Fatalf("healthy snapshot should produce no exceptions, got %d", len(got))
	}
}

func TestBoardExceptionsDistinguishWaitingBlocks(t *testing.T) {
	blockedAt := time.Date(2026, 7, 4, 16, 30, 0, 0, time.UTC)
	tests := []struct {
		name             string
		blocked          telemetry.Blocked
		wantKind         primitives.Kind
		wantTitle        string
		wantDetail       string
		wantReviewAction bool
	}{
		{
			name: "dependency source waits without review",
			blocked: telemetry.Blocked{
				Issue: telemetry.Issue{
					ID:         "issue-176",
					Identifier: "digitaldrywood/detent#176",
					ProjectID:  "detent",
					State:      "Todo",
					BlockedBy:  []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#170", State: "In Progress"}},
				},
				Error:     "blocked by non-terminal dependency",
				Source:    telemetry.BlockedSourceDependency,
				BlockedAt: &blockedAt,
			},
			wantKind:   primitives.KindWarn,
			wantTitle:  "Dependency waiting",
			wantDetail: "dependency not ready",
		},
		{
			name: "project status waits without review",
			blocked: telemetry.Blocked{
				Issue: telemetry.Issue{
					ID:         "issue-177",
					Identifier: "digitaldrywood/detent#177",
					ProjectID:  "detent",
					State:      "Blocked",
				},
				Error:     "blocked by project status",
				Source:    telemetry.BlockedSourceProjectStatus,
				BlockedAt: &blockedAt,
			},
			wantKind:   primitives.KindWarn,
			wantTitle:  "Blocked status waiting",
			wantDetail: "paused by project status",
		},
		{
			name: "human project block keeps review",
			blocked: telemetry.Blocked{
				Issue: telemetry.Issue{
					ID:         "issue-178",
					Identifier: "digitaldrywood/detent#178",
					ProjectID:  "detent",
					State:      "Blocked",
				},
				Error:          "needs operator approval",
				Source:         telemetry.BlockedSourceProjectStatus,
				RecoveryReason: "human_blocker",
				BlockedAt:      &blockedAt,
			},
			wantKind:         primitives.KindErr,
			wantTitle:        "Needs review",
			wantDetail:       "needs operator approval",
			wantReviewAction: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := boardTestData()
			data.Snapshot.GeneratedAt = blockedAt.Add(12 * time.Minute)
			data.Snapshot.Blocked = []telemetry.Blocked{tt.blocked}

			exceptions := boardExceptions(data, true)
			if len(exceptions) != 1 {
				t.Fatalf("expected one exception, got %d", len(exceptions))
			}
			exception := exceptions[0]
			if exception.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q", exception.Kind, tt.wantKind)
			}
			if exception.Title != tt.wantTitle {
				t.Fatalf("Title = %q, want %q", exception.Title, tt.wantTitle)
			}
			if !strings.Contains(exception.Rest, tt.wantDetail) {
				t.Fatalf("Rest = %q, want detail containing %q", exception.Rest, tt.wantDetail)
			}
			if got := exception.ActionLabel == "Review"; got != tt.wantReviewAction {
				t.Fatalf("Review action present = %t, want %t; exception = %+v", got, tt.wantReviewAction, exception)
			}
		})
	}
}

func TestBoardBlockedWaitingCardsUseWarningTreatment(t *testing.T) {
	data := boardTestData()
	blockedAt := data.Snapshot.GeneratedAt.Add(-15 * time.Minute)
	data.Snapshot.Blocked = []telemetry.Blocked{
		{
			Issue: telemetry.Issue{
				ID:         "issue-176",
				Identifier: "digitaldrywood/detent#176",
				ProjectID:  "detent",
				Title:      "Wait for parent issue",
				State:      "Blocked",
				BlockedBy:  []telemetry.BlockedRef{{Identifier: "digitaldrywood/detent#170", State: "In Progress"}},
			},
			Error:     "blocked by non-terminal dependency",
			Source:    telemetry.BlockedSourceDependency,
			BlockedAt: &blockedAt,
		},
	}

	view := boardViewFromDashboard(data)
	var card boardCardView
	for _, lane := range view.Lanes {
		if lane.Title != "Blocked" {
			continue
		}
		for _, candidate := range lane.Cards {
			if candidate.Number == "#176" {
				card = candidate
			}
		}
	}

	if card.Number != "#176" {
		t.Fatalf("missing blocked waiting card in view: %+v", view.Lanes)
	}
	if card.ExtraKind != primitives.KindWarn || !card.ExtraChip {
		t.Fatalf("card extra = %q chip %t, want warn chip; card = %+v", card.ExtraKind, card.ExtraChip, card)
	}
	if !strings.Contains(card.ExtraText, "waiting - digitaldrywood/detent#170 In Progress") {
		t.Fatalf("card extra text = %q", card.ExtraText)
	}
	cardClass := boardCardClass(card)
	if !strings.Contains(cardClass, "border-warn/45") || strings.Contains(cardClass, "border-err/45") {
		t.Fatalf("card class = %q, want warning border without error border", cardClass)
	}
}

func TestBoardHumanBlockedCardsUseErrorTreatment(t *testing.T) {
	data := boardTestData()
	blockedAt := data.Snapshot.GeneratedAt.Add(-15 * time.Minute)
	data.Snapshot.Blocked = []telemetry.Blocked{
		{
			Issue: telemetry.Issue{
				ID:         "issue-178",
				Identifier: "digitaldrywood/detent#178",
				ProjectID:  "detent",
				Title:      "Needs operator input",
				State:      "Blocked",
			},
			Error:          "needs operator approval",
			Source:         telemetry.BlockedSourceProjectStatus,
			RecoveryReason: "human_blocker",
			BlockedAt:      &blockedAt,
		},
	}

	view := boardViewFromDashboard(data)
	var card boardCardView
	for _, lane := range view.Lanes {
		if lane.Title != "Blocked" {
			continue
		}
		for _, candidate := range lane.Cards {
			if candidate.Number == "#178" {
				card = candidate
			}
		}
	}

	if card.Number != "#178" {
		t.Fatalf("missing human blocked card in view: %+v", view.Lanes)
	}
	if card.ExtraKind != primitives.KindErr || !card.ExtraChip {
		t.Fatalf("card extra = %q chip %t, want err chip; card = %+v", card.ExtraKind, card.ExtraChip, card)
	}
	if !strings.Contains(card.ExtraText, "needs review - needs operator approval") {
		t.Fatalf("card extra text = %q", card.ExtraText)
	}
	cardClass := boardCardClass(card)
	if !strings.Contains(cardClass, "border-err/45") {
		t.Fatalf("card class = %q, want error border", cardClass)
	}
}

func TestBoardCardIdentityToken(t *testing.T) {
	tests := []struct {
		name       string
		identifier string
		issueID    string
		number     string
		want       string
	}{
		{name: "full identifier", identifier: "digitaldrywood/detent#919", issueID: "I_kw919", number: "#919", want: "digitaldrywood/detent#919"},
		{name: "tracker issue id", identifier: "MT-1", issueID: "memory-1", number: "MT-1", want: "memory-1"},
		{name: "display number", number: "#12", want: "#12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boardCardIdentityToken(tt.identifier, tt.issueID, tt.number); got != tt.want {
				t.Fatalf("boardCardIdentityToken(%q, %q, %q) = %q, want %q", tt.identifier, tt.issueID, tt.number, got, tt.want)
			}
		})
	}
}

func TestBoardCardSlug(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{token: "gopherguides/gopher-ai#185", want: "gopherguides-gopher-ai-185"},
		{token: "My Project #9", want: "my-project-9"},
		{token: "#12", want: "12"},
		{token: "", want: "unknown"},
	}
	for _, tt := range tests {
		if got := boardCardSlug(tt.token); got != tt.want {
			t.Fatalf("boardCardSlug(%q) = %q, want %q", tt.token, got, tt.want)
		}
	}
}

func TestBoardCardScopedSlug(t *testing.T) {
	tests := []struct {
		name     string
		project  string
		identity string
		want     string
	}{
		{name: "repository identity stays global", project: "detent", identity: "digitaldrywood/detent#919", want: "digitaldrywood-detent-919"},
		{name: "local identity keeps project scope", project: "detent", identity: "issue-919", want: "detent-issue-919"},
		{name: "missing project keeps identity", identity: "issue-919", want: "issue-919"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boardCardScopedSlug(tt.project, tt.identity); got != tt.want {
				t.Fatalf("boardCardScopedSlug(%q, %q) = %q, want %q", tt.project, tt.identity, got, tt.want)
			}
		})
	}
}

func TestBoardCardFallsBackToDashboardProjectID(t *testing.T) {
	// Legacy single-project snapshots can omit Issue.ProjectID; the card
	// slug and sheet links must still resolve against the scoped project.
	data := DashboardData{
		ProjectID: "detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			BoardIssues: []telemetry.Issue{
				{ID: "i1", Identifier: "digitaldrywood/detent#42", Title: "No project id card", State: "Todo"},
			},
		},
		Kanban: KanbanData{States: []string{"Todo"}},
	}

	view := boardViewFromDashboard(data)
	var card boardCardView
	for _, lane := range view.Lanes {
		for _, c := range lane.Cards {
			if c.Number == "#42" {
				card = c
			}
		}
	}
	if card.DomID != "card-digitaldrywood-detent-42" {
		t.Fatalf("card dom id = %q, want card-digitaldrywood-detent-42", card.DomID)
	}
	if card.Project != "detent" {
		t.Fatalf("card project = %q, want detent fallback", card.Project)
	}
	if got, _ := sheetOpenAttrs(card.Project, card.Identity, card.Scope, true)["hx-get"].(string); !strings.Contains(got, "project=detent") {
		t.Fatalf("card sheet link should carry the fallback project, got %q", got)
	}

	if _, ok := FindBoardCard(data, "detent", "42"); !ok {
		t.Fatalf("FindBoardCard should match a card by the fallback project id")
	}

	// A blocked row with no ProjectID must still carry the scoped project in
	// its Review link so the sheet keeps project-scoped actions.
	blockedAt := data.Snapshot.GeneratedAt.Add(-time.Minute)
	data.Snapshot.Blocked = []telemetry.Blocked{
		{Issue: telemetry.Issue{Identifier: "digitaldrywood/detent#43"}, Error: "blocked", BlockedAt: &blockedAt},
	}
	exceptions := boardExceptions(data, true)
	if len(exceptions) != 1 {
		t.Fatalf("expected one exception, got %d", len(exceptions))
	}
	if got, _ := exceptions[0].ActionAttrs["hx-get"].(string); !strings.Contains(got, "project=detent") {
		t.Fatalf("exception review link should carry the fallback project, got %q", got)
	}
	if exceptions[0].ID != "exception-digitaldrywood-detent-43" {
		t.Fatalf("exception id = %q, want exception-digitaldrywood-detent-43", exceptions[0].ID)
	}
}

func TestFindBoardCardUsesIdentityBeforeDisplayNumber(t *testing.T) {
	data := DashboardData{
		ProjectID: "aggregate",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			BoardIssues: []telemetry.Issue{
				{ID: "repo-a-7", Identifier: "digitaldrywood/repo-a#7", ProjectID: "aggregate", Title: "Repo A", State: "Todo"},
				{ID: "repo-b-7", Identifier: "digitaldrywood/repo-b#7", ProjectID: "aggregate", Title: "Repo B", State: "Todo"},
			},
		},
		Kanban: KanbanData{States: []string{"Todo"}},
	}

	view := boardViewFromDashboard(data)
	var ids []string
	for _, lane := range view.Lanes {
		for _, card := range lane.Cards {
			ids = append(ids, card.DomID)
			if card.Number != "#7" {
				t.Fatalf("visible card number = %q, want #7", card.Number)
			}
		}
	}
	if strings.Join(ids, ",") != "card-digitaldrywood-repo-a-7,card-digitaldrywood-repo-b-7" {
		t.Fatalf("card dom ids = %v", ids)
	}

	card, ok := FindBoardCard(data, "aggregate", "digitaldrywood/repo-b#7")
	if !ok {
		t.Fatalf("FindBoardCard should match repo-b by identity")
	}
	if card.Identifier != "digitaldrywood/repo-b#7" {
		t.Fatalf("FindBoardCard matched %q, want repo-b", card.Identifier)
	}

	legacy, ok := FindBoardCard(data, "aggregate", "7")
	if !ok {
		t.Fatalf("FindBoardCard should keep bare-number fallback")
	}
	if legacy.IssueNumber != "#7" {
		t.Fatalf("legacy fallback issue number = %q, want #7", legacy.IssueNumber)
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

func TestBoardSnapshotKeepsLanesDuringDegradedRefresh(t *testing.T) {
	// A degraded refresh that still carries prior tracker data must keep the
	// last-known lanes visible, not flash skeletons.
	data := DashboardData{
		Projects: []ProjectSmallMultiple{{ID: "detent"}},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Refresh:     telemetry.Refresh{Status: telemetry.RefreshStatusDegraded, LastError: "tracker unavailable"},
			BoardIssues: []telemetry.Issue{
				{ID: "i1", Identifier: "digitaldrywood/detent#7", ProjectID: "detent", Title: "Last known card", State: "Backlog"},
			},
		},
		Kanban: KanbanData{States: []string{"Backlog"}},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, "dt-skeleton") {
		t.Fatalf("degraded refresh with prior data must not render skeletons:\n%s", html)
	}
	if !strings.Contains(html, `id="board-lanes"`) || !strings.Contains(html, "Last known card") {
		t.Fatalf("degraded refresh should keep the last-known board:\n%s", html)
	}
}

func TestBoardSnapshotShowsDegradedFirstRefresh(t *testing.T) {
	// A configured instance whose first tracker refresh fails (degraded, no
	// prior data) must surface the readiness error, not spin skeletons.
	data := DashboardData{
		Projects: []ProjectSmallMultiple{{ID: "detent"}},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Refresh:     telemetry.Refresh{Status: telemetry.RefreshStatusDegraded, LastError: "tracker unavailable"},
		},
		Kanban: KanbanData{States: []string{"Backlog"}},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, "dt-skeleton") {
		t.Fatalf("degraded first refresh must not render skeletons:\n%s", html)
	}
	if !strings.Contains(html, `aria-label="Snapshot readiness"`) {
		t.Fatalf("degraded first refresh should render the readiness state:\n%s", html)
	}
}

func TestBoardSnapshotStates(t *testing.T) {
	firstRun := renderBoardComponent(t, BoardSnapshot(DashboardData{}))
	if !strings.Contains(firstRun, "Connect a repository to start orchestrating.") {
		t.Fatalf("unconfigured board should render first-run state:\n%s", firstRun)
	}
	if strings.Contains(firstRun, "dt-skeleton") {
		t.Fatalf("first-run state must not render skeletons")
	}

	pending := renderBoardComponent(t, BoardSnapshot(DashboardData{
		Projects: []ProjectSmallMultiple{{ID: "detent"}},
	}))
	if !strings.Contains(pending, "dt-skeleton") {
		t.Fatalf("configured board awaiting first snapshot should render skeletons:\n%s", pending)
	}
	if strings.Contains(pending, `id="board-lanes"`) {
		t.Fatalf("skeleton state must not render live lanes")
	}

	ready := renderBoardComponent(t, BoardSnapshot(boardTestData()))
	if strings.Contains(ready, "dt-skeleton") {
		t.Fatalf("ready board must never render skeletons")
	}
}

func TestBoardSnapshotRenders(t *testing.T) {
	data := boardTestData()
	html := renderBoardComponent(t, BoardSnapshot(data))

	for _, want := range []string{
		`id="board-lanes"`,
		`id="card-gopherguides-gopher-ai-185"`,
		`id="fig-running"`,
		`id="fig-blocked"`,
		"Needs review",
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

func TestBoardEmptyBoardKeepsNonTerminalLanesVisible(t *testing.T) {
	data := DashboardData{
		ProjectID: "detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			BoardIssues: []telemetry.Issue{},
			Refresh:     telemetry.Refresh{LastRefreshAt: timePtr(time.Date(2026, 7, 4, 15, 59, 0, 0, time.UTC))},
		},
		Kanban: KanbanData{States: []string{"Backlog", "In Progress", "Done"}, TerminalStates: []string{"Cancelled"}},
	}
	view := boardViewFromDashboard(data)
	if view.Total == 0 {
		t.Fatalf("expected lanes on an empty configured board")
	}
	if view.Visible == 0 {
		t.Fatalf("empty board should keep non-terminal lanes visible, got 0 of %d", view.Total)
	}
}

func TestBoardCardTerminalNotDone(t *testing.T) {
	card := projectKanbanCard{IssueNumber: "#9", ProjectID: "detent", Title: "Cancelled work"}
	done := boardCardViewFromCard(projectKanbanLane{Title: "Done"}, card, true, "fleet", "detent")
	if !done.Done || !done.Terminal {
		t.Fatalf("Done lane card should be Done and Terminal: %+v", done)
	}
	cancelled := boardCardViewFromCard(projectKanbanLane{Title: "Cancelled"}, card, true, "fleet", "detent")
	if cancelled.Done {
		t.Fatalf("Cancelled card must not be marked Done (no ✓): %+v", cancelled)
	}
	if !cancelled.Terminal {
		t.Fatalf("Cancelled card should still be Terminal: %+v", cancelled)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestBoardFallsBackToSnapshotProjectID(t *testing.T) {
	// Legacy single-project snapshot: Issue.ProjectID empty, data.ProjectID
	// empty, but Snapshot.Project.ID identifies the project.
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			BoardIssues: []telemetry.Issue{
				{ID: "i1", Identifier: "digitaldrywood/detent#7", Title: "Card without project id", State: "Backlog"},
			},
		},
		Kanban: KanbanData{States: []string{"Backlog"}},
	}
	if got := boardFallbackProjectID(data); got != "detent" {
		t.Fatalf("fallback project id = %q, want detent", got)
	}
	view := boardViewFromDashboard(data)
	for _, lane := range view.Lanes {
		for _, c := range lane.Cards {
			if c.Project != "detent" {
				t.Fatalf("card project = %q, want detent snapshot fallback", c.Project)
			}
		}
	}
}

func TestBoardLaneTerminalPerProject(t *testing.T) {
	// Fleet board: "Released" is terminal for project-b but not project-a.
	// A lane holding only project-b cards should read terminal; the global
	// set (from project-a) must not override that.
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			BoardIssues: []telemetry.Issue{
				{ID: "b1", Identifier: "b#1", ProjectID: "project-b", Title: "Released work", State: "Released"},
			},
		},
		Kanban: KanbanData{
			States:                  []string{"Backlog", "Released"},
			TerminalStates:          []string{"Cancelled"},
			TerminalStatesByProject: map[string][]string{"project-b": {"Released"}},
		},
	}
	view := boardViewFromDashboard(data)
	var released boardLaneView
	for _, lane := range view.Lanes {
		if strings.EqualFold(lane.Title, "Released") {
			released = lane
		}
	}
	if released.DefaultVisible {
		t.Fatalf("Released lane is terminal for project-b and should hide by default")
	}
	if len(released.Cards) == 1 && !released.Cards[0].Terminal {
		t.Fatalf("project-b Released card should get terminal treatment: %+v", released.Cards[0])
	}
}
