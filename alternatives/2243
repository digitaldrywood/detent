package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func (o *Orchestrator) agentOverrideRejectionHandler(ctx context.Context, issue connector.Issue) runpkg.AgentOverrideRejectionHandler {
	return func(rejections []runpkg.AgentOverrideRejection) error {
		body, marker := agentOverrideRejectionComment(rejections)
		if body == "" || issueHasAgentOverrideRejectionMarker(issue, marker) {
			return nil
		}
		return o.connector.CreateComment(ctx, issue.ID, body)
	}
}

func agentOverrideRejectionComment(rejections []runpkg.AgentOverrideRejection) (string, string) {
	if len(rejections) == 0 {
		return "", ""
	}

	var signature strings.Builder
	var details strings.Builder
	for _, rejection := range rejections {
		field := strings.TrimSpace(rejection.Field)
		value := strings.TrimSpace(rejection.Value)
		reason := strings.TrimSpace(rejection.Reason)
		if field == "" || reason == "" {
			continue
		}
		fmt.Fprintf(&signature, "%s\x00%s\x00%s\n", field, value, reason)
		fmt.Fprintf(&details, "\n- `%s`", field)
		if value != "" {
			fmt.Fprintf(&details, " value %q", value)
		}
		details.WriteString(": ")
		details.WriteString(reason)
	}
	if details.Len() == 0 {
		return "", ""
	}

	sum := sha256.Sum256([]byte(signature.String()))
	hash := hex.EncodeToString(sum[:])[:16]
	marker := "<!-- detent-agent-rejection:" + hash + " -->"
	body := marker + "\nDetent ignored the following issue-body `detent-agent` override(s) and continued with the project defaults:" + details.String()
	return body, marker
}

func issueHasAgentOverrideRejectionMarker(issue connector.Issue, marker string) bool {
	if marker == "" {
		return false
	}
	for _, comment := range issue.Comments {
		if strings.Contains(comment.Body, marker) {
			return true
		}
	}
	return false
}
