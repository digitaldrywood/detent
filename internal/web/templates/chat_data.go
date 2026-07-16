package templates

import (
	"strings"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
)

type ChatData struct {
	Conversation chatpkg.Conversation
	Error        string
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
