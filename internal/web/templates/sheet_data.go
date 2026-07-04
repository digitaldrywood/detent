package templates

import (
	"net/url"
	"strings"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// sheetSession is the live-session slice of the detail sheet: what the
// agent is doing right now, or why it is blocked.
type sheetSession struct {
	Present   bool
	Kind      primitives.Kind
	State     string
	Host      string
	SessionID string
	Runtime   string
	Turns     string
	LastEvent string
	Message   string
	Error     string
}

// FindBoardCard locates one card on the fleet board by project and issue
// number so the detail sheet can render it.
func FindBoardCard(data DashboardData, projectID string, issueNumber string) (projectKanbanCard, bool) {
	projectID = strings.TrimSpace(projectID)
	issueNumber = strings.TrimSpace(issueNumber)
	if !strings.HasPrefix(issueNumber, "#") && issueNumber != "" {
		issueNumber = "#" + issueNumber
	}
	board := projectKanbanBoardView(data)
	for _, lane := range board.AllLanes {
		for _, card := range lane.Cards {
			if card.IssueNumber == issueNumber && (projectID == "" || strings.TrimSpace(card.ProjectID) == projectID) {
				return card, true
			}
		}
	}
	return projectKanbanCard{}, false
}

func sheetSessionFor(snapshot telemetry.Snapshot, card projectKanbanCard) sheetSession {
	for _, running := range snapshot.Running {
		if issueIdentifier(running.Issue) == card.Identifier {
			session := sheetSession{
				Present:   true,
				Kind:      primitives.KindOK,
				State:     "Agent running",
				Host:      strings.TrimSpace(running.WorkerHost),
				SessionID: strings.TrimSpace(running.SessionID),
				Runtime:   formatDuration(running.RuntimeSeconds),
				Turns:     formatCount(running.TurnCount),
				LastEvent: strings.TrimSpace(running.LastEvent),
				Message:   strings.TrimSpace(running.LastMessage),
			}
			return session
		}
	}
	for _, blocked := range snapshot.Blocked {
		if issueIdentifier(blocked.Issue) == card.Identifier {
			session := sheetSession{
				Present:   true,
				Kind:      primitives.KindErr,
				State:     "Session blocked",
				Host:      strings.TrimSpace(blocked.WorkerHost),
				SessionID: strings.TrimSpace(blocked.SessionID),
				LastEvent: strings.TrimSpace(blocked.LastEvent),
				Message:   strings.TrimSpace(blocked.LastMessage),
				Error:     strings.TrimSpace(blocked.Error),
			}
			return session
		}
	}
	return sheetSession{}
}

// boardCardSheetPath is the hx-get target that opens the detail sheet.
func boardCardSheetPath(projectID string, issueNumber string) string {
	values := url.Values{}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		values.Set("project", projectID)
	}
	values.Set("issue", strings.TrimPrefix(strings.TrimSpace(issueNumber), "#"))
	return "/api/v1/board/card?" + values.Encode()
}

func sheetOpenAttrs(projectID string, issueNumber string) templ.Attributes {
	return templ.Attributes{
		"hx-get":    boardCardSheetPath(projectID, issueNumber),
		"hx-target": "#detail-sheet-host",
		"hx-swap":   "innerHTML",
	}
}

func sheetHasActions(data DashboardData, card projectKanbanCard) bool {
	return projectKanbanCardCanMove(data, card) || projectKanbanCardCanRemove(data, card)
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
