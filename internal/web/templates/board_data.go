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
	DropState      string
	DropKey        string
	Count          string
	Live           bool
	DefaultVisible bool
	EmptyMessage   string
	Cards          []boardCardView
}

// boardCardView keeps cards uniform: an 11px mono meta row, a two-line
// title, and AT MOST one extra signal (chip, status line, or none).
type boardCardView struct {
	DomID             string
	Identity          string
	IssueID           string
	Number            string
	Project           string
	MoveProject       string
	Scope             string
	CurrentState      string
	PRNumber          string
	DragDrop          bool
	CanDrag           bool
	AllowedTargets    string
	MoveDisabledText  string
	MoveDisabledLabel string
	Running           bool
	Done              bool
	Terminal          bool
	MetaRight         string
	AgeFooter         string
	AgeFooterTitle    string
	Title             string
	ExtraKind         primitives.Kind
	ExtraText         string
	ExtraChip         bool
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
	dragDrop := projectKanbanDragDropEnabled(data)
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
		if dragDrop {
			laneView.DropState = lane.Title
			laneView.DropKey = projectKanbanStateKey(lane.Title)
		}
		for _, card := range lane.Cards {
			cardTerminal := projectKanbanTerminalState(lane.Title, projectKanbanTerminalStateSetForProject(data, card.ProjectID))
			laneView.Cards = append(laneView.Cards, boardCardViewFromCard(data, lane, card, cardTerminal, projectKanbanBoardScope(data), fallbackProjectID))
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
	if boardActions && !data.Kanban.ShowBlockedAlerts {
		return nil
	}

	rows := make([]telemetry.Blocked, 0, len(data.Snapshot.Blocked))
	for _, row := range data.Snapshot.Blocked {
		if boardBlockedWaiting(row.Source, row.RecoveryReason, row.Error) {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	return []primitives.Exception{boardBlockedExceptionSummary(data, rows, boardActions)}
}

func boardBlockedExceptionSummary(data DashboardData, rows []telemetry.Blocked, boardActions bool) primitives.Exception {
	row := rows[0]
	projectID := strings.TrimSpace(row.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(data.ProjectID)
	}
	identity := boardCardIdentityToken(row.Identifier, row.ID, projectKanbanIssueNumber(row.Issue))
	exception := primitives.Exception{
		ID:    "exception-" + boardCardScopedSlug(projectID, identity),
		Kind:  primitives.KindErr,
		Title: "Needs review",
		Repo:  projectID,
		Ref:   projectKanbanIssueNumber(row.Issue),
		Rest:  boardExceptionDetail(row, pipelineNow(data.Snapshot)),
	}
	if len(rows) == 1 {
		exception.ActionLabel = "Review"
		exception.ActionAttrs = sheetOpenAttrs(projectID, identity, projectKanbanBoardScope(data), boardActions)
		return exception
	}
	exception.ID = "exception-blocked-review"
	exception.Title = formatCount(len(rows)) + " blocked items need review"
	exception.Rest = strings.TrimSpace(exception.Rest + " · " + boardMoreBlockedLabel(len(rows)-1))
	return exception
}

func boardMoreBlockedLabel(count int) string {
	if count == 1 {
		return "plus 1 more"
	}
	return "plus " + formatCount(count) + " more"
}

func boardExceptionDetail(row telemetry.Blocked, now time.Time) string {
	detail := boardBlockedDetail(row.Source, row.RecoveryReason, row.Error)
	if detail == "" && boardBlockedWaiting(row.Source, row.RecoveryReason, row.Error) {
		if boardBlockedDependencyWaiting(row.Source, row.RecoveryReason, row.Error, row.BlockedBy) {
			detail = "dependency not ready"
		} else {
			detail = "paused by project status"
		}
	}
	if detail == "" {
		detail = "needs operator attention"
	}
	if row.BlockedAt != nil {
		detail += " · waiting " + prPipelineAge(*row.BlockedAt, now)
	}
	return detail
}

func boardCardViewFromCard(data DashboardData, lane projectKanbanLane, card projectKanbanCard, terminal bool, scope string, fallbackProjectID string) boardCardView {
	// Legacy single-project snapshots can include issues without setting
	// Issue.ProjectID, so fall back to the scoped dashboard project so the
	// card slug and the sheet's project-scoped Move/Remove links resolve.
	projectID := strings.TrimSpace(card.ProjectID)
	if projectID == "" {
		projectID = fallbackProjectID
	}
	moveProjectID := strings.TrimSpace(projectKanbanCardProjectID(data, card))
	if moveProjectID == "" {
		moveProjectID = projectID
	}
	identity := boardCardIdentityToken(card.Identifier, card.IssueID, card.IssueNumber)
	moveDisabledText := projectKanbanCardMoveDisabledText(data, card)
	canDrag := moveDisabledText == ""
	view := boardCardView{
		DomID:             "card-" + boardCardScopedSlug(projectID, identity),
		Identity:          identity,
		IssueID:           card.IssueID,
		Number:            card.IssueNumber,
		Project:           projectID,
		MoveProject:       moveProjectID,
		Scope:             scope,
		CurrentState:      card.Stage,
		DragDrop:          canDrag || moveDisabledText != "",
		CanDrag:           canDrag,
		MoveDisabledText:  moveDisabledText,
		MoveDisabledLabel: boardMoveDisabledLabel(moveDisabledText),
		Running:           strings.EqualFold(lane.Title, "In Progress"),
		// Done drives the green ✓; other terminal states (Cancelled, Closed)
		// are terminal but not done, so they suppress meta without claiming
		// success.
		Done:     strings.EqualFold(lane.Title, "Done"),
		Terminal: terminal,
		Title:    card.Title,
	}
	if canDrag {
		view.AllowedTargets = projectKanbanMoveTargetKeys(data, card)
	}
	if card.PRNumber > 0 {
		view.PRNumber = strconv.Itoa(card.PRNumber)
	}
	switch {
	case view.Done || view.Terminal:
		view.MetaRight = ""
	case card.PRNumber > 0 && !view.Running:
		view.MetaRight = "PR #" + strconv.Itoa(card.PRNumber)
	}
	if boardLaneShowsAge(lane.Title, view.Terminal) {
		view.AgeFooter = boardCompactAge(card.TimeInStage)
		view.AgeFooterTitle = strings.TrimSpace(card.TimeInStageTitle)
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
	if boardBlockedWaiting(card.BlockedSource, card.BlockedRecoveryReason, card.BlockedReason) {
		return primitives.KindWarn, boardCardBlockedWaitingText(card), true
	}
	if reason := boardBlockedDetail(card.BlockedSource, card.BlockedRecoveryReason, card.BlockedReason); reason != "" {
		return primitives.KindErr, "needs review - " + reason, true
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
	if card.GatePending {
		return primitives.KindInfo, "Awaiting checks", true
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

func boardCardBlockedWaitingText(card projectKanbanCard) string {
	if len(card.Blockers) > 0 {
		return "waiting - " + card.Blockers[0]
	}
	if detail := boardBlockedDetail(card.BlockedSource, card.BlockedRecoveryReason, card.BlockedReason); detail != "" {
		return "waiting - " + detail
	}
	if boardBlockedDependencyWaiting(card.BlockedSource, card.BlockedRecoveryReason, card.BlockedReason, nil) {
		return "waiting - dependency"
	}
	return "waiting - project status"
}

func boardBlockedWaiting(source telemetry.BlockedSource, recoveryReason string, reason string) bool {
	if strings.EqualFold(strings.TrimSpace(recoveryReason), "human_blocker") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(recoveryReason), "dependency_blocker") {
		return true
	}
	switch telemetry.BlockedSource(strings.TrimSpace(string(source))) {
	case telemetry.BlockedSourceDependency, telemetry.BlockedSourceProjectStatus:
		return true
	default:
		return boardBlockedLegacyWaitingReason(reason)
	}
}

func boardBlockedDependencyWaiting(source telemetry.BlockedSource, recoveryReason string, reason string, blockers []telemetry.BlockedRef) bool {
	if telemetry.BlockedSource(strings.TrimSpace(string(source))) == telemetry.BlockedSourceDependency {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(recoveryReason), "dependency_blocker") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(reason), "blocked by non-terminal dependency") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), "depends on ") {
		return true
	}
	return len(blockers) > 0
}

func boardBlockedLegacyWaitingReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	if strings.EqualFold(reason, "blocked by non-terminal dependency") {
		return true
	}
	if strings.EqualFold(reason, "blocked by project status") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(reason), "depends on ")
}

func boardBlockedDetail(source telemetry.BlockedSource, recoveryReason string, reason string) string {
	reason = strings.TrimSpace(reason)
	switch reason {
	case "blocked by non-terminal dependency":
		return "dependency not ready"
	case "blocked by project status":
		if strings.EqualFold(strings.TrimSpace(recoveryReason), "dependency_blocker") ||
			telemetry.BlockedSource(strings.TrimSpace(string(source))) == telemetry.BlockedSourceDependency {
			return "dependency not ready"
		}
		return "paused by project status"
	default:
		return reason
	}
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
	if card.ExtraChip && card.ExtraKind == primitives.KindWarn {
		return "flex flex-none flex-col gap-1.5 rounded-card border border-warn/45 bg-elev p-3"
	}
	if card.ExtraChip && card.ExtraKind == primitives.KindErr {
		return "flex flex-none flex-col gap-1.5 rounded-card border border-err/45 bg-elev p-3"
	}
	return "flex flex-none flex-col gap-1.5 rounded-card border border-line bg-elev p-3"
}

func boardCardInteractionClass(card boardCardView) string {
	base := " select-none data-[kanban-dragging=true]:opacity-60"
	if card.CanDrag {
		return base + " cursor-grab hover:border-accent/50 active:cursor-grabbing"
	}
	if card.MoveDisabledText != "" {
		return base + " cursor-pointer hover:border-line"
	}
	return " cursor-pointer hover:border-accent/50"
}

func boardMoveDisabledLabel(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch {
	case reason == "":
		return ""
	case strings.Contains(reason, "all-project"), strings.Contains(reason, "read-only"), strings.Contains(reason, "tracker does not support"):
		return "Read-only"
	case strings.Contains(reason, "snapshot"), strings.Contains(reason, "refresh"):
		return "Stale"
	case strings.Contains(reason, "linked issue"):
		return "No issue"
	default:
		return "No move"
	}
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

// boardLaneShowsAge keeps intake and terminal lanes quiet: time-in-stage only
// matters once work is moving and before it has finished.
func boardLaneShowsAge(title string, terminal bool) bool {
	if terminal {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(title)) {
	case "backlog", "todo", "done", "cancelled", "canceled", "closed", "duplicate":
		return false
	}
	return true
}

func boardCardIdentityToken(identifier string, issueID string, number string) string {
	identifier = strings.TrimSpace(identifier)
	if issueIdentifierHasNumber(identifier) {
		return identifier
	}
	if issueID = strings.TrimSpace(issueID); issueID != "" {
		return issueID
	}
	return strings.TrimSpace(number)
}

func issueIdentifierHasNumber(identifier string) bool {
	index := strings.LastIndex(identifier, "#")
	return index > 0 && index < len(identifier)-1
}

func issueIdentifierHasRepositoryNumber(identifier string) bool {
	index := strings.LastIndex(identifier, "#")
	return index > 0 && index < len(identifier)-1 && strings.Contains(identifier[:index], "/")
}

func boardCardScopedIdentityToken(projectID string, identity string) string {
	projectID = strings.TrimSpace(projectID)
	identity = strings.TrimSpace(identity)
	if identity == "" || projectID == "" || issueIdentifierHasRepositoryNumber(identity) {
		return identity
	}
	return projectID + ":" + identity
}

func boardCardScopedSlug(projectID string, identity string) string {
	return boardCardSlug(boardCardScopedIdentityToken(projectID, identity))
}

func boardCardSlug(identity string) string {
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.TrimSpace(identity) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			builder.WriteRune(r)
			lastDash = false
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + ('a' - 'A'))
			lastDash = false
		default:
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "unknown"
	}
	return slug
}
