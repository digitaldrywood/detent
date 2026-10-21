package agentoverride

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/markdownfence"
)

// renderedFence is the fence Render emits. A body is rewritten with this
// fence even when the block it replaces used tildes, so the hub produces one
// shape and FromIssueBody keeps accepting both.
const renderedFence = "```"

// Render returns the detent-agent block for the block-level model and effort
// of the override, or an empty string when neither is set. Role overrides are
// not rendered: nothing that writes a block today sets one, and emitting a
// field the writer never filled would silently widen the block.
func Render(override Override) string {
	model := strings.TrimSpace(override.Model)
	effort := strings.TrimSpace(override.Effort)
	if model == "" && effort == "" {
		return ""
	}
	var block strings.Builder
	block.WriteString(renderedFence + "detent-agent\nschema: 1\n")
	if model != "" {
		block.WriteString("model: " + model + "\n")
	}
	if effort != "" {
		block.WriteString("effort: " + effort + "\n")
	}
	block.WriteString(renderedFence)
	return block.String()
}

// ApplyToIssueBody returns body with its detent-agent block replaced by the
// rendered override. A body without a block gains one at the end; an override
// that sets nothing removes the block instead. Applying the same override
// twice returns the same body, so a caller can compare and skip the write.
func ApplyToIssueBody(body string, override Override) string {
	rendered := Render(override)
	lines := strings.Split(body, "\n")
	start, end, found := lastBlockLines(lines)
	if !found {
		if rendered == "" {
			return body
		}
		trimmed := strings.TrimRight(body, " \t\r\n")
		if trimmed == "" {
			return rendered
		}
		return trimmed + "\n\n" + rendered
	}
	replacement := []string(nil)
	if rendered != "" {
		replacement = strings.Split(rendered, "\n")
	}
	before := trimTrailingBlank(lines[:start])
	after := trimLeadingBlank(lines[end+1:])
	rebuilt := make([]string, 0, len(before)+len(replacement)+len(after)+2)
	rebuilt = append(rebuilt, before...)
	if len(replacement) > 0 {
		if len(rebuilt) > 0 {
			rebuilt = append(rebuilt, "")
		}
		rebuilt = append(rebuilt, replacement...)
	}
	if len(after) > 0 {
		if len(rebuilt) > 0 {
			rebuilt = append(rebuilt, "")
		}
		rebuilt = append(rebuilt, after...)
	}
	return strings.Join(rebuilt, "\n")
}

// lastBlockLines reports the line range of the last detent-agent block,
// opening and closing fence included. It mirrors lastBlock, which returns
// the content rather than where it is.
func lastBlockLines(lines []string) (start, end int, found bool) {
	var fence markdownfence.Fence
	capture := false
	opening := 0
	for index, line := range lines {
		previous := fence
		marker := fence.Consume(line)
		if previous == "" && fence != "" {
			fields := strings.Fields(strings.TrimSpace(line)[len(fence):])
			capture = len(fields) > 0 && fields[0] == "detent-agent"
			opening = index
			continue
		}
		if marker && fence == "" {
			if capture {
				start, end, found = opening, index, true
			}
			capture = false
		}
	}
	return start, end, found
}

func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func trimLeadingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return lines
}
