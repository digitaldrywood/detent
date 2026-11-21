package viewstore

import (
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func executionLabel(stage string) string {
	switch stage {
	case "implementation", "implementing", "code":
		return "Implementing"
	case "rework", "reworking":
		return "Reworking"
	case "planning", "plan":
		return "Planning"
	case "validation", "validating", "review", "security_audit":
		return "Validating"
	case "merging", "merge":
		return "Merging"
	case "finalizing":
		return "Finalizing"
	default:
		return ""
	}
}

func StatusLine(card tracker.NativeBoardCard) string {
	if card.Issue.Terminal {
		return card.Issue.State
	}
	blockers := 0
	number := 0
	for _, blocker := range card.Issue.Blockers {
		if !blocker.Terminal {
			blockers++
			number = blocker.Number
		}
	}
	if blockers > 0 {
		if blockers == 1 && number > 0 {
			return "Waiting on #" + strconv.Itoa(number)
		}
		return "Waiting on " + strconv.Itoa(blockers)
	}
	if card.Execution != nil {
		if label := executionLabel(card.Execution.Stage); label != "" {
			return label
		}
		if card.Execution.Running {
			return "Running"
		}
		if card.Attempt == nil || card.Attempt.ID != card.Execution.ID {
			return ""
		}
	}
	if card.Attempt != nil {
		switch card.Attempt.Status {
		case "failed":
			return "Attempt failed"
		case "interrupted":
			return "Interrupted"
		}
	}
	return ""
}

func ExpireLease(attempt *tracker.NativeBoardAttempt, execution *tracker.NativeBoardExecution, line string) string {
	card := tracker.NativeBoardCard{Attempt: attempt, Execution: execution}
	previous := StatusLine(card)
	if attempt != nil && attempt.Status == "running" {
		attempt.Status = "interrupted"
	}
	if execution != nil {
		execution.Running, execution.Stage = false, ""
	}
	if line == previous {
		return StatusLine(card)
	}
	return line
}
