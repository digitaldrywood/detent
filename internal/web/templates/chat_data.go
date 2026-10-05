package templates

import (
	"fmt"
	"strings"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
)

type ChatData struct {
	ApprovalBasePath string
	ApprovalPath     string
	CSRF             string
	Conversation     chatpkg.Conversation
	Error            string
	FormToken        string
	ActionTokens     map[string]string
}

func chatMessageClass(message chatpkg.Message) string {
	if message.Role == chatpkg.RoleUser {
		return "ml-8 rounded-card bg-accent px-3 py-2 text-sm leading-relaxed text-page"
	}
	if message.Error {
		return "mr-8 rounded-card border border-err/40 bg-err/10 px-3 py-2 text-sm leading-relaxed text-err"
	}
	return "mr-8 rounded-card border border-line bg-elev px-3 py-2 text-sm leading-relaxed text-text"
}

func chatActionClass(action chatpkg.Action) string {
	base := "rounded-card border p-3"
	switch action.Status {
	case chatpkg.ActionSucceeded:
		return base + " border-ok/40 bg-ok/10"
	case chatpkg.ActionFailed:
		return base + " border-err/40 bg-err/10"
	case chatpkg.ActionRejected:
		return base + " border-line bg-elev opacity-70"
	default:
		return base + " border-warn/50 bg-warn/10"
	}
}

func chatActionStatus(action chatpkg.Action) string {
	switch action.Status {
	case chatpkg.ActionSucceeded:
		return "Executed"
	case chatpkg.ActionFailed:
		return "Failed"
	case chatpkg.ActionRejected:
		return "Cancelled"
	default:
		return "Confirmation required"
	}
}

func chatActionPath(action chatpkg.Action, decision string) string {
	return "/api/v1/chat/actions/" + action.ID + "/" + strings.TrimSpace(decision)
}

func chatApprovalPath(data ChatData) string {
	if data.ApprovalPath != "" {
		return data.ApprovalPath
	}
	return "/chat/approval"
}

func (d ChatData) approvalStylesheet() string { return d.ApprovalBasePath + "/static/css/output.css" }

func splitPriority(priority *int) string {
	if priority == nil {
		return "No priority"
	}
	if *priority < 0 || *priority > 3 {
		return "Unknown priority"
	}
	return []string{"Urgent", "High", "Normal", "Low"}[*priority]
}

func splitNode(position int, split *chatpkg.IssueSplit) string {
	if position == 0 {
		return "Parent"
	}
	if position < 1 || position > len(split.Children) {
		return "Unknown child"
	}
	return fmt.Sprintf("%d. %s", position, split.Children[position-1].Title)
}
