package templates

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestBoardDetailSheetRendersEfficiencyReceipt(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	board := projectKanbanBoardView(data)
	if len(board.AllLanes) == 0 || len(board.AllLanes[0].Cards) == 0 {
		t.Fatal("board fixture has no cards")
	}
	card := board.AllLanes[0].Cards[0]
	data.EfficiencyReceipts = []efficiency.Receipt{{
		ProjectID:         card.ProjectID,
		IssueID:           card.IssueID,
		Identifier:        card.Identifier,
		Sessions:          2,
		Attempts:          2,
		InputTokens:       1000,
		CachedInputTokens: 970,
		OutputTokens:      200,
		TotalTokens:       1200,
		EstimatedCostUSD:  1.25,
		WallSeconds:       600,
		WorkingSeconds:    360,
		GateWaitSeconds:   120,
		MergeTrainSeconds: 60,
		ParkedSeconds:     30,
		Redispatches:      1,
		CIReruns:          1,
		TokensAnomaly:     true,
		InProgress:        true,
	}}
	html := renderBoardComponent(t, BoardCardSheet(data, card, false, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{}))
	for _, want := range []string{"efficiency-receipt", "Efficiency receipt", "97%", "$1.25", "redispatches", "Live", "Anomaly"} {
		if !strings.Contains(html, want) {
			t.Fatalf("detail sheet missing %q:\n%s", want, html)
		}
	}
}

func TestBoardDetailSheetShowsRetryLimitAttemptError(t *testing.T) {
	t.Parallel()

	card := projectKanbanCard{
		IssueID:     "issue-2052",
		IssueNumber: "#2052",
		Identifier:  "digitaldrywood/detent#2052",
		ProjectID:   "detent",
		Title:       "Retry limit evidence",
		Stage:       "Blocked",
	}
	data := DashboardData{
		ProjectID: "detent",
		Snapshot: telemetry.Snapshot{Blocked: []telemetry.Blocked{{
			Issue: telemetry.Issue{
				ID:         card.IssueID,
				Identifier: card.Identifier,
				ProjectID:  card.ProjectID,
				State:      card.Stage,
			},
			Error:        "terminal_attempt_retry_limit",
			AttemptError: "custom backend rejected --lease-ttl",
		}}},
	}

	html := renderBoardComponent(t, BoardCardSheet(data, card, false, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{}))
	for _, want := range []string{"terminal_attempt_retry_limit", "last attempt", "custom backend rejected --lease-ttl"} {
		if !strings.Contains(html, want) {
			t.Fatalf("detail sheet missing %q:\n%s", want, html)
		}
	}
}

func TestBoardCardRendersCumulativeParkSummary(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, 8, 9, 16, 6, 0, 0, time.UTC)
	last := time.Date(2026, 8, 12, 17, 34, 57, 0, time.UTC)
	summary, detail := boardCardParkSummary(telemetry.ParkSummary{
		AttemptCount: 7,
		ParkCount:    3,
		Causes:       []telemetry.ParkCauseSummary{{Cause: "no_progress_limit", Count: 2, FirstAt: first, LastAt: last}},
		Tokens:       telemetry.ParkTokenTotals{InputTokens: 5_247_029, CachedInputTokens: 4_978_944, OutputTokens: 25_709, ReasoningOutputTokens: 10_232},
	})
	for _, want := range []string{"7 attempts", "3 parks", "5.2M input", "5.0M cached", "25.7K output", "10.2K reasoning"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
	for _, want := range []string{"no_progress_limit: 2", first.Format(time.RFC3339), last.Format(time.RFC3339)} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q missing %q", detail, want)
		}
	}
	html := renderBoardComponent(t, boardCardView2(boardCardView{DomID: "card-1773", Title: "Park summary", ParkSummary: summary, ParkDetail: detail}))
	for _, want := range []string{"data-board-card-park-summary", "data-help-title=\"Park history\"", "no_progress_limit"} {
		if !strings.Contains(html, want) {
			t.Fatalf("card HTML missing %q:\n%s", want, html)
		}
	}
}

func TestBoardCardRendersCompletionProgressClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		progress telemetry.CompletionProgress
		want     []string
	}{
		{
			name:     "verifiable non-diff progress",
			progress: telemetry.CompletionProgress{Outcome: "success", Reason: "verifiable_non_diff_progress", Kinds: []string{"audit_artifact"}},
			want:     []string{"Last turn · audit artifact", "verifiable_non_diff_progress"},
		},
		{
			name:     "operational completion",
			progress: telemetry.CompletionProgress{Outcome: "success", Reason: "operational_completion", Kinds: []string{"operational_completion"}, CompletionKind: "operational"},
			want:     []string{"Last turn · operational completion", "completion kind: operational", "operational_completion"},
		},
		{
			name:     "prose-only no progress",
			progress: telemetry.CompletionProgress{Outcome: telemetry.CompletionProgressOutcomeNoProgress, Reason: "completed_clean_diff_without_pull_request", ConsecutiveNoProgress: 2, NoProgressLimit: 3},
			want:     []string{"Last turn · no progress 2/3", "completed_clean_diff_without_pull_request"},
		},
		{
			name:     "safety no-progress overrides observed artifact",
			progress: telemetry.CompletionProgress{Outcome: telemetry.CompletionProgressOutcomeNoProgress, Reason: "stranded_unpushed_work", Kinds: []string{"audit_artifact"}, ConsecutiveNoProgress: 3, NoProgressLimit: 3},
			want:     []string{"Last turn · no progress 3/3", "stranded_unpushed_work"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			summary, detail, kind := boardCardCompletionProgress(tt.progress)
			html := renderBoardComponent(t, boardCardView2(boardCardView{DomID: "card-progress", Title: tt.name, ProgressSummary: summary, ProgressDetail: detail, ProgressKind: kind}))
			for _, want := range tt.want {
				if !strings.Contains(html, want) {
					t.Fatalf("card HTML missing %q:\n%s", want, html)
				}
			}
			if !strings.Contains(html, "data-board-card-progress") {
				t.Fatalf("card HTML missing progress marker:\n%s", html)
			}
		})
	}
}

func TestBoardExceptionsIncludeFleetStalenessWarning(t *testing.T) {
	t.Parallel()
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			StalenessWarnings: []telemetry.StalenessWarning{{
				ID:             "warning-1",
				ProjectID:      "detent",
				Kind:           "lane_aging",
				Identifier:     "digitaldrywood/detent#1574",
				IssueURL:       "https://github.com/digitaldrywood/detent/issues/1574",
				Detail:         "the item has remained in Merging",
				AgeSeconds:     3 * 60 * 60,
				WaitingOnHuman: false,
			}},
		},
	}
	exceptions := boardExceptions(data, false)
	if len(exceptions) != 0 {
		t.Fatalf("boardExceptions() = %#v, want staleness excluded", exceptions)
	}
	rows := diagnosticsConditionRows(data.Snapshot)
	if len(rows) != 1 || rows[0].Class != observability.ClassDiagnostic || rows[0].Target != "digitaldrywood/detent#1574" {
		t.Fatalf("diagnostics conditions = %#v, want relocated staleness row", rows)
	}
}

func TestBoardCardAndDetailSheetRenderOrigin(t *testing.T) {
	t.Parallel()

	card := projectKanbanCard{
		IssueID:     "issue-1537",
		IssueNumber: "#1537",
		Identifier:  "digitaldrywood/detent#1537",
		ProjectID:   "detent",
		Title:       "Track admission provenance",
		Stage:       "Todo",
		Origin:      "admission",
		OriginActor: "ada",
	}
	view := boardCardViewFromCard(
		DashboardData{},
		projectKanbanLane{Title: "Todo"},
		card,
		false,
		"project",
		"detent",
	)
	cardHTML := renderBoardComponent(t, boardCardView2(view))
	for _, want := range []string{`data-board-card-origin`, "via admission", "@ada"} {
		if !strings.Contains(cardHTML, want) {
			t.Fatalf("board card missing %q:\n%s", want, cardHTML)
		}
	}
	sheetHTML := renderBoardComponent(t, BoardCardSheet(
		DashboardData{},
		card,
		false,
		false,
		KanbanConversationData{},
		BoardActivityData{},
		BoardSessionData{},
	))
	for _, want := range []string{"Origin", "via admission", "@ada"} {
		if !strings.Contains(sheetHTML, want) {
			t.Fatalf("detail sheet missing %q:\n%s", want, sheetHTML)
		}
	}
}

func TestBoardDetailSheetUsesTrackerSpecificIssueAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		trackerKind string
		url         string
		want        []string
		unwanted    []string
	}{
		{
			name:        "local tracker",
			trackerKind: "local_sqlite",
			url:         "/projects/video/issues/wi-123",
			want:        []string{"Open issue", `href="/projects/video/issues/wi-123"`},
			unwanted:    []string{"Open on GitHub"},
		},
		{
			name:        "github tracker",
			trackerKind: "github",
			url:         "https://github.com/digitaldrywood/detent/issues/123",
			want:        []string{"Open on GitHub", `target="_blank"`},
			unwanted:    []string{"Open issue"},
		},
		{
			name:        "linear tracker",
			trackerKind: "linear",
			url:         "https://linear.app/example/issue/DET-123",
			want:        []string{"Open on GitHub", `target="_blank"`},
			unwanted:    []string{"Open issue"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := DashboardData{
				ProjectID: "video",
				Kanban: KanbanData{
					ProjectID:   "video",
					TrackerKind: tt.trackerKind,
				},
			}
			card := projectKanbanCard{IssueNumber: "#123", Identifier: "wi-123", Title: "Issue action", URL: tt.url}
			html := renderBoardComponent(t, BoardCardSheet(data, card, false, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{}))
			for _, want := range tt.want {
				if !strings.Contains(html, want) {
					t.Fatalf("sheet missing %q:\n%s", want, html)
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(html, unwanted) {
					t.Fatalf("sheet contains %q:\n%s", unwanted, html)
				}
			}
		})
	}
}

func TestProjectKanbanCardUsesInternalURLForLocalTracker(t *testing.T) {
	t.Parallel()

	data := DashboardData{
		DashboardURL: "https://detent.example",
		ProjectID:    "video",
		Kanban: KanbanData{
			ProjectID:   "video",
			TrackerKind: "local_sqlite",
		},
	}
	card := projectKanbanCardForIssue(data, telemetry.Issue{
		ID:         "wi-123",
		Identifier: "wi-123",
		ProjectID:  "video",
		URL:        "https://github.com/example/repository",
	}, "Todo", time.Time{}, time.Time{})
	if card.URL != "https://detent.example/projects/video/issues/wi-123" {
		t.Fatalf("card.URL = %q, want internal issue detail URL", card.URL)
	}
}

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
						URL:        "https://github.com/gopherguides/gopher-ai/issues/185",
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
	if card.URL != "https://github.com/gopherguides/gopher-ai/issues/185" {
		t.Fatalf("card URL = %q", card.URL)
	}
	if card.AgeFooter != "3m" {
		t.Fatalf("running card age footer = %q, want 3m", card.AgeFooter)
	}
	if card.MetaRight != "" {
		t.Fatalf("running card age should not consume top-right metadata, got %q", card.MetaRight)
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
	if done.DefaultVisible {
		t.Fatalf("populated Done lane should be hidden by default")
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

func TestBoardViewInProgressLaneCountsOnlyLiveAttempts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{ID: "live", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", Title: "Live attempt", State: "In Progress"},
				{ID: "retry", Identifier: "digitaldrywood/detent#2", ProjectID: "detent", Title: "Retry waiting", State: "In Progress"},
				{ID: "idle", Identifier: "digitaldrywood/detent#3", ProjectID: "detent", Title: "No live attempt", State: "In Progress"},
			},
			Running: []telemetry.Running{{
				Issue: telemetry.Issue{ID: "live", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", State: "In Progress"},
			}},
			Queue: []telemetry.Queued{{
				Issue: telemetry.Issue{ID: "retry", Identifier: "digitaldrywood/detent#2", ProjectID: "detent", State: "In Progress"},
			}},
		},
		Kanban: KanbanData{States: []string{"In Progress"}},
	}

	view := boardViewFromDashboard(data)
	if len(view.Lanes) != 1 {
		t.Fatalf("lanes = %d, want 1", len(view.Lanes))
	}
	lane := view.Lanes[0]
	if lane.Count != "3 (1 live)" || !lane.Live {
		t.Fatalf("In Progress lane count/live = %q/%v, want 3 (1 live)/true", lane.Count, lane.Live)
	}
	for _, tt := range []struct {
		name    string
		running []telemetry.Running
		want    string
	}{
		{name: "incomplete runtime without rows", want: "3 (live unknown)"},
		{name: "incomplete runtime with known lower bound", running: data.Snapshot.Running, want: "3 (1+ live)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := data.Snapshot
			snapshot.Runtime = telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed}
			snapshot.Running = tt.running
			partial := boardViewFromDashboard(DashboardData{Snapshot: snapshot, Kanban: data.Kanban})
			if got := partial.Lanes[0].Count; got != tt.want {
				t.Fatalf("incomplete runtime lane count = %q, want %q", got, tt.want)
			}
		})
	}
	cards := make(map[string]boardCardView, len(lane.Cards))
	for _, card := range lane.Cards {
		cards[card.IssueID] = card
	}
	if !cards["live"].Running || cards["retry"].Running || cards["idle"].Running {
		t.Fatalf("card live states = live:%v retry:%v idle:%v", cards["live"].Running, cards["retry"].Running, cards["idle"].Running)
	}
	if cards["retry"].ExtraText != "Awaiting retry" || cards["idle"].ExtraText != "No live attempt" {
		t.Fatalf("card wait signals = retry:%q idle:%q", cards["retry"].ExtraText, cards["idle"].ExtraText)
	}
}

func TestRecentCompletionCardSuppressesMoveDisabledBadge(t *testing.T) {
	t.Parallel()

	card := projectKanbanCard{
		IssueNumber:      "#1385",
		Identifier:       "digitaldrywood/detent#1385",
		ProjectID:        "detent",
		Title:            "Completed digitaldrywood/detent#1385",
		Stage:            "Done",
		RecentCompletion: true,
	}
	lane := projectKanbanLane{Title: "Done", Cards: []projectKanbanCard{card}}
	view := boardCardViewFromCard(DashboardData{}, lane, card, true, "project", "detent")

	if view.Number != "#1385" {
		t.Fatalf("card number = %q, want #1385", view.Number)
	}
	if view.MoveDisabledText != "" || view.MoveDisabledLabel != "" {
		t.Fatalf("recent completion move badge = %q / %q, want suppressed", view.MoveDisabledText, view.MoveDisabledLabel)
	}
	if view.DragDrop || view.CanDrag {
		t.Fatalf("recent completion should remain non-draggable: %+v", view)
	}
}

func TestBoardCardDataSeqUsesProjectRefresh(t *testing.T) {
	t.Parallel()

	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			Refresh: telemetry.Refresh{DataSeq: 99},
			Projects: []telemetry.ProjectSnapshot{
				{Project: telemetry.Project{ID: "alpha"}, Refresh: telemetry.Refresh{DataSeq: 5}},
				{Project: telemetry.Project{ID: "bravo"}, Refresh: telemetry.Refresh{DataSeq: 12}},
			},
			BoardIssues: []telemetry.Issue{
				{ID: "alpha-issue", Identifier: "example/alpha#1", ProjectID: "alpha", Title: "Alpha", State: "Todo"},
				{ID: "bravo-issue", Identifier: "example/bravo#2", ProjectID: "bravo", Title: "Bravo", State: "Todo"},
			},
		},
		Kanban: KanbanData{States: []string{"Todo"}},
	}

	view := boardViewFromDashboard(data)
	if len(view.Lanes) != 1 || len(view.Lanes[0].Cards) != 2 {
		t.Fatalf("board cards = %+v, want one lane with two cards", view.Lanes)
	}
	got := map[string]uint64{}
	for _, card := range view.Lanes[0].Cards {
		got[card.Project] = card.DataSeq
	}
	if got["alpha"] != 5 || got["bravo"] != 12 {
		t.Fatalf("card data seqs = %v, want alpha=5 bravo=12", got)
	}

	html := renderBoardComponent(t, boardCardView2(view.Lanes[0].Cards[0]))
	if !strings.Contains(html, `data-kanban-data-seq="`) {
		t.Fatalf("board card missing data sequence attribute:\n%s", html)
	}
}

func TestBoardViewSortsCardsBySchedulerPriorityInputs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	oldest := now.Add(-5 * time.Hour)
	older := now.Add(-4 * time.Hour)
	middle := now.Add(-3 * time.Hour)
	newer := now.Add(-2 * time.Hour)
	newest := now.Add(-time.Hour)
	data := DashboardData{
		ProjectID: "detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{ID: "plain", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", Title: "Old unprioritized", State: "Todo", StageUpdatedAt: &oldest},
				{ID: "unblocker", Identifier: "digitaldrywood/detent#6", ProjectID: "detent", Title: "Derived unblocker", State: "Todo", UnblockerCount: 2, StageUpdatedAt: &newest},
				{ID: "bug", Identifier: "digitaldrywood/detent#2", ProjectID: "detent", Title: "Bug label", State: "Todo", Labels: []string{"bug"}, StageUpdatedAt: &older},
				{ID: "hotfix", Identifier: "digitaldrywood/detent#3", ProjectID: "detent", Title: "Hotfix label", State: "Todo", Labels: []string{"hotfix"}, StageUpdatedAt: &middle},
				{ID: "rank-three", Identifier: "digitaldrywood/detent#4", ProjectID: "detent", Title: "Mapped rank three", State: "Todo", Priority: boardPriorityPointer(3), PriorityName: "P2", StageUpdatedAt: &newer},
				{ID: "rank-one", Identifier: "digitaldrywood/detent#5", ProjectID: "detent", Title: "Mapped rank one", State: "Todo", Priority: boardPriorityPointer(1), PriorityName: "P0", StageUpdatedAt: &newest},
			},
		},
		Kanban: KanbanData{
			ProjectID:               "detent",
			States:                  []string{"Todo"},
			DispatchPriorityByLabel: []string{"hotfix", "bug"},
		},
	}

	view := boardViewFromDashboard(data)
	if len(view.Lanes) != 1 {
		t.Fatalf("lanes = %d, want 1", len(view.Lanes))
	}
	got := make([]string, 0, len(view.Lanes[0].Cards))
	for _, card := range view.Lanes[0].Cards {
		got = append(got, card.IssueID)
	}
	want := []string{"rank-one", "rank-three", "hotfix", "bug", "unblocker", "plain"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("card order = %#v, want %#v", got, want)
	}
}

func TestBoardCardExtraShowsWaitReasonAheadOfCI(t *testing.T) {
	t.Parallel()

	card := projectKanbanCard{
		WaitDetail: "waiting on artifact gate status ('queued')",
		CIStatus:   "pass",
	}

	kind, text, chip := boardCardExtra(card, boardCardView{})
	if kind != primitives.KindInfo || text != card.WaitDetail || chip {
		t.Fatalf("boardCardExtra() = %q, %q, %t, want info wait detail", kind, text, chip)
	}
}

func TestBoardCardViewSurfacesStrandedDuration(t *testing.T) {
	t.Parallel()

	card := projectKanbanCard{
		IssueID:     "issue-1860",
		Identifier:  "digitaldrywood/detent#1860",
		IssueNumber: "#1860",
		ProjectID:   "detent",
		Title:       "Recover stranded active cards",
		Stage:       "In Progress",
	}
	data := DashboardData{Snapshot: telemetry.Snapshot{StrandedActiveIssues: []telemetry.StrandedIssue{{
		ProjectID: "detent", IssueID: card.IssueID, Identifier: card.Identifier, DurationSeconds: 31 * 24 * 60 * 60,
	}}}}

	view := boardCardViewFromCard(data, projectKanbanLane{Title: "In Progress"}, card, false, "fleet", "")
	if view.ExtraKind != primitives.KindWarn || view.ExtraText != "Stranded 31d · no worker" || !view.ExtraChip {
		t.Fatalf("stranded card signal = %q, %q, %t", view.ExtraKind, view.ExtraText, view.ExtraChip)
	}
	if view.CompactSignal != view.ExtraText {
		t.Fatalf("CompactSignal = %q, want %q", view.CompactSignal, view.ExtraText)
	}
}

func TestBoardCardRendersMergeLaneProgress(t *testing.T) {
	t.Parallel()

	card := projectKanbanCard{
		IssueID:         "queued",
		Identifier:      "digitaldrywood/video-studio#114",
		IssueNumber:     "#114",
		ProjectID:       "video-studio",
		Title:           "Queued merge",
		Stage:           "Merging",
		MergeLaneStatus: "Draining #2",
		MergeLaneDetail: "2nd in merge queue; lane draining behind digitaldrywood/video-studio#106 / PR #113; phase watching current-head CI",
		MergeLaneKind:   primitives.KindOK,
	}

	view := boardCardViewFromCard(DashboardData{}, projectKanbanLane{Title: "Merging"}, card, false, "fleet", "")
	if view.CompactSignal != card.MergeLaneStatus {
		t.Fatalf("CompactSignal = %q, want %q", view.CompactSignal, card.MergeLaneStatus)
	}
	html := renderBoardComponent(t, boardCardView2(view))
	for _, want := range []string{`data-board-card-merge-lane`, "Draining #2", card.MergeLaneDetail, "text-ok"} {
		if !strings.Contains(html, want) {
			t.Fatalf("merge-lane card missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "dt-pulse") {
		t.Fatalf("merge-lane progress dot must remain static:\n%s", html)
	}
}

func TestBoardCardExtraShowsTokenCeilingFailure(t *testing.T) {
	t.Parallel()

	reason := "token ceiling circuit breaker: observed 16100000 tokens above the 16000000 max_session_tokens ceiling"
	card := projectKanbanCard{
		BlockedSource:         telemetry.BlockedSourceProjectStatus,
		BlockedReason:         reason,
		BlockedRecoveryReason: "human_blocker",
	}

	kind, text, chip := boardCardExtra(card, boardCardView{})
	if kind != primitives.KindErr || text != "needs review - "+reason || !chip {
		t.Fatalf("boardCardExtra() = %q, %q, %t, want error token ceiling chip", kind, text, chip)
	}
}

func TestBoardCardPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		card       projectKanbanCard
		wantBadge  string
		wantDetail string
		wantTop    bool
	}{
		{
			name:       "top tracker priority",
			card:       projectKanbanCard{PriorityRank: 1, PriorityName: "P0"},
			wantBadge:  "P0",
			wantDetail: "Tracker priority P0 maps to dispatch rank 1.",
			wantTop:    true,
		},
		{
			name:       "top dispatch label",
			card:       projectKanbanCard{DispatchPriorityLabel: "hotfix", DispatchPriorityRank: 1},
			wantBadge:  "hotfix",
			wantDetail: "Label hotfix is configured at dispatch label rank 1.",
			wantTop:    true,
		},
		{
			name:       "tracker priority precedes top label",
			card:       projectKanbanCard{PriorityRank: 4, PriorityName: "P3", DispatchPriorityLabel: "hotfix", DispatchPriorityRank: 1},
			wantBadge:  "P3",
			wantDetail: "Tracker priority P3 maps to dispatch rank 4. Label hotfix is configured at dispatch label rank 1.",
		},
		{
			name:       "derived unblocker boost",
			card:       projectKanbanCard{UnblockerCount: 3},
			wantBadge:  "unblocker",
			wantDetail: "Unblocks 3 issues.",
		},
		{
			name:       "single derived dependent",
			card:       projectKanbanCard{UnblockerCount: 1},
			wantBadge:  "unblocker",
			wantDetail: "Unblocks 1 issue.",
		},
		{
			name: "unprioritized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			badge, title, detail, top := boardCardPriority(tt.card)
			if badge != tt.wantBadge || detail != tt.wantDetail || top != tt.wantTop {
				t.Fatalf("boardCardPriority() = %q/%q/%t, want %q/%q/%t", badge, detail, top, tt.wantBadge, tt.wantDetail, tt.wantTop)
			}
			if badge == "" && title != "" {
				t.Fatalf("title = %q without priority badge", title)
			}
			if badge != "" && title != "Dispatch priority" {
				t.Fatalf("title = %q, want Dispatch priority", title)
			}
		})
	}
}

func TestBoardViewUsesPerProjectDispatchLabelConfig(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	older := now.Add(-2 * time.Hour)
	newer := now.Add(-time.Hour)
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{ID: "docs-hotfix", Identifier: "digitaldrywood/docs#1", ProjectID: "docs", Title: "Unconfigured label", State: "Todo", Labels: []string{"hotfix"}, StageUpdatedAt: &older},
				{ID: "detent-hotfix", Identifier: "digitaldrywood/detent#2", ProjectID: "detent", Title: "Configured label", State: "Todo", Labels: []string{"hotfix"}, StageUpdatedAt: &newer},
			},
		},
		Kanban: KanbanData{
			States: []string{"Todo"},
			Projects: map[string]KanbanProjectData{
				"detent": {ProjectID: "detent", DispatchPriorityByLabel: []string{"hotfix"}},
				"docs":   {ProjectID: "docs"},
			},
		},
	}

	view := boardViewFromDashboard(data)
	cards := view.Lanes[0].Cards
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2", len(cards))
	}
	if cards[0].IssueID != "detent-hotfix" || cards[0].PriorityBadge != "hotfix" {
		t.Fatalf("first card = %#v, want configured detent hotfix", cards[0])
	}
	if cards[1].IssueID != "docs-hotfix" || cards[1].PriorityBadge != "" {
		t.Fatalf("second card = %#v, want unprioritized docs card", cards[1])
	}
}

func TestBoardViewUsesTopLevelDispatchLabelConfigForLegacyHomeBoard(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	older := now.Add(-2 * time.Hour)
	newer := now.Add(-time.Hour)
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent"},
			BoardIssues: []telemetry.Issue{
				{ID: "plain", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", Title: "Older plain issue", State: "Todo", StageUpdatedAt: &older},
				{ID: "hotfix", Identifier: "digitaldrywood/detent#2", ProjectID: "detent", Title: "Newer hotfix", State: "Todo", Labels: []string{"hotfix"}, StageUpdatedAt: &newer},
			},
		},
		Kanban: KanbanData{
			States:                  []string{"Todo"},
			DispatchPriorityByLabel: []string{"hotfix"},
		},
	}

	view := boardViewFromDashboard(data)
	cards := view.Lanes[0].Cards
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2", len(cards))
	}
	if cards[0].IssueID != "hotfix" || cards[0].PriorityBadge != "hotfix" {
		t.Fatalf("first card = %#v, want prioritized legacy home-board hotfix", cards[0])
	}
}

func boardPriorityPointer(value int) *int {
	return &value
}

func TestRunningBoardCardAndDetailSheetRenderRuntimeIdentity(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	identity := agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", data.Snapshot.GeneratedAt.Add(-time.Minute)).
		Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", data.Snapshot.GeneratedAt))
	data.Snapshot.Running[0].RuntimeIdentity = identity
	data.Snapshot.Running[0].SessionID = "thread-185"
	data.Snapshot.Running[0].DetentSessionID = 185
	view := boardViewFromDashboard(data)
	var running boardCardView
	for _, lane := range view.Lanes {
		for _, card := range lane.Cards {
			if card.Running {
				running = card
			}
		}
	}
	if running.RuntimeSummary != "Codex · openai · gpt-5.6-sol · xhigh" {
		t.Fatalf("RuntimeSummary = %q", running.RuntimeSummary)
	}
	if !running.RuntimeBadge {
		t.Fatal("RuntimeBadge = false, want true")
	}
	if running.RuntimeCozyText != "gpt-5.6-sol · xhigh" {
		t.Fatalf("RuntimeCozyText = %q", running.RuntimeCozyText)
	}
	if running.RuntimeComfyText != "Codex · gpt-5.6-sol · xhigh" {
		t.Fatalf("RuntimeComfyText = %q", running.RuntimeComfyText)
	}
	if running.RuntimeDetail != "Provider: openai · Provider session: thread-185 · Role: code · Detent session: 185" {
		t.Fatalf("RuntimeDetail = %q", running.RuntimeDetail)
	}

	cardHTML := renderBoardComponent(t, boardCardView2(running))
	for _, want := range []string{
		`id="card-gopherguides-gopher-ai-185-runtime-badge"`,
		`data-board-runtime-badge`,
		`data-help-trigger`,
		`data-help-scope="runtime-identity"`,
		`data-help-description="Provider: openai · Provider session: thread-185 · Role: code · Detent session: 185"`,
		`data-runtime-density="cozy"`,
		`>gpt-5.6-sol · xhigh<`,
		`data-runtime-density="comfy"`,
		`>Codex · gpt-5.6-sol · xhigh<`,
	} {
		if !strings.Contains(cardHTML, want) {
			t.Fatalf("running card missing %q:\n%s", want, cardHTML)
		}
	}
	if strings.Contains(cardHTML, `>agent working<`) {
		t.Fatalf("resolved running card retained fallback badge text:\n%s", cardHTML)
	}
	card, ok := FindBoardCard(data, "gopher-ai", "gopherguides/gopher-ai#185")
	if !ok {
		t.Fatal("FindBoardCard() did not find running card")
	}
	sheetHTML := renderBoardComponent(t, BoardCardSheet(data, card, true, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{}))
	for _, want := range []string{
		"Agent system",
		"Codex",
		"Backend profile",
		"codex-high",
		"Provider",
		"openai · runtime",
		"Model",
		"gpt-5.6-sol · runtime",
		"Requested model",
		"gpt-5.5 · configured",
		"Effort",
		"xhigh · runtime",
		"Service tier",
		"priority · runtime",
	} {
		if !strings.Contains(sheetHTML, want) {
			t.Fatalf("detail sheet missing %q:\n%s", want, sheetHTML)
		}
	}
}

func TestRunningBoardCardKeepsRuntimeBadgeWithOperationalStatus(t *testing.T) {
	t.Parallel()

	resolvedIdentity := agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
		Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{}))
	tests := []struct {
		name     string
		identity agentidentity.Identity
		want     string
	}{
		{name: "resolved identity", identity: resolvedIdentity, want: "gpt-5.6-sol · xhigh"},
		{name: "telemetry lag", identity: agentidentity.Identity{}, want: "agent working"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			card := projectKanbanCard{
				Identifier:      "digitaldrywood/detent#1134",
				IssueID:         "issue-1134",
				IssueNumber:     "#1134",
				ProjectID:       "detent",
				Title:           "Inline runtime identity",
				Stage:           "In Progress",
				WaitDetail:      "Awaiting tool result",
				RuntimeIdentity: tt.identity,
			}
			data := DashboardData{Snapshot: telemetry.Snapshot{Running: []telemetry.Running{{
				Issue: telemetry.Issue{ID: card.IssueID, Identifier: card.Identifier, ProjectID: card.ProjectID},
			}}}}
			view := boardCardViewFromCard(
				data,
				projectKanbanLane{Title: "In Progress"},
				card,
				false,
				"fleet",
				"",
			)
			if !view.RuntimeBadge || view.RuntimeCozyText != tt.want {
				t.Fatalf("runtime badge = %#v", view)
			}
			if view.ExtraText != "Awaiting tool result" {
				t.Fatalf("ExtraText = %q", view.ExtraText)
			}

			html := renderBoardComponent(t, boardCardView2(view))
			for _, want := range []string{
				`data-board-runtime-badge`,
				`>` + tt.want + `<`,
				`Awaiting tool result`,
			} {
				if !strings.Contains(html, want) {
					t.Fatalf("running card missing %q:\n%s", want, html)
				}
			}
		})
	}
}

func TestBoardCardAuthorDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		author      string
		originActor string
		want        string
	}{
		{name: "author shown", author: " corylanou ", want: "@corylanou"},
		{name: "leading at normalized", author: "@corylanou", want: "@corylanou"},
		{name: "author absent"},
		{name: "author blank", author: " \t "},
		{name: "same as origin actor", author: "corylanou", originActor: "corylanou"},
		{name: "same as normalized origin actor", author: "@CoryLanou", originActor: " @corylanou "},
		{name: "different from origin actor", author: "loganlanou", originActor: "corylanou", want: "@loganlanou"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := boardCardAuthorDetail(test.author, test.originActor); got != test.want {
				t.Fatalf("boardCardAuthorDetail(%q, %q) = %q, want %q", test.author, test.originActor, got, test.want)
			}
		})
	}
}

func TestRunningBoardCardRuntimeBadgeFallsBackUntilIdentityKnown(t *testing.T) {
	t.Parallel()

	view := boardViewFromDashboard(boardTestData())
	var running boardCardView
	for _, lane := range view.Lanes {
		for _, card := range lane.Cards {
			if card.Running {
				running = card
			}
		}
	}
	if !running.RuntimeBadge {
		t.Fatal("RuntimeBadge = false, want true")
	}
	if running.RuntimeCozyText != "agent working" || running.RuntimeComfyText != "agent working" {
		t.Fatalf("fallback runtime texts = %q / %q", running.RuntimeCozyText, running.RuntimeComfyText)
	}

	html := renderBoardComponent(t, boardCardView2(running))
	for _, want := range []string{
		`id="card-gopherguides-gopher-ai-185-runtime-badge"`,
		`data-board-runtime-badge`,
		`>agent working<`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fallback running card missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `data-help-scope="runtime-identity"`) {
		t.Fatalf("fallback running card rendered an empty runtime flyout:\n%s", html)
	}
}

func TestBoardCardViewBuildsDensitySpecificContent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 16, 18, 0, 0, 0, time.UTC)
	identity := agentidentity.Configured("codex-high", "codex", "priority", "code", "gpt-5.6-sol", "openai", "xhigh", "", now)
	card := projectKanbanCard{
		Identifier:      "digitaldrywood/detent#1360",
		IssueID:         "issue-1360",
		IssueNumber:     "#1360",
		ProjectID:       "detent",
		Title:           "Make density informational",
		Stage:           "In Progress",
		AuthorID:        "corylanou",
		Labels:          []string{"detent:todo", "ux"},
		PRNumber:        1400,
		CIStatus:        "pass",
		CIClass:         "border-ok/15 bg-ok/15 text-ok",
		RuntimeIdentity: identity,
	}
	data := DashboardData{Snapshot: telemetry.Snapshot{Running: []telemetry.Running{{
		Issue:       telemetry.Issue{Identifier: card.Identifier, ProjectID: card.ProjectID},
		LastMessage: "Rendered the richer card fields.",
	}}}}
	view := boardCardViewFromCard(data, projectKanbanLane{Title: "In Progress"}, card, false, "fleet", "")

	if view.State != "In Progress" || view.CompactSignal != "pass" {
		t.Fatalf("compact content = %q / %q", view.State, view.CompactSignal)
	}
	if strings.Join(view.Labels, ",") != "detent:todo,ux" || view.Effort != "xhigh" {
		t.Fatalf("rich labels/effort = %v / %q", view.Labels, view.Effort)
	}
	if view.Activity != "Rendered the richer card fields." {
		t.Fatalf("Activity = %q", view.Activity)
	}
	if view.AuthorDetail != "@corylanou" {
		t.Fatalf("AuthorDetail = %q, want @corylanou", view.AuthorDetail)
	}
	if view.PRStatus != "PR #1400 · CI pass" || view.PRStatusClass != card.CIClass {
		t.Fatalf("PR status = %q / %q", view.PRStatus, view.PRStatusClass)
	}

	html := renderBoardComponent(t, boardCardView2(view))
	for _, want := range []string{
		`data-board-card-content="compact"`,
		`data-board-card-content="cozy"`,
		`data-board-card-content="comfy"`,
		`data-board-card-labels`,
		`data-board-card-author`,
		`Filed by`,
		`@corylanou`,
		`data-board-card-effort`,
		`data-board-card-activity`,
		`data-board-card-pr-status`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("density card missing %q:\n%s", want, html)
		}
	}
}

func TestBoardCardAlwaysRendersPullRequest(t *testing.T) {
	t.Parallel()

	const (
		prLabel = "PR #1859"
		prURL   = "https://github.com/digitaldrywood/detent/pull/1859"
	)
	tests := []struct {
		name     string
		lane     projectKanbanLane
		terminal bool
		prepare  func(*DashboardData, *projectKanbanCard)
	}{
		{
			name: "running",
			lane: projectKanbanLane{Title: "In Progress"},
			prepare: func(data *DashboardData, card *projectKanbanCard) {
				data.Snapshot.Running = []telemetry.Running{{Issue: telemetry.Issue{
					ID: card.IssueID, Identifier: card.Identifier, ProjectID: card.ProjectID,
				}}}
			},
		},
		{
			name:     "terminal done",
			lane:     projectKanbanLane{Title: "Done"},
			terminal: true,
			prepare: func(_ *DashboardData, card *projectKanbanCard) {
				card.Stage = "Done"
				card.RecentCompletion = true
			},
		},
		{
			name: "move disabled",
			lane: projectKanbanLane{Title: "Todo"},
			prepare: func(data *DashboardData, _ *projectKanbanCard) {
				data.Snapshot.LastKnown = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := DashboardData{
				Snapshot: telemetry.Snapshot{GeneratedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)},
				Kanban: KanbanData{
					Mode:               "integration",
					CanMoveCards:       true,
					AllowedTransitions: map[string][]string{"Todo": {"In Progress"}, "In Progress": {"Done"}},
				},
			}
			card := projectKanbanCard{
				Identifier:  "digitaldrywood/detent#1859",
				IssueID:     "issue-1859",
				IssueNumber: "#1859",
				ProjectID:   "detent",
				Title:       "Prioritize the pull request link",
				Stage:       tt.lane.Title,
				PRNumber:    1859,
				PRURL:       prURL,
				Movable:     true,
			}
			tt.prepare(&data, &card)

			view := boardCardViewFromCard(data, tt.lane, card, tt.terminal, "fleet", "detent")
			if view.MetaRight != prLabel {
				t.Fatalf("MetaRight = %q, want %q", view.MetaRight, prLabel)
			}
			html := renderBoardComponent(t, boardCardView2(view))
			for _, density := range []string{"cozy", "compact"} {
				section := boardCardDensitySection(t, html, density)
				for _, want := range []string{`href="` + prURL + `"`, `>` + prLabel + `</a>`} {
					if !strings.Contains(section, want) {
						t.Fatalf("%s card missing %q:\n%s", density, want, section)
					}
				}
				if badge := strings.Index(section, `data-kanban-move-disabled-label`); badge >= 0 && strings.Index(section, prLabel) > badge {
					t.Fatalf("%s card renders move-disabled metadata before %s:\n%s", density, prLabel, section)
				}
				if done := strings.Index(section, `aria-label="done"`); done >= 0 && strings.Index(section, prLabel) > done {
					t.Fatalf("%s card renders done metadata before %s:\n%s", density, prLabel, section)
				}
			}
		})
	}
}

func TestBoardCardHeaderKeepsPullRequestAfterTruncatingMetadata(t *testing.T) {
	t.Parallel()

	card := boardCardView{
		DomID:          "card-detent-1629",
		Number:         "#1629",
		URL:            "https://github.com/digitaldrywood/detent/issues/1629",
		Project:        "digitaldrywood-release-train-platform",
		PriorityBadge:  "priority-medium",
		PriorityTitle:  "Dispatch priority",
		PriorityDetail: "Label priority-medium is configured at dispatch label rank 2.",
		MetaRight:      "PR #1630",
		PRURL:          "https://github.com/digitaldrywood/detent/pull/1630",
		Title:          "Keep pull request metadata inside the card",
	}
	html := renderBoardComponent(t, boardCardView2(card))

	tests := []struct {
		name string
		want string
	}{
		{name: "project can shrink to zero", want: `<span class="min-w-0 truncate">digitaldrywood-release-train-platform</span>`},
		{name: "priority badge preserves an ellipsis", want: `class="inline-flex min-w-7 max-w-24 shrink items-center`},
		{name: "priority text paints ellipsis", want: `<span class="min-w-0 truncate">priority-medium</span>`},
		{name: "pull request never shrinks", want: `class="ml-auto flex-none whitespace-nowrap tabular-nums"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(html, tt.want) {
				t.Fatalf("board card header missing %q:\n%s", tt.want, html)
			}
		})
	}
}

func TestBoardCardActivityPreview(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("activity ", 20)
	if got := boardCardActivityPreview("  one\n two  "); got != "one two" {
		t.Fatalf("normalized preview = %q", got)
	}
	got := boardCardActivityPreview(long)
	if len([]rune(got)) != 96 || !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated preview = %q (%d runes)", got, len([]rune(got)))
	}
}

func TestDetailSheetRendersUnknownRuntimeIdentityValuesAsUnavailable(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.Running[0].RuntimeIdentity = agentidentity.Configured(
		"claude-local",
		"claude_code",
		"local",
		"code",
		"fable",
		"",
		"",
		"",
		data.Snapshot.GeneratedAt.Add(-time.Minute),
	).Merge(agentidentity.RuntimeUpdate("qwen3-coder", "", "", "", data.Snapshot.GeneratedAt))

	card, ok := FindBoardCard(data, "gopher-ai", "gopherguides/gopher-ai#185")
	if !ok {
		t.Fatal("FindBoardCard() did not find running card")
	}
	html := renderBoardComponent(t, BoardCardSheet(data, card, true, false, KanbanConversationData{}, BoardActivityData{}, BoardSessionData{}))
	for _, want := range []string{
		"Claude Code",
		"claude-local",
		"Provider",
		"Model",
		"qwen3-coder · runtime",
		"Effort",
		"Unavailable · unknown",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("detail sheet missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "Service tier") {
		t.Fatalf("detail sheet rendered an unknown optional service tier:\n%s", html)
	}
}

func TestBoardLaneVisibilityResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		defaultVisible bool
		state          boardLaneVisibilityState
		want           bool
	}{
		{name: "auto follows shown default", defaultVisible: true, state: boardLaneVisibilityAuto, want: true},
		{name: "auto follows hidden default", defaultVisible: false, state: boardLaneVisibilityAuto, want: false},
		{name: "show overrides hidden default", defaultVisible: false, state: boardLaneVisibilityShow, want: true},
		{name: "hide overrides shown default", defaultVisible: true, state: boardLaneVisibilityHide, want: false},
		{name: "unknown state falls back to default", defaultVisible: true, state: boardLaneVisibilityState("unknown"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := boardLaneVisibilityResolve(tt.defaultVisible, tt.state); got != tt.want {
				t.Fatalf("boardLaneVisibilityResolve(%t, %q) = %t, want %t", tt.defaultVisible, tt.state, got, tt.want)
			}
		})
	}
}

func TestBoardLaneVisibilityPrefsFromStorage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		lane      string
		wantState boardLaneVisibilityState
		wantClear bool
	}{
		{name: "empty is auto", lane: "todo", wantState: boardLaneVisibilityAuto},
		{name: "current show state", raw: `{"v":1,"show":["todo"]}`, lane: "todo", wantState: boardLaneVisibilityShow},
		{name: "current hide state", raw: `{"v":1,"hide":["done"]}`, lane: "done", wantState: boardLaneVisibilityHide},
		{name: "show wins conflicting state", raw: `{"v":1,"show":["todo"],"hide":["todo"]}`, lane: "todo", wantState: boardLaneVisibilityShow},
		{name: "blank ids are discarded", raw: `{"v":1,"show":[" ","todo"],"hide":[""]}`, lane: "todo", wantState: boardLaneVisibilityShow},
		{name: "legacy boolean prefs clear to auto", raw: `{"todo":false}`, lane: "todo", wantState: boardLaneVisibilityAuto, wantClear: true},
		{name: "legacy visible lane list clears to auto", raw: `{"lanes":["todo"]}`, lane: "todo", wantState: boardLaneVisibilityAuto, wantClear: true},
		{name: "unknown version clears to auto", raw: `{"v":99,"show":["todo"]}`, lane: "todo", wantState: boardLaneVisibilityAuto, wantClear: true},
		{name: "invalid json clears to auto", raw: `{`, lane: "todo", wantState: boardLaneVisibilityAuto, wantClear: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefs, clear := boardLaneVisibilityPrefsFromStorage(tt.raw)
			if clear != tt.wantClear {
				t.Fatalf("clear = %t, want %t", clear, tt.wantClear)
			}
			if got := boardLaneVisibilityStateForLane(prefs, tt.lane); got != tt.wantState {
				t.Fatalf("state for %q = %q, want %q; prefs = %+v", tt.lane, got, tt.wantState, prefs)
			}
		})
	}
}

func TestBoardSnapshotRendersAwaitingChecksOnlyForGatePendingCards(t *testing.T) {
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
			BoardIssues: []telemetry.Issue{
				{
					ID:          "gate-pending",
					Identifier:  "digitaldrywood/detent#1030",
					ProjectID:   "detent",
					Title:       "Completed work waiting on checks",
					State:       "In Progress",
					GatePending: true,
				},
				{
					ID:         "active-work",
					Identifier: "digitaldrywood/detent#1031",
					ProjectID:  "detent",
					Title:      "Active work still running",
					State:      "In Progress",
				},
			},
		},
		Kanban: KanbanData{States: []string{"In Progress"}},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	flagged := boardCardSection(t, html, "Completed work waiting on checks")
	if !strings.Contains(flagged, "Awaiting checks") {
		t.Fatalf("gate-pending card missing awaiting checks badge:\n%s", flagged)
	}
	plain := boardCardSection(t, html, "Active work still running")
	if strings.Contains(plain, "Awaiting checks") {
		t.Fatalf("plain active card rendered awaiting checks badge:\n%s", plain)
	}
	if got := strings.Count(html, ">Awaiting checks<"); got != 2 {
		t.Fatalf("density-specific Awaiting checks labels rendered %d times, want 2:\n%s", got, html)
	}
}

func TestLongLocalWorkItemIdentifiersUseDefensiveTruncationClasses(t *testing.T) {
	localID := "wi-011cd179bc7ecf36b7197e4b"
	projectID := "digitaldrywood-video"
	title := "Local SQLite work item"
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
			BoardIssues: []telemetry.Issue{
				{
					ID:         localID,
					Identifier: localID,
					ProjectID:  projectID,
					Title:      title,
					State:      "Todo",
				},
			},
			Pipeline: []telemetry.Issue{
				{
					ID:         localID,
					Identifier: localID,
					ProjectID:  projectID,
					Title:      title,
					State:      "Human Review",
				},
			},
		},
		Kanban: KanbanData{States: []string{"Todo"}},
	}

	boardHTML := renderBoardComponent(t, BoardSnapshot(data))
	boardCard := boardCardSection(t, boardHTML, title)
	for _, want := range []string{
		`class="flex-none max-w-16 truncate text-text">` + localID,
		`class="min-w-0 truncate">` + projectID,
	} {
		if !strings.Contains(boardCard, want) {
			t.Fatalf("board card missing %q:\n%s", want, boardCard)
		}
	}

	sheetHTML := renderBoardComponent(t, BoardCardSheet(
		DashboardData{},
		projectKanbanCard{Identifier: localID, IssueNumber: localID, ProjectID: projectID, Title: title, Stage: "Todo"},
		true,
		false,
		KanbanConversationData{},
		BoardActivityData{},
		BoardSessionData{},
	))
	for _, want := range []string{
		`class="min-w-20 truncate">` + projectID,
		`class="inline-flex min-h-11 max-w-48 flex-none items-center truncate text-text md:min-h-0 md:inline">` + localID,
	} {
		if !strings.Contains(sheetHTML, want) {
			t.Fatalf("sheet header missing %q:\n%s", want, sheetHTML)
		}
	}

	fleetHTML := renderBoardComponent(t, FleetSnapshotV2(data))
	for _, want := range []string{
		`class="flex-none max-w-24 truncate text-text">` + localID,
		`class="min-w-16 truncate">` + projectID,
	} {
		if !strings.Contains(fleetHTML, want) {
			t.Fatalf("fleet PR card missing %q:\n%s", want, fleetHTML)
		}
	}

	runsHTML := renderBoardComponent(t, projectRunsTable("project-runs", []projectRunRow{{DomID: "run-local", Ref: localID, Title: title}}))
	if want := `class="min-w-0 truncate font-mono text-xs font-medium text-text tabular-nums">` + localID; !strings.Contains(runsHTML, want) {
		t.Fatalf("project runs row missing %q:\n%s", want, runsHTML)
	}

	exceptionHTML := renderBoardComponent(t, primitives.ExceptionStrip([]primitives.Exception{
		{ID: "exception-local", Kind: primitives.KindErr, Title: "Needs review", Repo: projectID, Ref: localID, Rest: "needs operator approval"},
	}))
	if want := `class="flex-none max-w-24 truncate font-mono text-xs font-medium text-text">` + localID; !strings.Contains(exceptionHTML, want) {
		t.Fatalf("exception strip missing %q:\n%s", want, exceptionHTML)
	}
}

func TestBoardCardAgeFooterVisibility(t *testing.T) {
	card := projectKanbanCard{
		IssueNumber:      "#982",
		ProjectID:        "detent",
		Title:            "Restore active-lane aging on Kanban cards",
		TimeInStage:      "12m 4s",
		TimeInStageTitle: "In Progress since 12:30 UTC (12m 4s)",
	}
	tests := []struct {
		name         string
		lane         string
		terminal     bool
		prNumber     int
		wantAge      string
		wantMeta     string
		wantDone     bool
		wantTerminal bool
	}{
		{name: "in progress", lane: "In Progress", wantAge: "12m"},
		{name: "production working lane", lane: "Production", wantAge: "12m"},
		{name: "rework", lane: "Rework", wantAge: "12m"},
		{name: "blocked", lane: "Blocked", wantAge: "12m"},
		{name: "review pr keeps metadata", lane: "Human Review", prNumber: 314, wantAge: "12m", wantMeta: "PR #314"},
		{name: "merging", lane: "Merging", prNumber: 315, wantAge: "12m", wantMeta: "PR #315"},
		{name: "backlog suppressed", lane: "Backlog"},
		{name: "todo suppressed", lane: "Todo"},
		{name: "done suppressed", lane: "Done", terminal: true, wantDone: true, wantTerminal: true},
		{name: "cancelled suppressed by terminal flag", lane: "Cancelled", terminal: true, wantTerminal: true},
		{name: "closed suppressed by lane name", lane: "Closed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := card
			candidate.PRNumber = tt.prNumber
			got := boardCardViewFromCard(DashboardData{}, projectKanbanLane{Title: tt.lane}, candidate, tt.terminal, "fleet", "detent")
			if got.AgeFooter != tt.wantAge {
				t.Fatalf("AgeFooter = %q, want %q; card = %+v", got.AgeFooter, tt.wantAge, got)
			}
			if got.AgeFooterTitle != "" && got.AgeFooterTitle != candidate.TimeInStageTitle {
				t.Fatalf("AgeFooterTitle = %q, want %q", got.AgeFooterTitle, candidate.TimeInStageTitle)
			}
			if got.MetaRight != tt.wantMeta {
				t.Fatalf("MetaRight = %q, want %q; card = %+v", got.MetaRight, tt.wantMeta, got)
			}
			if got.Done != tt.wantDone {
				t.Fatalf("Done = %t, want %t; card = %+v", got.Done, tt.wantDone, got)
			}
			if got.Terminal != tt.wantTerminal {
				t.Fatalf("Terminal = %t, want %t; card = %+v", got.Terminal, tt.wantTerminal, got)
			}
		})
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
	if len(figures) != 5 {
		t.Fatalf("expected 5 figures, got %d", len(figures))
	}
	byID := map[string]primitives.Figure{}
	for _, figure := range figures {
		byID[figure.ID] = figure
	}
	if byID["fig-blocked"].Value != "1" || !byID["fig-blocked"].Err {
		t.Fatalf("blocked figure should be err-colored when > 0: %+v", byID["fig-blocked"])
	}
	if byID["fig-ready"].Err {
		t.Fatalf("zero ready figure must stay neutral")
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

func TestBoardFiguresSeparateWaitingFromBlockedLane(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{
			{ID: "ready", State: "Todo"},
			{ID: "waiting", State: "Todo"},
			{ID: "blocked", State: "Blocked"},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "waiting", State: "Todo"}, Source: telemetry.BlockedSourceDependency},
			{Issue: telemetry.Issue{ID: "blocked", State: "Blocked"}, Error: "needs operator action"},
		},
	}

	byID := map[string]primitives.Figure{}
	for _, figure := range boardFigures(snapshot) {
		byID[figure.ID] = figure
	}
	if byID["fig-ready"].Value != "1" {
		t.Fatalf("ready figure = %+v, want 1", byID["fig-ready"])
	}
	if byID["fig-waiting"].Value != "1" {
		t.Fatalf("waiting figure = %+v, want 1", byID["fig-waiting"])
	}
	if byID["fig-blocked"].Value != "1" || !byID["fig-blocked"].Err {
		t.Fatalf("blocked figure = %+v, want exact blocked-lane count", byID["fig-blocked"])
	}
}

func TestBoardFiguresExposeIncompleteWorkloadProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		snapshot    telemetry.Snapshot
		wantReady   string
		wantWaiting string
		wantBlocked string
	}{
		{
			name: "complete projection ignores completed-only rows",
			snapshot: telemetry.Snapshot{
				Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				Completed: []telemetry.Completed{
					{Issue: telemetry.Issue{ID: "blocked", State: "Blocked"}, FinalState: "completed"},
					{Issue: telemetry.Issue{ID: "review", State: "Human Review"}, FinalState: "completed"},
				},
			},
			wantReady:   "0",
			wantWaiting: "0",
			wantBlocked: "0",
		},
		{
			name: "incomplete projection reports lower bound and unknown zeros",
			snapshot: telemetry.Snapshot{
				Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed},
				Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				Completed: []telemetry.Completed{
					{Issue: telemetry.Issue{ID: "blocked", State: "Blocked"}, FinalState: "completed"},
					{Issue: telemetry.Issue{ID: "review", State: "Human Review"}, FinalState: "completed"},
				},
			},
			wantReady:   "unknown",
			wantWaiting: "unknown",
			wantBlocked: "unknown",
		},
		{
			name: "incomplete projection reports current rows as lower bounds",
			snapshot: telemetry.Snapshot{
				Tracker:     telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed},
				Runtime:     telemetry.SnapshotSection{Source: telemetry.SnapshotSourceLive, Complete: true},
				BoardIssues: []telemetry.Issue{{ID: "blocked", State: "Blocked"}},
			},
			wantReady:   "unknown",
			wantWaiting: "unknown",
			wantBlocked: "1+",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			byID := map[string]primitives.Figure{}
			for _, figure := range boardFigures(tt.snapshot) {
				byID[figure.ID] = figure
			}
			if got := byID["fig-ready"].Value; got != tt.wantReady {
				t.Fatalf("ready figure = %q, want %q", got, tt.wantReady)
			}
			if got := byID["fig-waiting"].Value; got != tt.wantWaiting {
				t.Fatalf("waiting figure = %q, want %q", got, tt.wantWaiting)
			}
			if got := byID["fig-blocked"].Value; got != tt.wantBlocked {
				t.Fatalf("blocked figure = %q, want %q", got, tt.wantBlocked)
			}
		})
	}
}

func TestBoardExceptionsRequireOptIn(t *testing.T) {
	data := boardTestData()
	if got := boardExceptions(data, true); len(got) != 0 {
		t.Fatalf("default board should hide elevated blocked alerts, got %d", len(got))
	}

	data.Kanban.ShowBlockedAlerts = true
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
	if len(fleetExceptions) != 1 {
		t.Fatalf("expected one fleet exception, got %d", len(fleetExceptions))
	}
	if got, _ := fleetExceptions[0].ActionAttrs["hx-get"].(string); strings.Contains(got, "actions=board") {
		t.Fatalf("fleet exception review target should omit board actions, got %v", got)
	}

	data.Snapshot.Blocked = nil
	if got := boardExceptions(data, true); len(got) != 0 {
		t.Fatalf("healthy snapshot should produce no exceptions, got %d", len(got))
	}
}

func TestBoardExceptionsSuppressExpectedWaitingBlocks(t *testing.T) {
	blockedAt := time.Date(2026, 7, 4, 16, 30, 0, 0, time.UTC)
	tests := []struct {
		name    string
		blocked telemetry.Blocked
	}{
		{
			name: "dependency source waits on card only",
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
		},
		{
			name: "project status waits on card only",
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
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := boardTestData()
			data.Snapshot.GeneratedAt = blockedAt.Add(12 * time.Minute)
			data.Snapshot.Blocked = []telemetry.Blocked{tt.blocked}
			data.Kanban.ShowBlockedAlerts = true

			exceptions := boardExceptions(data, true)
			if len(exceptions) != 0 {
				t.Fatalf("expected waiting block to stay out of elevated alerts, got %+v", exceptions)
			}
		})
	}
}

func TestBoardExceptionsExposeOperatorStopRoutingRetry(t *testing.T) {
	data := boardTestData()
	data.Kanban.ShowBlockedAlerts = true
	data.Snapshot.Blocked = []telemetry.Blocked{
		{
			Issue:           telemetry.Issue{ID: "issue-1435", Identifier: "digitaldrywood/detent#1435", ProjectID: "detent"},
			Error:           "operator stop is waiting for the worker to exit",
			Source:          telemetry.BlockedSourceOperatorStop,
			RecoveryReason:  "pending",
			Attempt:         2,
			WorkAttemptID:   1435,
			DetentSessionID: 608,
			SessionID:       "thread-1435",
			Destination:     "Blocked",
		},
	}
	if exceptions := boardExceptions(data, true); len(exceptions) != 0 {
		t.Fatalf("pending operator stop exceptions = %+v, want none", exceptions)
	}

	data.Snapshot.Blocked[0].RecoveryReason = "transition_failed"
	data.Snapshot.Blocked[0].Error = "run stopped; retry the transition to Blocked: tracker unavailable"
	exceptions := boardExceptions(data, true)
	if len(exceptions) != 1 {
		t.Fatalf("transition failure exceptions = %+v, want one", exceptions)
	}
	exception := exceptions[0]
	if exception.Title != "Run stopped; routing failed" || exception.ActionLabel != "Retry routing" {
		t.Fatalf("exception = %+v", exception)
	}
	if got, _ := exception.ActionAttrs["hx-get"].(string); !strings.Contains(got, "/api/v1/projects/detent/runs/2/stop") {
		t.Fatalf("retry path = %q", got)
	}
	if exception.ActionAttrs["data-tui-dialog-target"] != kanbanActionDialogID {
		t.Fatalf("dialog target = %#v", exception.ActionAttrs["data-tui-dialog-target"])
	}
}

func TestBoardExceptionsCompactHumanBlocks(t *testing.T) {
	data := boardTestData()
	blockedAt := data.Snapshot.GeneratedAt.Add(-12 * time.Minute)
	data.Kanban.ShowBlockedAlerts = true
	data.Snapshot.Blocked = []telemetry.Blocked{
		{
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
		{
			Issue: telemetry.Issue{
				ID:         "issue-179",
				Identifier: "digitaldrywood/detent#179",
				ProjectID:  "detent",
				State:      "Blocked",
			},
			Error:          "missing credentials",
			Source:         telemetry.BlockedSourceProjectStatus,
			RecoveryReason: "human_blocker",
			BlockedAt:      &blockedAt,
		},
	}

	exceptions := boardExceptions(data, true)
	if len(exceptions) != 1 {
		t.Fatalf("expected compact blocker summary, got %d", len(exceptions))
	}
	exception := exceptions[0]
	if exception.ID != "exception-blocked-review" {
		t.Fatalf("summary id = %q, want exception-blocked-review", exception.ID)
	}
	if exception.Kind != primitives.KindErr {
		t.Fatalf("summary kind = %q, want err", exception.Kind)
	}
	if exception.Title != "2 blocked items need review" {
		t.Fatalf("summary title = %q", exception.Title)
	}
	if !strings.Contains(exception.Rest, "needs operator approval") || !strings.Contains(exception.Rest, "plus 1 more") {
		t.Fatalf("summary rest = %q", exception.Rest)
	}
	if exception.ActionLabel != "" {
		t.Fatalf("multi-blocker summary should not render one row action, got %+v", exception)
	}
}

func TestBoardExceptionsDistinguishHeldRecoveryFromDefer(t *testing.T) {
	t.Parallel()

	blockedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		blocked        telemetry.Blocked
		wantExceptions int
		wantDetail     string
	}{
		{
			name: "held recovery needs review",
			blocked: telemetry.Blocked{
				Issue:               telemetry.Issue{ID: "issue-held", Identifier: "digitaldrywood/detent#6", ProjectID: "detent", State: "Blocked"},
				Source:              telemetry.BlockedSourceProjectStatus,
				RecoveryAction:      "hold",
				RecoveryReason:      "invalid_workpad_signal",
				RecoveryRemedy:      "Move the issue to Todo or another fresh-work lane; no pull request exists to resume.",
				NeedsHumanAttention: true,
				BlockedAt:           &blockedAt,
			},
			wantExceptions: 1,
			wantDetail:     "fresh-work lane",
		},
		{
			name: "deferred dependency stays out of review",
			blocked: telemetry.Blocked{
				Issue:          telemetry.Issue{ID: "issue-deferred", Identifier: "digitaldrywood/detent#17", ProjectID: "detent", State: "Blocked"},
				Source:         telemetry.BlockedSourceProjectStatus,
				RecoveryAction: "defer",
				RecoveryReason: "dependency_recovery",
				RecoveryRoot: &telemetry.BlockedRecoveryRoot{
					IssueIdentifier: "digitaldrywood/detent#6",
					Reason:          "invalid_workpad_signal",
				},
				BlockedAt: &blockedAt,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := boardTestData()
			data.Kanban.ShowBlockedAlerts = true
			data.Snapshot.GeneratedAt = blockedAt.Add(time.Hour)
			data.Snapshot.Blocked = []telemetry.Blocked{tt.blocked}

			exceptions := boardExceptions(data, true)
			if len(exceptions) != tt.wantExceptions {
				t.Fatalf("exceptions = %#v", exceptions)
			}
			if tt.wantDetail != "" && !strings.Contains(exceptions[0].Rest, tt.wantDetail) {
				t.Fatalf("exception detail = %q", exceptions[0].Rest)
			}
		})
	}
}

func TestBoardBlockerEvidenceDetailSurfacesUnverifiableAgeAndOwner(t *testing.T) {
	t.Parallel()

	recordedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	detail := boardBlockerEvidenceDetail(telemetry.Blocked{BlockerEvidence: []telemetry.BlockerEvidence{{
		Type:         "free_text",
		Owner:        "human",
		Status:       "unverifiable",
		Unverifiable: true,
		RecordedAt:   &recordedAt,
	}}}, recordedAt.Add(90*time.Minute))
	for _, want := range []string{"unverifiable free text", "owner human", "age 1h 30m"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail = %q, want containing %q", detail, want)
		}
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
				State:      "Todo",
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
		if lane.Title != "Todo" {
			continue
		}
		for _, candidate := range lane.Cards {
			if candidate.Number == "#176" {
				card = candidate
			}
		}
	}

	if card.Number != "#176" {
		t.Fatalf("missing dependency-waiting card in Todo lane: %+v", view.Lanes)
	}
	if card.ExtraKind != primitives.KindWarn || !card.ExtraChip {
		t.Fatalf("card extra = %q chip %t, want warn chip; card = %+v", card.ExtraKind, card.ExtraChip, card)
	}
	if !strings.Contains(card.ExtraText, "waiting on digitaldrywood/detent#170 (In Progress)") {
		t.Fatalf("card extra text = %q", card.ExtraText)
	}
	cardClass := boardCardClass(card)
	if !strings.Contains(cardClass, "border-warn/45") || strings.Contains(cardClass, "border-err/45") {
		t.Fatalf("card class = %q, want warning border without error border", cardClass)
	}
}

func TestBoardBlockedCauseBadge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		card     projectKanbanCard
		wantKind primitives.Kind
		wantText string
	}{
		{
			name: "unresolved dependency names ref and state",
			card: projectKanbanCard{
				BlockedSource:         telemetry.BlockedSourceProjectStatus,
				BlockedReason:         "waiting on gopherguides/corp#526 (In Progress)",
				BlockedRecoveryAction: "defer",
				BlockedRecoveryReason: "dependency_recovery",
				Blockers:              []string{"gopherguides/corp#526 (In Progress)"},
			},
			wantKind: primitives.KindWarn,
			wantText: "waiting on gopherguides/corp#526 (In Progress)",
		},
		{
			name: "satisfied refs cannot claim dependency wait",
			card: projectKanbanCard{
				BlockedSource:         telemetry.BlockedSourceProjectStatus,
				BlockedReason:         "blocked by project status",
				BlockedRecoveryReason: "dependency_blocker",
				ClearedBlockers:       []string{"gopherguides/corp#526 (Done)"},
			},
			wantKind: primitives.KindWarn,
			wantText: "waiting - project status",
		},
		{
			name: "transient resource wait stays passive",
			card: projectKanbanCard{
				BlockedSource:         telemetry.BlockedSourceProjectStatus,
				BlockedReason:         "transient GitHub REST budget waiting for capacity: remaining=940/5000 reserve=1250",
				BlockedRecoveryAction: "defer",
				BlockedRecoveryReason: "github_rest_budget_wait",
			},
			wantKind: primitives.KindWarn,
			wantText: "waiting - transient GitHub REST budget waiting for capacity: remaining=940/5000 reserve=1250",
		},
		{
			name: "unrecorded cause needs review",
			card: projectKanbanCard{
				BlockedSource: telemetry.BlockedSourceProjectStatus,
				BlockedReason: "blocked, cause unrecorded",
			},
			wantKind: primitives.KindErr,
			wantText: "needs review - blocked, cause unrecorded",
		},
		{
			name: "external block without tracker cause needs review",
			card: projectKanbanCard{
				BlockedSource: telemetry.BlockedSourceProjectStatus,
				BlockedReason: staleness.ReasonBlockedOutsideDetent,
			},
			wantKind: primitives.KindErr,
			wantText: "needs review - " + staleness.ReasonBlockedOutsideDetent,
		},
		{
			name: "human delivery failure needs review",
			card: projectKanbanCard{
				BlockedSource:         telemetry.BlockedSourceProjectStatus,
				BlockedReason:         "no_commits_to_deliver: branch detent/gopher-corp-example has no local commits ahead",
				BlockedRecoveryAction: "hold",
				BlockedRecoveryReason: "no_commits_to_deliver",
				BlockedRecoveryRemedy: "return the issue to Todo when implementation work is ready to resume",
			},
			wantKind: primitives.KindErr,
			wantText: "needs review - no_commits_to_deliver: branch detent/gopher-corp-example has no local commits ahead — return the issue to Todo when implementation work is ready to resume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind, text, chip := boardCardExtra(tt.card, boardCardView{})
			if kind != tt.wantKind || text != tt.wantText || !chip {
				t.Fatalf("boardCardExtra() = %q, %q, %t, want %q, %q, true", kind, text, chip, tt.wantKind, tt.wantText)
			}
			if strings.Contains(text, "dependency not ready") {
				t.Fatalf("boardCardExtra() exposed unnamed dependency: %q", text)
			}
		})
	}
}

func TestBoardBlockedTimelineCauseRendersRecordedReason(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	blockedAt := data.Snapshot.GeneratedAt.Add(-time.Minute)
	cause := "current tracker lane is not worker-owned"
	data.Snapshot.Blocked = []telemetry.Blocked{{
		Issue: telemetry.Issue{
			ID:         "issue-revoked-block",
			Identifier: "digitaldrywood/detent#1995",
			ProjectID:  "detent",
			Title:      "Revoked worker lane",
			State:      "Blocked",
		},
		Error:     cause,
		Source:    telemetry.BlockedSourceProjectStatus,
		BlockedAt: &blockedAt,
	}}

	view := boardViewFromDashboard(data)
	var card boardCardView
	for _, lane := range view.Lanes {
		for _, candidate := range lane.Cards {
			if candidate.IssueID == "issue-revoked-block" {
				card = candidate
			}
		}
	}
	if card.IssueID == "" {
		t.Fatalf("rendered board missing revoked Blocked card: %#v", view.Lanes)
	}

	html := renderBoardComponent(t, boardCardView2(card))
	if !strings.Contains(html, cause) {
		t.Fatalf("rendered blocked card missing timeline cause %q:\n%s", cause, html)
	}
	if strings.Contains(html, "cause unrecorded") {
		t.Fatalf("rendered blocked card contradicted its timeline:\n%s", html)
	}
}

func TestBoardBlockedCardCauseUsesStoredRecoveryState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		action    string
		reason    string
		cause     string
		wantKind  primitives.Kind
		wantClass string
	}{
		{
			name:      "transient wait",
			action:    "defer",
			reason:    "github_rest_budget_wait",
			cause:     "transient GitHub REST budget waiting for capacity",
			wantKind:  primitives.KindWarn,
			wantClass: "border-warn/45",
		},
		{
			name:      "human attention",
			action:    "hold",
			reason:    "no_commits_to_deliver",
			cause:     "no_commits_to_deliver: current branch has no commits ahead",
			wantKind:  primitives.KindErr,
			wantClass: "border-err/45",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			card := projectKanbanCard{
				IssueID:               "issue-1915",
				Identifier:            "digitaldrywood/detent#1915",
				IssueNumber:           "#1915",
				ProjectID:             "detent",
				Title:                 tt.name,
				Stage:                 "Blocked",
				BlockedSource:         telemetry.BlockedSourceProjectStatus,
				BlockedReason:         tt.cause,
				BlockedRecoveryAction: tt.action,
				BlockedRecoveryReason: tt.reason,
				Comments: []telemetry.IssueComment{{
					Body: "activity feed supplied a different blocked cause",
				}},
			}
			view := boardCardViewFromCard(DashboardData{}, projectKanbanLane{Title: "Blocked"}, card, false, "fleet", "detent")

			if view.ExtraKind != tt.wantKind || !strings.Contains(view.ExtraText, tt.cause) {
				t.Fatalf("blocked cause = %q/%q, want %q containing %q", view.ExtraKind, view.ExtraText, tt.wantKind, tt.cause)
			}
			if view.Activity != "" {
				t.Fatalf("Activity = %q, want stored blocked cause to be authoritative", view.Activity)
			}
			html := renderBoardComponent(t, boardCardView2(view))
			if !strings.Contains(html, tt.wantClass) || strings.Contains(html, "activity feed supplied") {
				t.Fatalf("blocked card did not use stored cause treatment:\n%s", html)
			}
		})
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

func TestBoardCardSurfacesWorkpadBlockerResolution(t *testing.T) {
	t.Parallel()

	openRef := telemetry.BlockedRef{
		Identifier:   "gopherguides/corp#492",
		State:        "In Progress",
		TrackerState: "open",
		Source:       "workpad",
	}
	closedRef := telemetry.BlockedRef{
		Identifier:   "gopherguides/corp#491",
		State:        "Done",
		TrackerState: "closed",
		Source:       "workpad",
	}
	duplicateClosedRef := closedRef
	duplicateClosedRef.Source = "native"
	tests := []struct {
		name     string
		refs     []telemetry.BlockedRef
		wantRefs []string
	}{
		{name: "open ref renders live", refs: []telemetry.BlockedRef{openRef}, wantRefs: []string{"gopherguides/corp#492 (live)"}},
		{name: "closed ref renders resolved", refs: []telemetry.BlockedRef{closedRef}, wantRefs: []string{"gopherguides/corp#491 (resolved)"}},
		{name: "closed workpad ref duplicated by native relation renders resolved", refs: []telemetry.BlockedRef{duplicateClosedRef}, wantRefs: []string{"gopherguides/corp#491 (resolved)"}},
		{name: "mixed refs retain operative hold", refs: []telemetry.BlockedRef{closedRef, openRef}, wantRefs: []string{"gopherguides/corp#491 (resolved)", "gopherguides/corp#492 (live)"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := boardTestData()
			blockedAt := data.Snapshot.GeneratedAt.Add(-time.Minute)
			data.Snapshot.Blocked = []telemetry.Blocked{{
				Issue: telemetry.Issue{
					ID:         "issue-workpad-resolution",
					Identifier: "gopherguides/corp#486",
					Number:     486,
					ProjectID:  "detent",
					Title:      "Surface resolved Workpad refs",
					State:      "Blocked",
					BlockedBy:  tt.refs,
				},
				Error:               "the workflow emits Test (Go 1.26.5)",
				Source:              telemetry.BlockedSourceProjectStatus,
				RecoveryAction:      "hold",
				RecoveryReason:      "human_action",
				RecoveryRemedy:      "approve the remaining deployment",
				NeedsHumanAttention: true,
				BlockedAt:           &blockedAt,
			}}

			html := renderBoardComponent(t, BoardSnapshot(data))
			for _, want := range append(tt.wantRefs, "needs review - the workflow emits Test (Go 1.26.5) — approve the remaining deployment") {
				if !strings.Contains(html, want) {
					t.Fatalf("board card missing %q:\n%s", want, html)
				}
			}
			if !strings.Contains(html, "data-board-card-blockers") {
				t.Fatalf("board card missing blocker-ref annotation:\n%s", html)
			}
		})
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
	attrs := sheetOpenAttrs(card.Project, card.Identity, card.Scope, true)
	if got, _ := attrs["hx-get"].(string); !strings.Contains(got, "project=detent") {
		t.Fatalf("card sheet link should carry the fallback project, got %q", got)
	}
	if got, _ := attrs["hx-trigger"].(string); got != "click[!event.target.closest('button,a,input,select,textarea')]" {
		t.Fatalf("card sheet trigger = %q, want nested controls excluded", got)
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
	data.Kanban.ShowBlockedAlerts = true
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

func TestBoardMoveDisabledLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason string
		want   string
	}{
		{reason: "This project's tracker does not support moving cards.", want: "Read-only"},
		{reason: "This project board is read-only.", want: "Read-only"},
		{reason: "Project is initializing; moves are disabled until tracker data is current.", want: "Initializing"},
		{reason: "Tracker snapshot is not ready; moves are disabled until data is current.", want: "Stale"},
		{reason: "No linked issue is available for this card.", want: "No issue"},
		{reason: "No allowed transition is configured from Done.", want: "No move"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			if got := boardMoveDisabledLabel(tt.reason); got != tt.want {
				t.Fatalf("boardMoveDisabledLabel(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

func TestBoardCardSurfacesMoveRefusalInEveryDensity(t *testing.T) {
	t.Parallel()

	reason := "Tracker candidate data for this card is stale; moves are disabled until it refreshes."
	html := renderBoardComponent(t, boardCardView2(boardCardView{
		DomID:             "card-1842",
		Title:             "Scoped stale card",
		State:             "Todo",
		DragDrop:          true,
		MoveDisabledText:  reason,
		MoveDisabledLabel: "Stale",
	}))
	tests := []struct {
		name   string
		marker string
	}{
		{name: "cozy", marker: `data-board-card-content="cozy"`},
		{name: "compact", marker: `data-board-card-content="compact"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			start := strings.Index(html, tt.marker)
			if start < 0 {
				t.Fatalf("card missing %s density content:\n%s", tt.name, html)
			}
			section := html[start:]
			if next := strings.Index(section[len(tt.marker):], `data-board-card-content=`); next >= 0 {
				section = section[:len(tt.marker)+next]
			}
			for _, want := range []string{`data-kanban-move-disabled-label`, `title="` + reason + `"`, `>Stale</span>`} {
				if !strings.Contains(section, want) {
					t.Fatalf("%s density missing %q:\n%s", tt.name, want, section)
				}
			}
		})
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

func boardCardDensitySection(t *testing.T, html string, density string) string {
	t.Helper()
	marker := `data-board-card-content="` + density + `"`
	start := strings.Index(html, marker)
	if start < 0 {
		t.Fatalf("card missing %s density content:\n%s", density, html)
	}
	section := html[start:]
	if next := strings.Index(section[len(marker):], `data-board-card-content=`); next >= 0 {
		section = section[:len(marker)+next]
	}
	return section
}

func TestBoardScopeSelectLinksProjectKanbanOnce(t *testing.T) {
	data := boardTestData()
	data.Projects = []ProjectSmallMultiple{{ID: "detent", Name: "Detent"}}

	html := renderBoardComponent(t, boardScopeSelect(data))
	if !strings.Contains(html, `href="/projects/detent/kanban"`) {
		t.Fatalf("scope select missing kanban project href:\n%s", html)
	}
	if strings.Contains(html, "/kanban/kanban") {
		t.Fatalf("scope select appended kanban twice:\n%s", html)
	}
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
		`id="board-lane-position"`,
		"data-board-lane-position",
		"snap-x snap-mandatory gap-5",
		"w-full flex-none snap-start",
		`id="card-gopherguides-gopher-ai-185"`,
		`id="fig-running"`,
		`id="fig-blocked"`,
		"data-board-lane-picker",
		"data-board-lane-visibility",
		"data-board-lane-reset-all",
		"data-board-lane-reset",
		`value="auto"`,
		`value="show"`,
		`value="hide"`,
		"w-[calc(100vw-2.5rem)]",
		"min-h-11",
		"h-11",
		"size-11",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("board snapshot missing %q", want)
		}
	}
	if strings.Contains(html, "data-board-lane-toggle") || strings.Contains(html, `type="checkbox"`) {
		t.Fatalf("board lane picker must not render legacy checkbox toggles:\n%s", html)
	}
	if strings.Contains(html, `id="board-exceptions"`) {
		t.Fatalf("default board snapshot should not render elevated blocked alerts:\n%s", html)
	}
	if strings.Contains(html, "#0B0D10") || strings.Contains(html, "#14171C") {
		t.Fatalf("board snapshot must not contain raw hex colors")
	}
}

func TestBoardSnapshotRendersPendingEnrichmentPlaceholder(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.PendingEnrichment = true
	html := renderBoardComponent(t, BoardSnapshot(data))
	if !strings.Contains(html, "-- today") {
		t.Fatalf("pending enrichment missing spend placeholder:\n%s", html)
	}
}

func TestBoardSnapshotRendersLastKnownState(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.LastKnown = true
	html := renderBoardComponent(t, BoardSnapshot(data))

	if strings.Contains(html, `id="board-alerts"`) {
		t.Fatalf("last-known state without a refresh fault rendered a banner:\n%s", html)
	}
	for _, want := range []string{
		`id="board-freshness"`,
		"Last-known data",
		`id="board-lanes"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("last-known board snapshot missing %q:\n%s", want, html)
		}
	}
}

func TestBoardAlertsExcludeRoutineStartup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     int
	}{
		{
			name: "cached tracker startup",
			snapshot: telemetry.Snapshot{
				LastKnown: true,
				Tracker:   telemetry.SnapshotSection{Source: telemetry.SnapshotSourceCached, Complete: true},
				Runtime:   telemetry.SnapshotSection{Source: telemetry.SnapshotSourceUnknown},
				Refresh:   telemetry.Refresh{Status: telemetry.RefreshStatusInitializing},
			},
		},
		{
			name: "composite startup",
			snapshot: telemetry.Snapshot{
				Tracker: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed, Complete: true},
				Runtime: telemetry.SnapshotSection{Source: telemetry.SnapshotSourceMixed},
				Refresh: telemetry.Refresh{Status: telemetry.RefreshStatusInitializing},
			},
		},
		{name: "legacy last-known without a fault is quiet", snapshot: telemetry.Snapshot{LastKnown: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := len(boardAlerts(tt.snapshot)); got != tt.want {
				t.Fatalf("len(boardAlerts()) = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBoardAlertsBuildSeverityAndGroupedTrackerRows(t *testing.T) {
	t.Parallel()

	snapshot := boardAlertsHeavyTestSnapshot()
	alerts := boardAlerts(snapshot)
	wantKinds := []boardAlertKind{
		boardAlertKindLastKnown,
		boardAlertKindFailureBreaker,
		boardAlertKindBackendCapacity,
	}
	if len(alerts) != len(wantKinds) {
		t.Fatalf("boardAlerts() count = %d, want %d: %#v", len(alerts), len(wantKinds), alerts)
	}
	for index, want := range wantKinds {
		if alerts[index].Kind != want {
			t.Fatalf("boardAlerts()[%d].Kind = %q, want %q", index, alerts[index].Kind, want)
		}
	}
	if alerts[0].Tone != primitives.KindErr {
		t.Fatalf("highest-severity tone = %q, want err", alerts[0].Tone)
	}

	currentSnapshot := snapshot
	currentSnapshot.LastKnown = false
	if alerts := boardAlerts(currentSnapshot); len(alerts) != 2 {
		t.Fatalf("boardAlerts() = %#v, want only failure and backend faults", alerts)
	}
	rows := diagnosticsConditionRows(currentSnapshot)
	if len(rows) == 0 {
		t.Fatal("diagnostics omitted refresh failure details")
	}
}

func TestBoardAlertsRenderOnlyFaultConditions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     int
	}{
		{
			name: "finished fleet",
			snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Counts:      telemetry.Counts{Completed: 14},
				StalenessWarnings: []telemetry.StalenessWarning{
					{ID: "lane-age", Class: observability.ClassDiagnostic, Kind: "lane_aging", AgeSeconds: 86_400},
					{ID: "review-age", Class: observability.ClassReviewQueue, Kind: "lane_aging", WaitingOnHuman: true, AgeSeconds: 604_800},
					{ID: "decline", Class: observability.ClassDiagnostic, Kind: "repeated_decision", Detail: "authorization selector declined one issue"},
				},
				DispatchStalls: []telemetry.DispatchStatus{{Stalled: true, WaitReasonCode: "github_rest_capacity_paused", Class: observability.ClassDiagnostic}},
			},
		},
		{
			name: "provider out of tokens",
			snapshot: telemetry.Snapshot{GeneratedAt: now, BackendOutages: []telemetry.BackendOutage{{
				ProjectID: "detent", BackendID: "codex", Kind: "usage_limit_exceeded", Reason: "provider usage limit reached",
			}}},
			want: 1,
		},
		{
			name: "total selector exclusion",
			snapshot: telemetry.Snapshot{GeneratedAt: now, DispatchStalls: []telemetry.DispatchStatus{{
				ProjectID: "detent", CandidateCount: 8, SkippedCount: 8, Stalled: true, WaitReasonCode: "authorization_selector_declined", WaitReason: "authorization selector excludes every candidate",
			}}},
			want: 1,
		},
		{
			name: "per issue selector decline",
			snapshot: telemetry.Snapshot{GeneratedAt: now, StalenessWarnings: []telemetry.StalenessWarning{{
				ID: "issue-decline", Class: observability.ClassDiagnostic, Kind: "repeated_decision", Detail: "authorization selector declined one issue",
			}}},
		},
		{
			name: "review wait at any age",
			snapshot: telemetry.Snapshot{GeneratedAt: now, StalenessWarnings: []telemetry.StalenessWarning{{
				ID: "review-wait", Class: observability.ClassReviewQueue, Kind: "lane_aging", WaitingOnHuman: true, AgeSeconds: 31_536_000,
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alerts := boardAlerts(tt.snapshot)
			if len(alerts) != tt.want {
				t.Fatalf("boardAlerts() = %#v, want %d faults", alerts, tt.want)
			}
			for _, alert := range alerts {
				if alert.Tone != primitives.KindErr {
					t.Fatalf("alert tone = %q, want error", alert.Tone)
				}
			}
			data := boardTestData()
			data.Snapshot = tt.snapshot
			html := renderBoardComponent(t, BoardSnapshot(data))
			if tt.want == 0 && strings.Contains(html, `id="board-alerts"`) {
				t.Fatalf("board rendered a banner for non-fault conditions:\n%s", html)
			}
			if tt.want > 0 && (!strings.Contains(html, `data-board-alert-count="1"`) || !strings.Contains(html, ">1 fault<")) {
				t.Fatalf("board fault count missing:\n%s", html)
			}
		})
	}
}

func TestBoardAlertCountIncludesOnlyActionableConditions(t *testing.T) {
	t.Parallel()
	data := boardTestData()
	eligibleCandidates := 0
	data.Snapshot.FailureBreakers = []telemetry.FailureBreaker{{
		ProjectID:              "detent",
		Class:                  "runner_error",
		EligibleCandidateCount: &eligibleCandidates,
		Items:                  []telemetry.FailureBreakerItem{{IssueID: "parked", Parked: true}},
	}}
	data.Snapshot.StalenessWarnings = []telemetry.StalenessWarning{{
		ID:         "warning-actionable",
		Class:      observability.ClassFault,
		ProjectID:  "detent",
		Kind:       "repeated_decision",
		Identifier: "digitaldrywood/detent#1926",
		Detail:     "dispatch decision is repeating",
	}}

	alerts := boardAlerts(data.Snapshot)
	if len(alerts) != 1 || alerts[0].Kind != boardAlertKindStaleness {
		t.Fatalf("boardAlerts() = %#v, want only actionable staleness warning", alerts)
	}
	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		`data-board-alert-count="1"`,
		`hx-post="/api/v1/projects/detent/staleness-warnings/warning-actionable/acknowledge"`,
		`hx-post="/api/v1/projects/detent/staleness-warnings/acknowledge"`,
		`hx-swap="outerHTML"`,
		`data-staleness-warning-id="warning-actionable"`,
		">Dismiss<",
		">Dismiss all staleness warnings (1)<",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("board alert missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "Project failure breaker") {
		t.Fatalf("board counted deliberate parked breaker:\n%s", html)
	}
}

func TestBoardAlertsSurfaceCIUnavailableEvidence(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		GeneratedAt: time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC),
		CIUnavailable: []telemetry.CICondition{{
			ProjectID:           "detent",
			UnstartedCheckCount: 6,
			PullRequestCount:    2,
			OldestQueueSeconds:  47 * 60,
			ParkedAttemptCount:  2,
		}},
	}

	alerts := boardAlerts(snapshot)
	if len(alerts) != 1 || alerts[0].Kind != boardAlertKindCIUnavailable || alerts[0].Tone != primitives.KindErr {
		t.Fatalf("boardAlerts() = %#v, want one CI-unavailable error", alerts)
	}
	for _, want := range []string{"6 queued checks", "2 PRs", "47m", "2 attempts parked"} {
		combined := alerts[0].DetailRows[0].Summary + " " + alerts[0].DetailRows[0].Detail
		if !strings.Contains(combined, want) {
			t.Fatalf("CI alert detail = %q, want %q", combined, want)
		}
	}
}

func TestBoardAlertsNameTrackerAvailabilityCondition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	alerts := boardAlerts(telemetry.Snapshot{
		GeneratedAt: now,
		TrackerUnavailable: []telemetry.TrackerCondition{{
			ProjectID:         "detent",
			Connector:         "github",
			ConnectorInstance: "detent:github",
			Operation:         "observed_status",
			ErrorClass:        "server",
			NextProbeAt:       now.Add(30 * time.Second),
		}},
	})
	if len(alerts) != 1 || alerts[0].Kind != boardAlertKindTrackerUnavailable || alerts[0].Tone != primitives.KindErr {
		t.Fatalf("boardAlerts() = %#v, want tracker-unavailable error", alerts)
	}
	combined := alerts[0].TerseSummary + " " + alerts[0].DetailSummary + " " + alerts[0].DetailRows[0].Summary + " " + alerts[0].DetailRows[0].Detail
	for _, want := range []string{"Tracker unavailable", "github tracker", "tracker_unavailable", "observed_status", "server", "next canary in 30s"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("tracker alert = %q, want containing %q", combined, want)
		}
	}
	if alerts[0].Action == nil || !strings.Contains(alerts[0].Action.Path, "project_id=detent") {
		t.Fatalf("tracker alert action = %#v, want project-scoped clear", alerts[0].Action)
	}
}

func TestBoardAlertsCorroborateTrackerIncident(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      telemetry.ProviderStatus
		wantSummary string
		wantLink    string
	}{
		{
			name: "incident names components and links shortlink",
			status: telemetry.ProviderStatus{
				Provider: "GitHub",
				State:    telemetry.ProviderStatusCorroborated,
				Incident: &telemetry.ProviderIncident{
					Name:       "GitHub service disruption",
					URL:        "https://stspg.io/example",
					Status:     "mitigating",
					Components: []string{"API Requests", "Issues"},
				},
			},
			wantSummary: "GitHub incident affecting API Requests and Issues — mitigating",
			wantLink:    "https://stspg.io/example",
		},
		{
			name:        "missing incident is explicit corroboration information",
			status:      telemetry.ProviderStatus{Provider: "GitHub", State: telemetry.ProviderStatusNoMatch},
			wantSummary: "no matching provider incident",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			alerts := boardAlerts(telemetry.Snapshot{TrackerUnavailable: []telemetry.TrackerCondition{{
				ProjectID: "detent", Connector: "github", Operation: "observed_status", ErrorClass: "server", ProviderStatus: &tt.status,
			}}})
			if len(alerts) != 1 || len(alerts[0].DetailRows) != 1 {
				t.Fatalf("boardAlerts() = %#v, want one tracker alert row", alerts)
			}
			row := alerts[0].DetailRows[0]
			combined := row.Label + " " + row.Summary + " " + row.Detail
			if !strings.Contains(combined, tt.wantSummary) || row.Link != tt.wantLink {
				t.Fatalf("tracker provider row = %#v, want summary %q and link %q", row, tt.wantSummary, tt.wantLink)
			}
		})
	}
}

func TestBoardAlertsNameForgeWriteConditionDistinctly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	alerts := boardAlerts(telemetry.Snapshot{
		GeneratedAt: now,
		ForgeUnavailable: []telemetry.ForgeCondition{{
			ProjectID:   "detent",
			Host:        "github.com",
			Operation:   "git push",
			ErrorClass:  "transport",
			NextProbeAt: now.Add(30 * time.Second),
		}},
	})
	if len(alerts) != 1 || alerts[0].Kind != boardAlertKindForgeUnavailable || alerts[0].Tone != primitives.KindErr {
		t.Fatalf("boardAlerts() = %#v, want forge-unavailable error", alerts)
	}
	combined := alerts[0].TerseSummary + " " + alerts[0].DetailSummary + " " + alerts[0].DetailRows[0].Summary + " " + alerts[0].DetailRows[0].Detail
	for _, want := range []string{"Forge writes unavailable", "github.com forge", "forge_unavailable", "git push", "transport", "next write canary in 30s"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("forge alert = %q, want containing %q", combined, want)
		}
	}
	if strings.Contains(combined, "Tracker unavailable") || strings.Contains(combined, "tracker reads") {
		t.Fatalf("forge alert = %q, want no tracker-read label", combined)
	}
	if alerts[0].Action == nil || !strings.Contains(alerts[0].Action.Path, "host=github.com") || !strings.Contains(alerts[0].Action.Path, "project_id=detent") {
		t.Fatalf("forge alert action = %#v, want host- and project-scoped clear", alerts[0].Action)
	}
}

func TestBoardAlertsSurfaceDispatchStallAsNeedsAttention(t *testing.T) {
	t.Parallel()

	const authorizationDetail = "issue does not match authorization selector: missing required label `detent`"
	secondsSinceLastSelected := int64(14_400)
	alerts := boardAlerts(telemetry.Snapshot{DispatchStalls: []telemetry.DispatchStatus{{
		ProjectID:                "detent",
		CandidateCount:           8,
		WaitReason:               authorizationDetail,
		WaitReasonCode:           "authorization_selector_declined",
		SecondsSinceLastSelected: &secondsSinceLastSelected,
		StallDurationSeconds:     10_800,
		Stalled:                  true,
		NeedsHumanAttention:      true,
	}}})
	if len(alerts) != 1 || alerts[0].Kind != boardAlertKindDispatchStall || alerts[0].Tone != primitives.KindErr {
		t.Fatalf("boardAlerts() = %#v, want one dispatch-stall error", alerts)
	}
	combined := alerts[0].TerseSummary + " " + alerts[0].DetailSummary + " " + alerts[0].DetailRows[0].Summary + " " + alerts[0].DetailRows[0].Detail
	for _, want := range []string{"Dispatch stalled", "human attention", "8 candidates", "3h", authorizationDetail} {
		if !strings.Contains(combined, want) {
			t.Fatalf("dispatch alert = %q, want containing %q", combined, want)
		}
	}
}

func TestBoardAlertsRenderOneLineOverlayContract(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot = boardAlertsHeavyTestSnapshot()
	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		`id="board-alerts"`,
		`class="min-w-0 max-w-full self-center overflow-hidden`,
		`data-board-alert-count="3"`,
		`role="alert"`,
		`aria-live="polite"`,
		`type="button"`,
		`aria-expanded="false"`,
		`aria-controls="board-alerts-overlay"`,
		`class="hidden min-w-0 flex-1 truncate text-text md:inline"`,
		`href="/health/ui"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("unified board alerts missing %q:\n%s", want, html)
		}
	}
	for _, oldID := range []string{
		`id="board-last-known"`,
		`id="board-stale-data"`,
		`id="project-failure-breaker"`,
		`id="dispatch-recovery-status"`,
		`id="backend-capacity-outage"`,
		`id="update-pending"`,
	} {
		if strings.Contains(html, oldID) {
			t.Fatalf("unified board alerts rendered legacy section %q:\n%s", oldID, html)
		}
	}

	empty := renderBoardComponent(t, boardAlertsBar(nil))
	if empty != "" {
		t.Fatalf("empty board alerts rendered markup: %q", empty)
	}

	page := renderBoardComponent(t, BoardPage(data))
	if strings.Count(page, `id="board-alerts-overlay"`) != 1 {
		t.Fatalf("body-level alert overlay hosts = %d, want 1", strings.Count(page, `id="board-alerts-overlay"`))
	}
	for _, want := range []string{"max-h-[40vh]", "overflow-y-auto", "syncOverlayAfterMorph", `document.addEventListener("htmx:afterSettle"`, "window.htmx.process(host)"} {
		if !strings.Contains(page, want) {
			t.Fatalf("board page missing morph-safe alert behavior %q", want)
		}
	}
}

func TestBoardAdmissionProposalIndicator(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	proposal := func(index int) telemetry.AdmissionProposal {
		return telemetry.AdmissionProposal{
			ID:              "proposal-" + strconv.Itoa(index),
			ProjectID:       "detent",
			IssueID:         "issue-" + strconv.Itoa(index),
			IssueIdentifier: "digitaldrywood/detent#" + strconv.Itoa(1600+index),
			IssueURL:        "https://github.com/digitaldrywood/detent/issues/" + strconv.Itoa(1600+index),
			Confidence:      0.88,
			CreatedAt:       now.Add(-time.Duration(index) * time.Hour),
			ExpiresAt:       now.Add(time.Duration(24-index) * time.Hour),
		}
	}
	tests := []struct {
		name      string
		proposals []telemetry.AdmissionProposal
		wantRows  int
	}{
		{name: "zero"},
		{name: "one", proposals: []telemetry.AdmissionProposal{proposal(1)}, wantRows: 1},
		{name: "several", proposals: []telemetry.AdmissionProposal{proposal(1), proposal(2), proposal(3)}, wantRows: 3},
		{
			name:      "several beyond banner cap",
			proposals: []telemetry.AdmissionProposal{proposal(1), proposal(2), proposal(3), proposal(4), proposal(5), proposal(6), proposal(7)},
			wantRows:  7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := boardTestData()
			data.Snapshot.GeneratedAt = now
			data.Snapshot.AdmissionProposals = tt.proposals
			html := renderBoardComponent(t, BoardSnapshot(data))
			if strings.Contains(html, `data-board-alert="admission-proposal"`) || strings.Contains(html, `id="board-alerts"`) {
				t.Fatalf("review queue rendered a board fault banner:\n%s", html)
			}
			rows := diagnosticsConditionRows(data.Snapshot)
			if len(rows) != tt.wantRows {
				t.Fatalf("diagnostics admission rows = %#v, want %d", rows, tt.wantRows)
			}
			for _, row := range rows {
				if row.Class != observability.ClassReviewQueue {
					t.Fatalf("admission class = %q, want review queue", row.Class)
				}
			}
		})
	}
}

func boardAlertsHeavyTestSnapshot() telemetry.Snapshot {
	now := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-4*time.Minute - 28*time.Second)
	projectIDs := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	sources := make([]telemetry.RefreshSource, 0, len(projectIDs)*3)
	for _, projectID := range projectIDs {
		for _, name := range []telemetry.RefreshSourceName{
			telemetry.RefreshSourceCandidates,
			telemetry.RefreshSourceDrift,
			telemetry.RefreshSourceStatuses,
		} {
			source := telemetry.RefreshSource{
				ProjectID:     projectID,
				Name:          name,
				LastSuccessAt: &lastSuccess,
				FailureStreak: 3,
			}
			if name == telemetry.RefreshSourceStatuses {
				source.LastError = "GitHub status query returned status 503"
			}
			sources = append(sources, source)
		}
	}
	return telemetry.Snapshot{
		LastKnown:   true,
		GeneratedAt: now,
		Refresh: telemetry.Refresh{
			StaleAfterSeconds: 120,
			FailureThreshold:  3,
			Status:            telemetry.RefreshStatusDegraded,
			Sources:           sources,
		},
		FailureBreakers: []telemetry.FailureBreaker{{ProjectID: "alpha", Class: "backend_startup_timeout", Count: 4, WindowSeconds: 3600}},
		DispatchRecoveries: []telemetry.DispatchRecovery{{
			ProjectID: "bravo",
			Kind:      "github_rest",
			Status:    "waiting",
		}},
		BackendOutages: []telemetry.BackendOutage{{
			ProjectID: "charlie",
			BackendID: "claude-opus-5",
			Provider:  "anthropic",
			Reason:    "provider usage limit reached",
		}},
		Update: telemetry.Update{
			State:            "pending_idle",
			AvailableVersion: "0.50.0",
		},
	}
}

func TestBoardSnapshotRendersMorphSafePriorityBadge(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.ProjectID = "detent"
	data.Kanban.ProjectID = "detent"
	data.Snapshot.BoardIssues[0].ProjectID = "detent"
	data.Snapshot.BoardIssues[0].Priority = boardPriorityPointer(1)
	data.Snapshot.BoardIssues[0].PriorityName = "P0"
	html := renderBoardComponent(t, BoardSnapshot(data))

	for _, want := range []string{
		`id="card-gopherguides-gopher-ai-176-priority"`,
		`data-board-priority`,
		`data-board-priority-top="true"`,
		`data-help-scope="dispatch-priority"`,
		`data-help-title="Dispatch priority"`,
		`data-help-description="Tracker priority P0 maps to dispatch rank 1."`,
		`border-l-4`,
		`P0`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("priority board snapshot missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `id="help-tooltip"`) {
		t.Fatalf("priority tooltip host rendered inside morph target:\n%s", html)
	}

	page := renderBoardComponent(t, BoardPage(data))
	if strings.Count(page, `id="help-tooltip"`) != 1 {
		t.Fatalf("board page tooltip hosts = %d, want 1", strings.Count(page, `id="help-tooltip"`))
	}
	if !strings.Contains(page, `document.addEventListener("htmx:afterSettle"`) {
		t.Fatalf("board page missing help tooltip settle reassertion")
	}
	if !strings.Contains(page, `hx-swap="morph:innerHTML"`) {
		t.Fatalf("board page missing morph snapshot swap")
	}
	for _, want := range []string{
		"visibleBoardLanes",
		"lanePositionIndex",
		"updateLanePosition",
		"scheduleLanePosition",
		`event.target.matches("[data-board-lanes]")`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("board page missing mobile lane behavior %q", want)
		}
	}
}

func TestBoardSnapshotRendersUnblockerPriorityDetail(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.BoardIssues[0].UnblockerCount = 2
	html := renderBoardComponent(t, BoardSnapshot(data))

	for _, want := range []string{
		`data-board-priority`,
		`data-help-scope="dispatch-priority"`,
		`data-help-description="Unblocks 2 issues."`,
		`unblocker`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("unblocker board snapshot missing %q:\n%s", want, html)
		}
	}
}

func TestBoardSnapshotWithoutPriorityConfigKeepsCardRendering(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	view := boardViewFromDashboard(data)
	card := view.Lanes[1].Cards[0]
	if card.PriorityBadge != "" || card.PriorityDetail != "" || card.PriorityTop {
		t.Fatalf("unconfigured priority = %#v, want empty", card)
	}
	if got := boardCardClass(card); got != "flex flex-none flex-col gap-1.5 rounded-card border border-line bg-elev p-3" {
		t.Fatalf("boardCardClass() = %q, want unchanged default", got)
	}
	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, `data-board-priority`) {
		t.Fatalf("unconfigured board rendered priority metadata:\n%s", html)
	}
}

func TestBoardSnapshotRendersBackendCapacityBanner(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	lastProbeAt := data.Snapshot.GeneratedAt.Add(-time.Minute)
	data.Snapshot.BackendOutages = []telemetry.BackendOutage{{
		ProjectID:       "detent",
		BackendID:       "codex",
		BackendKind:     "codex",
		Provider:        "openai",
		Reason:          "provider usage limit reached",
		LastProbeAt:     &lastProbeAt,
		LastProbeResult: "capacity_exhausted",
		LastProbeDetail: "provider usage limit reached",
	}}
	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		`id="board-alerts"`,
		`id="board-alert-backend-capacity"`,
		`data-board-alert="backend-capacity-outage"`,
		"Backend codex at usage limit — 1 project",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("capacity banner missing %q:\n%s", want, html)
		}
	}
	for _, want := range []string{"Dispatch is paused for openai", `href="/health/ui"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("capacity overlay missing detail %q:\n%s", want, html)
		}
	}
	for _, unwanted := range []string{"Last probe", "Clear outage"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("capacity summary contains detail %q:\n%s", unwanted, html)
		}
	}
}

func TestBoardSnapshotHidesScheduledPacingStates(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{
		{ProjectID: "detent", Kind: "github_rest", Status: "waiting", ResumeAt: data.Snapshot.GeneratedAt.Add(14 * time.Minute)},
		{ProjectID: "docs", Kind: "pull_request_hydration", Reason: "rest_budget_reserved", Status: "waiting", ResumeAt: data.Snapshot.GeneratedAt.Add(14 * time.Minute)},
	}
	data.Snapshot.BackendOutages = []telemetry.BackendOutage{{
		Kind:     "github_rest_rate_limit",
		Reason:   "GitHub REST remaining is at or below dispatch floor",
		ResumeAt: data.Snapshot.GeneratedAt.Add(14 * time.Minute),
	}}

	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, unwanted := range []string{`id="dispatch-recovery-status"`, `id="backend-capacity-outage"`, "rest_budget_reserved"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("scheduled pacing state rendered Board banner %q:\n%s", unwanted, html)
		}
	}
	if !strings.Contains(html, `id="board-figures"`) || !strings.Contains(html, `id="board-lanes"`) {
		t.Fatalf("Board content missing after pacing banners were hidden:\n%s", html)
	}
}

func TestBoardSnapshotEscalatesStuckAutomaticRecovery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prepare  func(*DashboardData)
		wantID   string
		wantText string
	}{
		{
			name: "dispatch retry overdue",
			prepare: func(data *DashboardData) {
				data.Snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{{
					ProjectID: "detent",
					Kind:      "github_rest",
					Status:    "waiting",
					ResumeAt:  data.Snapshot.GeneratedAt.Add(-2 * time.Minute),
				}}
			},
			wantID:   `id="board-alert-dispatch-recovery"`,
			wantText: "Dispatch retry overdue for GitHub REST capacity",
		},
		{
			name: "capacity probe overdue",
			prepare: func(data *DashboardData) {
				nextProbeAt := data.Snapshot.GeneratedAt.Add(-2 * time.Minute)
				data.Snapshot.BackendOutages = []telemetry.BackendOutage{{
					ProjectID:   "detent",
					BackendID:   "codex",
					ResumeAt:    data.Snapshot.GeneratedAt.Add(-10 * time.Minute),
					NextProbeAt: &nextProbeAt,
				}}
			},
			wantID:   `id="board-alert-backend-capacity"`,
			wantText: "Backend codex recovery overdue",
		},
		{
			name: "repeated capacity probes",
			prepare: func(data *DashboardData) {
				nextProbeAt := data.Snapshot.GeneratedAt.Add(10 * time.Minute)
				data.Snapshot.BackendOutages = []telemetry.BackendOutage{{
					ProjectID:       "detent",
					BackendID:       "codex",
					ResumeAt:        data.Snapshot.GeneratedAt.Add(10 * time.Minute),
					NextProbeAt:     &nextProbeAt,
					LastProbeResult: "capacity_exhausted",
					ProbeAttempts:   3,
				}}
			},
			wantID:   `id="board-alert-backend-capacity"`,
			wantText: "Backend codex recovery failed repeatedly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := boardTestData()
			tt.prepare(&data)
			html := renderBoardComponent(t, BoardSnapshot(data))
			for _, want := range []string{tt.wantID, tt.wantText} {
				if !strings.Contains(html, want) {
					t.Fatalf("stuck recovery banner missing %q:\n%s", want, html)
				}
			}
		})
	}
}

func TestBoardSnapshotRendersProjectFailureBreakerBanner(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	eligibleCandidates := 1
	data.Snapshot.FailureBreakers = []telemetry.FailureBreaker{{
		ProjectID:              "detent",
		Class:                  "runner_error:b6c174a86dfb",
		Count:                  5,
		AttemptCount:           5,
		DistinctItemCount:      1,
		Cause:                  "provider usage limit reached",
		RepresentativeError:    "You've hit your limit. Try again at 9:39 PM",
		BackendID:              "claude-code",
		BackendKind:            "claude_code",
		Provider:               "anthropic",
		EligibleCandidateCount: &eligibleCandidates,
		Items: []telemetry.FailureBreakerItem{{
			IssueID:      "video-1",
			Identifier:   "2026-07-10-detent-not-vibe-coding-short",
			IssueURL:     "https://example.test/items/video-1",
			Title:        "Author beat visuals",
			CurrentState: "Blocked",
			AttemptCount: 5,
			Parked:       true,
		}},
		WindowSeconds: 3600,
		ResumeAt:      data.Snapshot.GeneratedAt.Add(time.Hour),
	}}
	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		`id="board-alerts"`,
		`id="board-alert-failure-breaker"`,
		`data-board-alert="project-failure-breaker"`,
		"Project failure breaker active — 1 project",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("failure breaker banner missing %q:\n%s", want, html)
		}
	}
	for _, want := range []string{
		"5 failed attempts across 1 item",
		"The project may dispatch one eligible candidate",
		"Author beat visuals — 2026-07-10-detent-not-vibe-coding-short",
		`href="https://example.test/items/video-1"`,
		"Provider usage limit reached",
		"Backend: claude-code · kind claude_code · provider anthropic",
		"Diagnostic class: runner_error:b6c174a86dfb",
		"will not retry merely because the project canary is eligible",
		localTimeToken(data.Snapshot.GeneratedAt.Add(time.Hour), LocalDateTimeZone),
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("failure breaker overlay missing detail %q:\n%s", want, html)
		}
	}
	for _, unwanted := range []string{"Run canary now"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("failure breaker summary contains detail %q:\n%s", unwanted, html)
		}
	}
}

func TestBoardSnapshotAttributesDeliverableFailureBreakerCommand(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.FailureBreakers = []telemetry.FailureBreaker{{
		ProjectID:     "detent.build",
		Class:         "deliverable_command_failure:codex_apps/github.create_pull_request",
		Count:         5,
		WindowSeconds: 3600,
		ResumeAt:      data.Snapshot.GeneratedAt.Add(time.Hour),
	}}
	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		"Deliverable command failed: codex apps/github.create pull request",
		"deliverable_command_failure:codex_apps/github.create_pull_request",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("failure breaker attribution missing %q:\n%s", want, html)
		}
	}
}

func TestBoardSnapshotHidesRampActiveDispatchRecovery(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{{
		ProjectID:     "detent",
		Kind:          "pull_request_hydration",
		Reason:        "rest_budget_reserved",
		Status:        "ramping",
		StartedAt:     data.Snapshot.GeneratedAt.Add(-time.Minute),
		ResumeAt:      data.Snapshot.GeneratedAt,
		Limit:         1,
		MaxConcurrent: 6,
		Admitted:      1,
	}}
	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, `id="dispatch-recovery-status"`) || strings.Contains(html, "Dispatch recovery ramp active") {
		t.Fatalf("board rendered ramp-active recovery:\n%s", html)
	}
}

func TestBoardSnapshotMovesWaitingDispatchRecoveriesToDiagnostics(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{
		{ProjectID: "detent", Kind: "github_rest", Status: "waiting"},
		{ProjectID: "docs", Kind: "github_rest", Status: "waiting"},
		{ProjectID: "billing", Kind: "backend_capacity", Status: "ramping", Limit: 1, MaxConcurrent: 6},
	}
	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, `id="board-alert-dispatch-recovery"`) {
		t.Fatalf("waiting recovery rendered a fault banner:\n%s", html)
	}
	rows := diagnosticsConditionRows(data.Snapshot)
	if len(rows) != 3 {
		t.Fatalf("diagnostics recovery rows = %#v, want all three recovery states", rows)
	}
	for _, row := range rows {
		if row.Class != observability.ClassDiagnostic {
			t.Fatalf("diagnostics recovery class = %q, want diagnostic", row.Class)
		}
	}
}

func TestDispatchRecoveryLabelsDistinguishWaitReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want string
	}{
		{kind: "github_rest", want: "GitHub REST capacity"},
		{kind: "pull_request_hydration", want: "pull-request hydration"},
		{kind: "backend_capacity", want: "backend capacity"},
		{kind: "project_failure_breaker", want: "project failure breaker"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			recovery := telemetry.DispatchRecovery{Kind: tt.kind, Status: "waiting"}
			if title := dispatchRecoveryTitle(recovery); !strings.Contains(title, tt.want) {
				t.Fatalf("dispatchRecoveryTitle() = %q, want %q", title, tt.want)
			}
		})
	}
}

func TestHealthSnapshotHidesCanaryActionWhileCanaryRuns(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.FailureBreakers = []telemetry.FailureBreaker{{
		ProjectID:     "detent",
		Class:         "backend_startup_timeout",
		Count:         5,
		WindowSeconds: 3600,
		ResumeAt:      data.Snapshot.GeneratedAt,
		CanaryIssueID: "issue-canary",
	}}
	html := renderBoardComponent(t, HealthSnapshotV2(data))
	if strings.Contains(html, "Run canary now") || !strings.Contains(html, "The project is dispatching one canary candidate") {
		t.Fatalf("active canary controls are incorrect:\n%s", html)
	}
}

func TestDiagnosticsSnapshotExplainsReadyCanaryWithoutEligibleCandidates(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	eligibleCandidates := 0
	data.Snapshot.FailureBreakers = []telemetry.FailureBreaker{{
		ProjectID:              "detent",
		Class:                  "runner_error:b6c174a86dfb",
		Count:                  5,
		AttemptCount:           5,
		DistinctItemCount:      1,
		EligibleCandidateCount: &eligibleCandidates,
		Items:                  []telemetry.FailureBreakerItem{{IssueID: "issue-1", CurrentState: "Blocked", Parked: true, AttemptCount: 5}},
		WindowSeconds:          3600,
		ResumeAt:               data.Snapshot.GeneratedAt,
	}}

	html := renderBoardComponent(t, ProjectDiagnosticsSnapshot(data))
	for _, want := range []string{
		"Project canary dispatch is ready, but no eligible candidate is currently available",
		"affected Blocked item will not retry merely because the project canary is eligible",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("zero-candidate breaker evidence missing %q:\n%s", want, html)
		}
	}
}

func TestBoardSnapshotRendersTransientOverloadCounterWithoutOutage(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.OverloadRetriesLastHour = 3
	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, `id="backend-overload-retries"`) || strings.Contains(html, "3 overload retries last hour") {
		t.Fatalf("board rendered informational overload retry notice:\n%s", html)
	}
	if strings.Contains(html, `id="backend-capacity-outage"`) {
		t.Fatalf("transient overload counter rendered an outage banner:\n%s", html)
	}
}

func TestBoardSnapshotMovesGitHubRESTCapacityToDiagnostics(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.BackendOutages = []telemetry.BackendOutage{{
		BackendID:   "github-rest",
		BackendKind: "tracker",
		Provider:    "github",
		Kind:        "github_rest_rate_limit",
		Reason:      "GitHub REST remaining 0 is at or below dispatch floor 1000",
	}}
	html := renderBoardComponent(t, BoardSnapshot(data))
	if strings.Contains(html, `id="board-alerts"`) {
		t.Fatalf("GitHub REST throttling rendered a fault banner:\n%s", html)
	}
	rows := diagnosticsConditionRows(data.Snapshot)
	if len(rows) != 1 || rows[0].Class != observability.ClassDiagnostic || !strings.Contains(rows[0].Detail, "remaining 0") {
		t.Fatalf("diagnostics GitHub REST row = %#v", rows)
	}
}

func TestDiagnosticsSnapshotKeepsRecoveryAndOverloadDetail(t *testing.T) {
	t.Parallel()

	data := boardTestData()
	data.Snapshot.DispatchRecoveries = []telemetry.DispatchRecovery{
		{ProjectID: "detent", Kind: "github_rest", Reason: "quota exhausted", Status: "waiting", ResumeAt: data.Snapshot.GeneratedAt.Add(time.Minute)},
		{ProjectID: "docs", Kind: "backend_capacity", Status: "ramping", Limit: 1, MaxConcurrent: 6},
	}
	data.Snapshot.OverloadRetriesLastHour = 3
	html := renderBoardComponent(t, ProjectDiagnosticsSnapshot(data))
	for _, want := range []string{
		"GitHub REST capacity is delaying dispatch: quota exhausted.",
		"backend capacity cleared; recovery is admitting up to 1 of 6 configured workers",
		"3 overload retries last hour",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Health recovery detail missing %q:\n%s", want, html)
		}
	}
}

func TestBoardSnapshotSurfacesHiddenPopulatedLanes(t *testing.T) {
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC),
			BoardIssues: []telemetry.Issue{
				{ID: "todo-1", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", Title: "Visible todo", State: "Todo"},
				{ID: "cancelled-1", Identifier: "digitaldrywood/detent#2", ProjectID: "detent", Title: "Hidden cancelled", State: "Cancelled"},
			},
		},
		Kanban: KanbanData{
			States:         []string{"Todo", "Cancelled"},
			TerminalStates: []string{"Cancelled"},
		},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		`data-board-hidden-card-count`,
		"1 hidden",
		"1 hidden card in Cancelled.",
		`data-board-lane-hidden-populated="true"`,
		"Auto hidden - 1 hidden card",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("hidden populated lane signal missing %q:\n%s", want, html)
		}
	}
}

func TestBoardSnapshotRendersActiveLaneAgeFooter(t *testing.T) {
	data := boardTestData()
	html := renderBoardComponent(t, BoardSnapshot(data))

	activeCard := boardCardSection(t, html, "refactor(tmux-start): extract inline bash to scripts")
	for _, want := range []string{
		`data-board-card-age-footer`,
		`title="In Progress since`,
		"In lane",
		"3m",
	} {
		if !strings.Contains(activeCard, want) {
			t.Fatalf("active card missing age footer marker %q:\n%s", want, activeCard)
		}
	}

	for _, title := range []string{
		"docs: agent session lifecycle diagram",
		"fix(worktree): stale lock cleanup on crash",
	} {
		card := boardCardSection(t, html, title)
		if strings.Contains(card, `data-board-card-age-footer`) {
			t.Fatalf("%q card should not render an age footer:\n%s", title, card)
		}
	}

	data.Snapshot.GeneratedAt = data.Snapshot.GeneratedAt.Add(time.Hour)
	updated := renderBoardComponent(t, BoardSnapshot(data))
	updatedCard := boardCardSection(t, updated, "refactor(tmux-start): extract inline bash to scripts")
	if !strings.Contains(updatedCard, "1h") {
		t.Fatalf("active card should update age from refreshed snapshot time:\n%s", updatedCard)
	}
}

func TestBoardSnapshotRendersLocalSQLiteProductionLaneAgeFooter(t *testing.T) {
	now := time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)
	stageUpdatedAt := time.Date(2026, 7, 9, 14, 17, 33, 936728130, time.UTC)
	data := DashboardData{
		Kanban: KanbanData{
			States: []string{"Backlog", "Todo", "Production", "Review", "Ready for Pickup"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			BoardIssues: []telemetry.Issue{
				{
					ID:             "wi-011cd179bc7ecf36b7197e4b",
					Identifier:     "wi-011cd179bc7ecf36b7197e4b",
					ProjectID:      "video-production",
					Title:          "Render local artifact",
					State:          "Production",
					StageUpdatedAt: &stageUpdatedAt,
				},
			},
		},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	card := boardCardSection(t, html, "Render local artifact")
	for _, want := range []string{
		`data-board-card-age-footer`,
		`title="Production since`,
		"In lane",
		"42m",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("local_sqlite Production card missing age footer marker %q:\n%s", want, card)
		}
	}
}

func TestBoardSnapshotRendersOptInBlockedAlert(t *testing.T) {
	data := boardTestData()
	data.Kanban.ShowBlockedAlerts = true
	html := renderBoardComponent(t, BoardSnapshot(data))

	for _, want := range []string{
		`id="board-exceptions"`,
		"Needs review",
		"needs operator approval",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("opt-in board snapshot missing %q:\n%s", want, html)
		}
	}
}

func TestBoardSnapshotRendersKanbanDragAttributes(t *testing.T) {
	now := time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC)
	data := DashboardData{
		ProjectID:   "detent",
		ProjectName: "Detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "I_kw1",
					Identifier: "digitaldrywood/detent#1",
					ProjectID:  "detent",
					Title:      "Movable drag card",
					State:      "Todo",
				},
				{
					Identifier: "digitaldrywood/detent#2",
					ProjectID:  "detent",
					Title:      "PR only drag card",
					State:      "Human Review",
					PullRequest: &telemetry.PullRequest{
						Number: 43,
						URL:    "https://github.test/digitaldrywood/detent/pull/43",
					},
				},
			},
		},
		Kanban: KanbanData{
			Mode:         "integration",
			States:       []string{"Todo", "In Progress", "Blocked", "Human Review", "Rework", "Done"},
			CanMoveCards: true,
			AllowedTransitions: map[string][]string{
				"Todo":         {"In Progress", "Blocked"},
				"Human Review": {"Rework"},
			},
		},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	for _, want := range []string{
		`data-kanban-drop-state="In Progress"`,
		`data-kanban-drop-key="inprogress"`,
		`id="board-feedback"`,
		`hidden`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("board snapshot missing %q:\n%s", want, html)
		}
	}

	card := boardCardSection(t, html, "Movable drag card")
	for _, want := range []string{
		`data-kanban-card`,
		`data-kanban-current-state="Todo"`,
		`data-kanban-issue-id="I_kw1"`,
		`data-kanban-action="move"`,
		`data-kanban-allowed-targets="inprogress blocked"`,
		`hx-post="/api/v1/kanban/move"`,
		`hx-target="#snapshot"`,
		`hx-swap="morph:innerHTML"`,
		`name="kanban_drag" value="true"`,
		`name="kanban_board" value="project"`,
		`name="project_id" value="detent"`,
		`name="issue_id" value="I_kw1"`,
		`name="current_state" value="Todo"`,
		`name="target_state" value="" data-kanban-drag-target-state`,
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("movable card missing %q:\n%s", want, card)
		}
	}

	prOnly := boardCardSection(t, html, "PR only drag card")
	for _, want := range []string{
		`data-kanban-card`,
		`data-kanban-current-state="Human Review"`,
		`data-kanban-move-disabled="true"`,
		`data-kanban-move-disabled-reason="No linked issue is available for this PR-only card."`,
		`data-kanban-move-disabled-label`,
		`No issue`,
	} {
		if !strings.Contains(prOnly, want) {
			t.Fatalf("PR-only card missing %q:\n%s", want, prOnly)
		}
	}
	for _, forbidden := range []string{
		`data-kanban-action="move"`,
		`data-kanban-drag-move-form`,
		`data-kanban-issue-id=`,
	} {
		if strings.Contains(prOnly, forbidden) {
			t.Fatalf("PR-only card rendered forbidden %q:\n%s", forbidden, prOnly)
		}
	}
}

func TestBoardSnapshotOmitsKanbanDragAttributesWhenDisabled(t *testing.T) {
	now := time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC)
	readySnapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "I_kw1",
				Identifier: "digitaldrywood/detent#1",
				ProjectID:  "detent",
				Title:      "Non-draggable card",
				State:      "Todo",
			},
		},
	}
	tests := []struct {
		name       string
		data       DashboardData
		wantReason string
		wantLabel  string
		wantDrops  bool
	}{
		{
			name: "read-only project board",
			data: DashboardData{
				ProjectID: "detent",
				Snapshot:  readySnapshot,
				Kanban:    KanbanData{Mode: "read_only", States: []string{"Todo", "In Progress"}},
			},
			wantReason: "This project board is read-only.",
			wantLabel:  "Read-only",
		},
		{
			name: "fleet board without per-project kanban data",
			data: DashboardData{
				Snapshot: readySnapshot,
				Kanban:   KanbanData{Mode: "integration", States: []string{"Todo", "In Progress"}},
			},
			wantReason: "This project board is read-only.",
			wantLabel:  "Read-only",
		},
		{
			name: "degraded project board with prior data",
			data: DashboardData{
				ProjectID: "detent",
				Snapshot: telemetry.Snapshot{
					GeneratedAt: now,
					Project:     readySnapshot.Project,
					BoardIssues: readySnapshot.BoardIssues,
					Refresh:     telemetry.Refresh{Status: telemetry.RefreshStatusDegraded, LastError: "tracker unavailable"},
				},
				Kanban: KanbanData{Mode: "integration", States: []string{"Todo", "In Progress"}, CanMoveCards: true},
			},
			wantReason: "Tracker refresh is degraded; moves are disabled until a fresh snapshot is ready.",
			wantLabel:  "Stale",
			wantDrops:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := renderBoardComponent(t, BoardSnapshot(tt.data))
			card := boardCardSection(t, html, "Non-draggable card")
			for _, want := range []string{
				`data-kanban-card`,
				`data-kanban-current-state="Todo"`,
				`data-kanban-move-disabled="true"`,
				`data-kanban-move-disabled-reason="` + tt.wantReason + `"`,
				`title="` + tt.wantReason + `"`,
				`data-kanban-move-disabled-label`,
				tt.wantLabel,
			} {
				if !strings.Contains(card, want) {
					t.Fatalf("disabled card missing %q:\n%s", want, card)
				}
			}
			if tt.wantDrops {
				if !strings.Contains(html, `data-kanban-drop-state`) {
					t.Fatalf("disabled card board missing drop targets for other fresh cards:\n%s", html)
				}
			} else if strings.Contains(html, `data-kanban-drop-state`) {
				t.Fatalf("disabled board rendered drop targets:\n%s", html)
			}
			for _, forbidden := range []string{`data-kanban-drag-move-form`, `data-kanban-action="move"`} {
				if strings.Contains(html, forbidden) {
					t.Fatalf("disabled board rendered %q:\n%s", forbidden, html)
				}
			}
		})
	}
}

func TestBoardSnapshotFleetBoardRendersDragAttributes(t *testing.T) {
	now := time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC)
	data := DashboardData{
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Projects: []telemetry.ProjectSnapshot{
				{Project: telemetry.Project{ID: "detent", DisplayName: "Detent"}},
				{Project: telemetry.Project{ID: "docs-site", DisplayName: "Docs Site"}},
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "I_kw1",
					Identifier: "digitaldrywood/detent#1",
					ProjectID:  "detent",
					Title:      "Fleet draggable card",
					State:      "Todo",
				},
				{
					ID:         "I_docs1",
					Identifier: "digitaldrywood/docs-site#1",
					ProjectID:  "docs-site",
					Title:      "Fleet read-only project card",
					State:      "Todo",
				},
			},
		},
		Kanban: KanbanData{
			Mode:   "read_only",
			States: []string{"Todo", "In Progress"},
			Projects: map[string]KanbanProjectData{
				"detent": {
					Mode:         "integration",
					ProjectID:    "detent",
					States:       []string{"Todo", "In Progress"},
					CanMoveCards: true,
					AllowedTransitions: map[string][]string{
						"Todo": {"In Progress"},
					},
				},
				"docs-site": {
					Mode:      "read_only",
					ProjectID: "docs-site",
					States:    []string{"Todo", "In Progress"},
				},
			},
		},
	}
	html := renderBoardComponent(t, BoardSnapshot(data))

	draggable := boardCardSection(t, html, "Fleet draggable card")
	for _, want := range []string{
		`data-kanban-action="move"`,
		`data-kanban-allowed-targets="inprogress"`,
		`data-kanban-drag-move-form`,
		`name="kanban_board" value="fleet"`,
		`name="project_id" value="detent"`,
	} {
		if !strings.Contains(draggable, want) {
			t.Fatalf("fleet card missing %q:\n%s", want, draggable)
		}
	}

	readOnly := boardCardSection(t, html, "Fleet read-only project card")
	for _, want := range []string{
		`data-kanban-move-disabled="true"`,
		`data-kanban-move-disabled-reason="This project board is read-only."`,
	} {
		if !strings.Contains(readOnly, want) {
			t.Fatalf("read-only project card missing %q:\n%s", want, readOnly)
		}
	}
	if strings.Contains(readOnly, `data-kanban-action="move"`) {
		t.Fatalf("read-only project card must not be draggable:\n%s", readOnly)
	}

	if !strings.Contains(html, `data-kanban-drop-state="Todo"`) || !strings.Contains(html, `data-kanban-drop-key="todo"`) {
		t.Fatalf("fleet lanes missing drop targets:\n%s", html)
	}
}

func TestBoardSnapshotExplainsCardLevelMoveDisabledReasons(t *testing.T) {
	now := time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC)
	data := DashboardData{
		ProjectID: "detent",
		Snapshot: telemetry.Snapshot{
			GeneratedAt: now,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			BoardIssues: []telemetry.Issue{
				{
					Identifier: "digitaldrywood/detent#2",
					ProjectID:  "detent",
					Title:      "No linked issue card",
					State:      "Human Review",
					PullRequest: &telemetry.PullRequest{
						Number: 43,
						URL:    "https://github.test/digitaldrywood/detent/pull/43",
					},
				},
				{
					ID:         "I_done",
					Identifier: "digitaldrywood/detent#3",
					ProjectID:  "detent",
					Title:      "No transition card",
					State:      "Done",
				},
			},
		},
		Kanban: KanbanData{
			Mode:               "integration",
			States:             []string{"Human Review", "Rework", "Done"},
			CanMoveCards:       true,
			AllowedTransitions: map[string][]string{"Human Review": {"Rework"}},
		},
	}

	html := renderBoardComponent(t, BoardSnapshot(data))
	tests := []struct {
		title      string
		wantReason string
		wantLabel  string
	}{
		{
			title:      "No linked issue card",
			wantReason: "No linked issue is available for this PR-only card.",
			wantLabel:  "No issue",
		},
		{
			title:      "No transition card",
			wantReason: "No allowed transition is configured from Done.",
			wantLabel:  "No move",
		},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			card := boardCardSection(t, html, tt.title)
			for _, want := range []string{
				`data-kanban-move-disabled="true"`,
				`data-kanban-move-disabled-reason="` + tt.wantReason + `"`,
				`title="` + tt.wantReason + `"`,
				`data-kanban-move-disabled-label`,
				tt.wantLabel,
			} {
				if !strings.Contains(card, want) {
					t.Fatalf("disabled card missing %q:\n%s", want, card)
				}
			}
			for _, forbidden := range []string{
				`data-kanban-action="move"`,
				`data-kanban-drag-move-form`,
			} {
				if strings.Contains(card, forbidden) {
					t.Fatalf("disabled card rendered forbidden %q:\n%s", forbidden, card)
				}
			}
		})
	}
}

func TestBoardKanbanDragScriptSubmitsAllowedDrop(t *testing.T) {
	html := renderBoardComponent(t, boardKanbanDragScript())
	for _, want := range []string{
		`window.__detentBoardKanbanDragHandlersRegistered`,
		`lane.dataset.laneHidden = "false";`,
		`lane.dataset.kanbanDropAllowed = allowed ? "true" : "false";`,
		`document.addEventListener("pointerdown"`,
		`pressedCard.dataset.kanbanMoveDisabled === "true"`,
		`pressedElement.closest("a, button, input, select, textarea, summary, label, [data-help-trigger]")`,
		`feedback(pressedCard.dataset.kanbanMoveDisabledReason || "This card cannot be moved.", "error");`,
		`document.addEventListener("pointermove"`,
		`document.addEventListener("pointerup"`,
		`feedback("Move blocked by transition policy.", "error");`,
		`targetState.value = lane.dataset.kanbanDropState || "";`,
		`form.requestSubmit();`,
		`document.body.addEventListener("htmx:responseError"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("drag script missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `target.innerHTML`) || strings.Contains(html, `snapshot.innerHTML`) {
		t.Fatalf("drag script must not swap innerHTML inside snapshot:\n%s", html)
	}
}

func TestProjectBoardPageIncludesBoardScripts(t *testing.T) {
	data := boardTestData()
	data.ProjectID = "detent"
	data.ProjectName = "Detent"
	data.Kanban.Mode = "integration"
	html := renderBoardComponent(t, ProjectBoardPage(data))
	for _, want := range []string{
		`syncOverlayAfterMorph`,
		`window.__detentBoardKanbanDragHandlersRegistered`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("project board page must include %q:\n%s", want, html)
		}
	}
}

func boardCardSection(t *testing.T, html string, title string) string {
	t.Helper()
	titleIndex := strings.Index(html, title)
	if titleIndex < 0 {
		t.Fatalf("missing card title %q:\n%s", title, html)
	}
	start := strings.LastIndex(html[:titleIndex], "<article")
	if start < 0 {
		t.Fatalf("missing article for %q:\n%s", title, html)
	}
	end := strings.Index(html[titleIndex:], "</article>")
	if end < 0 {
		t.Fatalf("missing article close for %q:\n%s", title, html)
	}
	return html[start : titleIndex+end+len("</article>")]
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
	done := boardCardViewFromCard(DashboardData{}, projectKanbanLane{Title: "Done"}, card, true, "fleet", "detent")
	if !done.Done || !done.Terminal {
		t.Fatalf("Done lane card should be Done and Terminal: %+v", done)
	}
	cancelled := boardCardViewFromCard(DashboardData{}, projectKanbanLane{Title: "Cancelled"}, card, true, "fleet", "detent")
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
