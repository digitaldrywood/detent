package templates

import (
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// boardView is the redesigned home page: exception strip, figure row, and
// fixed-height lanes that scroll independently. Everything is pre-formatted
// so the template stays declarative.
type boardView struct {
	Key        string
	Exceptions []primitives.Exception
	Figures    []primitives.Figure
	TPS        string
	Spend      string
	Lanes      []boardLaneView
	Visible    int
	Total      int
}

type boardLaneView struct {
	DomID          string
	LaneID         string
	Title          string
	Count          string
	Live           bool
	DefaultVisible bool
	EmptyMessage   string
	Cards          []boardCardView
}

// boardCardView keeps cards uniform: an 11px mono meta row, a two-line
// title, and AT MOST one extra signal (chip, status line, or none).
type boardCardView struct {
	DomID     string
	Number    string
	Project   string
	Scope     string
	Running   bool
	Done      bool
	Terminal  bool
	MetaRight string
	Title     string
	ExtraKind primitives.Kind
	ExtraText string
	ExtraChip bool
}

func boardViewFromDashboard(data DashboardData) boardView {
	board := projectKanbanBoardView(data)
	view := boardView{
		Key:        boardVisibilityKey(data),
		Exceptions: boardExceptions(data, true),
		Figures:    boardFigures(data.Snapshot),
		TPS:        throughputRate(data.Snapshot),
		Spend:      formatUSD(data.Snapshot.Budget.CurrentSpendUSD) + " today",
	}
	fallbackProjectID := boardFallbackProjectID(data)
	globalTerminalStates := projectKanbanTerminalStateSet(data.Kanban.TerminalStates)
	// An entirely empty board shows its non-terminal lanes so the operator
	// sees the empty states rather than a blank strip; once any lane has
	// cards, empty lanes collapse to reduce clutter.
	boardHasCards := false
	for _, lane := range board.AllLanes {
		if len(lane.Cards) > 0 {
			boardHasCards = true
			break
		}
	}
	for _, lane := range board.AllLanes {
		// The fleet board mixes projects, so a lane's terminal-ness is
		// resolved per card. A populated lane counts as terminal only when it
		// is terminal for every card's own project; an empty lane falls back
		// to the global set.
		laneTerminal := boardLaneTerminal(data, lane, globalTerminalStates)
		laneView := boardLaneView{
			DomID:  "lane-" + lane.ID,
			LaneID: lane.ID,
			Title:  lane.Title,
			Count:  formatCount(len(lane.Cards)),
			Live:   strings.EqualFold(lane.Title, "In Progress") && len(lane.Cards) > 0,
			// Populated lanes show by default, except terminal graveyards
			// (Cancelled, Closed, …). Done stays visible so finished work
			// reads at a glance; everything is reachable via the picker.
			DefaultVisible: boardLaneDefaultVisible(lane, laneTerminal, boardHasCards),
			EmptyMessage:   "No issues in " + lane.Title,
		}
		for _, card := range lane.Cards {
			cardTerminal := projectKanbanTerminalState(lane.Title, projectKanbanTerminalStateSetForProject(data, card.ProjectID))
			laneView.Cards = append(laneView.Cards, boardCardViewFromCard(lane, card, cardTerminal, projectKanbanBoardScope(data), fallbackProjectID))
		}
		view.Lanes = append(view.Lanes, laneView)
		view.Total++
		if laneView.DefaultVisible {
			view.Visible++
		}
	}
	return view
}

// boardFallbackProjectID resolves the project a card belongs to when its
// Issue.ProjectID is empty (legacy single-project snapshots): the scoped
// dashboard project, then the snapshot project, then the sole configured
// project. Without it the sheet request omits project scope and eligible
// cards lose their Move action on the home board.
func boardFallbackProjectID(data DashboardData) string {
	if id := strings.TrimSpace(data.ProjectID); id != "" {
		return id
	}
	if id := strings.TrimSpace(data.Snapshot.Project.ID); id != "" {
		return id
	}
	if len(data.Projects) == 1 {
		return strings.TrimSpace(data.Projects[0].ID)
	}
	return ""
}

// boardLaneTerminal reports whether a lane should be treated as a terminal
// graveyard. A populated lane is terminal only when it is terminal for every
// card's own project; an empty lane uses the global terminal set.
func boardLaneTerminal(data DashboardData, lane projectKanbanLane, globalTerminalStates map[string]struct{}) bool {
	if len(lane.Cards) == 0 {
		return projectKanbanTerminalState(lane.Title, globalTerminalStates)
	}
	for _, card := range lane.Cards {
		if !projectKanbanTerminalState(lane.Title, projectKanbanTerminalStateSetForProject(data, card.ProjectID)) {
			return false
		}
	}
	return true
}

func boardLaneDefaultVisible(lane projectKanbanLane, terminal bool, boardHasCards bool) bool {
	if !boardHasCards {
		// Empty board: keep non-terminal lanes visible so their empty
		// states are legible instead of a blank strip.
		return !terminal
	}
	return len(lane.Cards) > 0 && (!terminal || strings.EqualFold(lane.Title, "Done"))
}

func boardVisibilityKey(data DashboardData) string {
	if id := strings.TrimSpace(data.ProjectID); id != "" {
		return "project." + id
	}
	return "fleet"
}

func boardFigures(snapshot telemetry.Snapshot) []primitives.Figure {
	blocked := blockedCount(snapshot)
	return []primitives.Figure{
		{ID: "fig-running", Value: formatCount(runningCount(snapshot)), Label: "running"},
		{ID: "fig-queued", Value: formatCount(queueCount(snapshot)), Label: "queued"},
		{ID: "fig-blocked", Value: formatCount(blocked), Label: "blocked", Err: blocked > 0},
		{ID: "fig-completed", Value: formatCount(completedCount(snapshot)), Label: "completed"},
	}
}

// boardExceptions builds the exception strip. boardActions is true only
// when the calling page renders a board into #snapshot (Board, project
// Kanban), so the Review sheet may offer inline Move/Remove; the Fleet and
// Overview pages pass false so their Review sheets stay read-only.
func boardExceptions(data DashboardData, boardActions bool) []primitives.Exception {
	snapshot := data.Snapshot
	now := pipelineNow(snapshot)
	fallbackProjectID := strings.TrimSpace(data.ProjectID)
	exceptions := make([]primitives.Exception, 0, len(snapshot.Blocked))
	for _, row := range snapshot.Blocked {
		// Legacy single-project snapshots can leave Issue.ProjectID empty; fall
		// back to the scoped dashboard project so the Review sheet keeps its
		// project-scoped Move/Remove links (matching board card views).
		projectID := strings.TrimSpace(row.ProjectID)
		if projectID == "" {
			projectID = fallbackProjectID
		}
		exception := primitives.Exception{
			ID:    "exception-" + boardCardSlug(projectID, projectKanbanIssueNumber(row.Issue)),
			Kind:  primitives.KindErr,
			Title: "Session blocked",
			Repo:  projectID,
			Ref:   projectKanbanIssueNumber(row.Issue),
			Rest:  boardExceptionDetail(row, now),
		}
		exception.ActionLabel = "Review"
		exception.ActionAttrs = sheetOpenAttrs(projectID, projectKanbanIssueNumber(row.Issue), projectKanbanBoardScope(data), boardActions)
		exceptions = append(exceptions, exception)
	}
	return exceptions
}

func boardExceptionDetail(row telemetry.Blocked, now time.Time) string {
	detail := strings.TrimSpace(row.Error)
	if detail == "" {
		detail = "needs operator attention"
	}
	if row.BlockedAt != nil {
		detail += " · waiting " + prPipelineAge(*row.BlockedAt, now)
	}
	return detail
}

func boardCardViewFromCard(lane projectKanbanLane, card projectKanbanCard, terminal bool, scope string, fallbackProjectID string) boardCardView {
	// Legacy single-project snapshots can include issues without setting
	// Issue.ProjectID, so fall back to the scoped dashboard project so the
	// card slug and the sheet's project-scoped Move/Remove links resolve.
	projectID := strings.TrimSpace(card.ProjectID)
	if projectID == "" {
		projectID = fallbackProjectID
	}
	view := boardCardView{
		DomID:   "card-" + boardCardSlug(projectID, card.IssueNumber),
		Number:  card.IssueNumber,
		Project: projectID,
		Scope:   scope,
		Running: strings.EqualFold(lane.Title, "In Progress"),
		// Done drives the green ✓; other terminal states (Cancelled, Closed)
		// are terminal but not done, so they suppress meta without claiming
		// success.
		Done:     strings.EqualFold(lane.Title, "Done"),
		Terminal: terminal,
		Title:    card.Title,
	}
	switch {
	case view.Done || view.Terminal:
		view.MetaRight = ""
	case card.PRNumber > 0 && !view.Running:
		view.MetaRight = "PR #" + strconv.Itoa(card.PRNumber)
	case boardLaneShowsAge(lane.Title):
		view.MetaRight = boardCompactAge(card.TimeInStage)
	}
	view.ExtraKind, view.ExtraText, view.ExtraChip = boardCardExtra(card, view)
	return view
}

// boardCardExtra picks the single allowed extra signal, most urgent first:
// an exception chip, then a status line. Cards never stack signals.
func boardCardExtra(card projectKanbanCard, view boardCardView) (primitives.Kind, string, bool) {
	if view.Done || view.Terminal {
		return primitives.KindNeutral, "", false
	}
	if label := strings.TrimSpace(card.AttentionLabel); label != "" {
		return primitives.KindErr, "blocked — " + label, true
	}
	if len(card.Blockers) > 0 {
		return primitives.KindErr, "blocked — " + card.Blockers[0], true
	}
	if reason := strings.TrimSpace(card.ConflictReason); reason != "" {
		return primitives.KindWarn, reason, true
	}
	if status := strings.TrimSpace(card.CIStatus); status != "" {
		return primitives.KindInfo, status, false
	}
	if view.Running {
		if stage := strings.TrimSpace(card.WaitDetail); stage != "" {
			return primitives.KindInfo, stage, false
		}
		return primitives.KindOK, "agent working", false
	}
	return primitives.KindNeutral, "", false
}

// boardFirstRun is true only when nothing is configured at all: no
// projects registered and no usable board data. Running mode always has
// at least one project, so this is effectively the unconfigured guard.
func boardFirstRun(data DashboardData) bool {
	return len(data.Projects) == 0 && !projectKanbanBoardLoaded(data)
}

func BoardShellDataFromDashboard(data DashboardData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = "board"
	shell.IncludeDashboardCharts = false
	return shell
}

func boardBoolAttr(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func boardLaneCountLabel(view boardView) string {
	return formatCount(view.Visible) + "/" + formatCount(view.Total)
}

func boardScopeLabel(data DashboardData) string {
	if name := strings.TrimSpace(data.ProjectName); name != "" {
		return name
	}
	if id := strings.TrimSpace(data.ProjectID); id != "" {
		return id
	}
	return "All projects"
}

func boardFeedbackClass(kind string) string {
	if kind == "error" {
		return "text-err"
	}
	return "text-sec"
}

func boardFeedbackGlyph(kind string) string {
	if kind == "error" {
		return "⬣"
	}
	return "✓"
}

func boardCardClass(card boardCardView) string {
	if card.Done {
		return "flex flex-none flex-col gap-1.5 rounded-card border border-line bg-surface p-3 opacity-75"
	}
	if card.ExtraChip && card.ExtraKind == primitives.KindErr {
		return "flex flex-none flex-col gap-1.5 rounded-card border border-err/45 bg-elev p-3"
	}
	return "flex flex-none flex-col gap-1.5 rounded-card border border-line bg-elev p-3"
}

func boardCardNumberClass(card boardCardView) string {
	if card.Done {
		return "text-sec"
	}
	return "text-text"
}

func boardCardTitleClass(card boardCardView) string {
	if card.Done {
		return "line-clamp-2 text-sm text-sec"
	}
	return "line-clamp-2 text-sm text-text"
}

func boardExtraTextClass(kind primitives.Kind) string {
	switch kind {
	case primitives.KindOK:
		return "text-ok"
	case primitives.KindWarn:
		return "text-warn"
	case primitives.KindErr:
		return "text-err"
	case primitives.KindInfo:
		return "text-info"
	}
	return "text-sec"
}

// boardCompactAge reduces "3m 39s" to "3m": board cards are narrow, and
// the leading unit is all an at-a-glance read needs. Numbers must never
// wrap or clip, so the value is shortened rather than truncated.
func boardCompactAge(age string) string {
	age = strings.TrimSpace(age)
	if age == "" || age == "n/a" {
		return ""
	}
	if head, _, ok := strings.Cut(age, " "); ok {
		return head
	}
	return age
}

// boardLaneShowsAge keeps intake lanes quiet: time-in-stage only matters
// once work is moving.
func boardLaneShowsAge(title string) bool {
	switch strings.ToLower(strings.TrimSpace(title)) {
	case "backlog", "todo":
		return false
	}
	return true
}

func boardCardSlug(projectID string, number string) string {
	slug := strings.TrimSpace(projectID) + "-" + strings.TrimPrefix(strings.TrimSpace(number), "#")
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, slug)
	return strings.Trim(slug, "-")
}
