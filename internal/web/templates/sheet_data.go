package templates

import (
	"net/url"
	"strings"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// sheetSession is the live-session slice of the detail sheet: what the
// agent is doing right now, or why it is blocked.
type sheetSession struct {
	Present         bool
	Kind            primitives.Kind
	State           string
	Host            string
	SessionID       string
	Runtime         string
	Turns           string
	LastEvent       string
	Message         string
	Error           string
	RuntimeIdentity agentidentity.Identity
}

// FindBoardCard locates one card on the fleet board by project and issue
// identity so the detail sheet can render it.
func FindBoardCard(data DashboardData, projectID string, issueIdentity string) (projectKanbanCard, bool) {
	projectID = strings.TrimSpace(projectID)
	issueIdentity = strings.TrimSpace(issueIdentity)
	if issueIdentity == "" {
		return projectKanbanCard{}, false
	}
	// Legacy single-project snapshots can leave Issue.ProjectID empty, so a
	// card matches when its project id equals the request or when the card
	// carries none and the request scopes to the dashboard's project.
	fallbackProjectID := strings.TrimSpace(data.ProjectID)
	board := projectKanbanBoardView(data)
	for _, lane := range board.AllLanes {
		for _, card := range lane.Cards {
			if !boardCardMatchesIdentity(card, issueIdentity) {
				continue
			}
			cardProjectID := strings.TrimSpace(card.ProjectID)
			if cardProjectID == "" {
				cardProjectID = fallbackProjectID
			}
			if projectID == "" || cardProjectID == projectID {
				return card, true
			}
		}
	}
	return projectKanbanCard{}, false
}

func boardCardMatchesIdentity(card projectKanbanCard, issueIdentity string) bool {
	issueIdentity = strings.TrimSpace(issueIdentity)
	if issueIdentity == "" {
		return false
	}
	for _, candidate := range []string{card.Identity, card.Identifier, card.IssueID} {
		if strings.TrimSpace(candidate) == issueIdentity {
			return true
		}
	}
	return boardCardMatchesLegacyIssueNumber(card.IssueNumber, issueIdentity)
}

func boardCardMatchesLegacyIssueNumber(cardNumber string, issueIdentity string) bool {
	cardNumber = strings.TrimSpace(cardNumber)
	issueIdentity = strings.TrimSpace(issueIdentity)
	if cardNumber == "" || issueIdentity == "" {
		return false
	}
	if cardNumber == issueIdentity {
		return true
	}
	if strings.HasPrefix(cardNumber, "#") {
		return strings.TrimPrefix(cardNumber, "#") == strings.TrimPrefix(issueIdentity, "#")
	}
	return false
}

func sheetSessionFor(snapshot telemetry.Snapshot, card projectKanbanCard) sheetSession {
	for _, running := range snapshot.Running {
		if issueIdentifier(running.Issue) == card.Identifier && sheetSessionMatchesProject(running.ProjectID, card) {
			identity := running.RuntimeIdentity
			if identity.IsZero() {
				identity = card.RuntimeIdentity
			}
			session := sheetSession{
				Present:         true,
				Kind:            primitives.KindOK,
				State:           "Agent running",
				Host:            strings.TrimSpace(running.WorkerHost),
				SessionID:       strings.TrimSpace(running.SessionID),
				Runtime:         formatDuration(running.RuntimeSeconds),
				Turns:           formatCount(running.TurnCount),
				LastEvent:       strings.TrimSpace(running.LastEvent),
				Message:         strings.TrimSpace(displayOutputText(running.LastMessage, running.LastMessageTruncation)),
				RuntimeIdentity: identity,
			}
			return session
		}
	}
	for _, blocked := range snapshot.Blocked {
		if issueIdentifier(blocked.Issue) == card.Identifier && sheetSessionMatchesProject(blocked.ProjectID, card) {
			session := sheetSession{
				Present:         true,
				Kind:            primitives.KindErr,
				State:           "Session blocked",
				Host:            strings.TrimSpace(blocked.WorkerHost),
				SessionID:       strings.TrimSpace(blocked.SessionID),
				LastEvent:       strings.TrimSpace(blocked.LastEvent),
				Message:         strings.TrimSpace(blocked.LastMessage),
				Error:           blockedSessionError(blocked),
				RuntimeIdentity: card.RuntimeIdentity,
			}
			return session
		}
	}
	if !card.RuntimeIdentity.IsZero() {
		return sheetSession{
			Present:         true,
			Kind:            primitives.KindNeutral,
			State:           "Recent session",
			RuntimeIdentity: card.RuntimeIdentity,
		}
	}
	return sheetSession{}
}

func blockedSessionError(blocked telemetry.Blocked) string {
	cause := strings.TrimSpace(blocked.Error)
	attemptError := strings.TrimSpace(blocked.AttemptError)
	if cause == "" {
		return attemptError
	}
	if attemptError == "" {
		return cause
	}
	return cause + " — last attempt: " + attemptError
}

func findEfficiencyReceipt(receipts []efficiency.Receipt, issue telemetry.Issue) (efficiency.Receipt, bool) {
	for _, receipt := range receipts {
		if strings.TrimSpace(receipt.ProjectID) != "" && strings.TrimSpace(issue.ProjectID) != "" && receipt.ProjectID != issue.ProjectID {
			continue
		}
		if receipt.IssueID == issue.ID || (receipt.Identifier != "" && receipt.Identifier == issue.Identifier) {
			return receipt, true
		}
	}
	return efficiency.Receipt{}, false
}

func efficiencyReceiptForCard(data DashboardData, card projectKanbanCard) (efficiency.Receipt, bool) {
	return findEfficiencyReceipt(data.EfficiencyReceipts, telemetry.Issue{
		ID:         card.IssueID,
		Identifier: card.Identifier,
		ProjectID:  card.ProjectID,
	})
}

// sheetSessionMatchesProject guards against cross-project identifier
// collisions: non-GitHub trackers can reuse an identifier (e.g. MT-1)
// across projects, so a session only matches a card when their project
// ids agree. Empty ids on either side stay permissive.
func sheetSessionMatchesProject(sessionProjectID string, card projectKanbanCard) bool {
	sessionProjectID = strings.TrimSpace(sessionProjectID)
	cardProjectID := strings.TrimSpace(card.ProjectID)
	if sessionProjectID == "" || cardProjectID == "" {
		return true
	}
	return sessionProjectID == cardProjectID
}

// boardCardSheetPath is the hx-get target that opens the detail sheet.
// Scope records which board hosts the sheet (fleet or project) so the
// server rebuilds the same scope. boardActions records whether the
// opening page's #snapshot region actually holds a board — only then can
// the sheet's Move/Remove actions safely morph the board back into it.
// The Fleet and project Overview pages show the exception strip but their
// #snapshot holds other content, so their Review sheets omit those
// actions rather than swap board lanes over the page the user is on.
func boardCardSheetPath(projectID string, issueIdentity string, scope string, boardActions bool) string {
	return boardCardDetailPath("/api/v1/board/card", projectID, issueIdentity, scope, boardActions)
}

func boardCardCorePath(projectID string, issueIdentity string, scope string, boardActions bool) string {
	return boardCardDetailPath("/api/v1/board/card/core", projectID, issueIdentity, scope, boardActions)
}

func boardCardReceiptPath(projectID string, issueIdentity string, scope string, boardActions bool) string {
	return boardCardDetailPath("/api/v1/board/receipt", projectID, issueIdentity, scope, boardActions)
}

func boardCardDetailPath(path string, projectID string, issueIdentity string, scope string, boardActions bool) string {
	values := url.Values{}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		values.Set("project", projectID)
	}
	values.Set("issue", strings.TrimSpace(issueIdentity))
	if scope = strings.TrimSpace(scope); scope != "" && scope != "fleet" {
		values.Set("scope", scope)
	}
	if boardActions {
		values.Set("actions", "board")
	}
	return path + "?" + values.Encode()
}

func sheetOpenAttrs(projectID string, issueIdentity string, scope string, boardActions bool) templ.Attributes {
	return templ.Attributes{
		"hx-get":                     boardCardSheetPath(projectID, issueIdentity, scope, boardActions),
		"hx-trigger":                 "click[!event.target.closest('[data-ai-debug-url]')]",
		"hx-target":                  "#detail-sheet-host",
		"hx-swap":                    "innerHTML",
		"hx-sync":                    "#detail-sheet-host:replace",
		"data-detail-sheet-trigger":  "true",
		"data-detail-sheet-core-url": boardCardCorePath(projectID, issueIdentity, scope, boardActions),
	}
}

func boardCardDetailIdentity(data DashboardData, card projectKanbanCard) (string, string, string) {
	projectID := projectKanbanCardProjectID(data, card)
	identity := boardCardIdentityToken(card.Identifier, card.IssueID, card.IssueNumber)
	return projectID, identity, projectKanbanBoardScope(data)
}

func boardCardDetailKey(projectID string, issueIdentity string) string {
	return boardCardScopedSlug(projectID, issueIdentity)
}

func sheetHasActions(data DashboardData, card projectKanbanCard, boardActions bool) bool {
	return boardActions && (projectKanbanCardCanMove(data, card) ||
		projectKanbanCardCanRemove(data, card) ||
		projectKanbanCardCanComment(data, card))
}

func projectKanbanCardUsesInternalIssueView(data DashboardData, card projectKanbanCard) bool {
	return strings.EqualFold(strings.TrimSpace(projectKanbanCardKanbanData(data, card).TrackerKind), "local_sqlite")
}

func boardCardSheetClass(expanded bool) string {
	base := "flex h-full w-full min-w-0 flex-none flex-col overflow-hidden bg-surface md:border-l md:border-line"
	if expanded {
		return base
	}
	return base + " md:w-100"
}

func sheetAttentionLabel(card projectKanbanCard) string {
	label := strings.TrimSpace(card.AttentionLabel)
	if detail := strings.TrimSpace(card.AttentionDetail); detail != "" {
		return label + " — " + detail
	}
	return label
}

func sheetStateKind(data DashboardData, card projectKanbanCard) primitives.Kind {
	if len(card.Blockers) > 0 || card.AttentionLabel != "" {
		return primitives.KindErr
	}
	if strings.EqualFold(card.Stage, "In Progress") {
		return primitives.KindOK
	}
	if projectKanbanTerminalState(card.Stage, projectKanbanTerminalStateSet(data.Kanban.TerminalStates)) {
		return primitives.KindNeutral
	}
	return primitives.KindInfo
}
