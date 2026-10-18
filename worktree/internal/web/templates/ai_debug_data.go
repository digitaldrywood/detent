package templates

import (
	"net/url"
	"strings"
)

func aiDebugIssuePath(projectID string, issueIdentity string) string {
	values := url.Values{
		"scope":   {"issue"},
		"project": {strings.TrimSpace(projectID)},
		"issue":   {strings.TrimSpace(issueIdentity)},
	}
	return "/api/v1/ai-debug?" + values.Encode()
}

func aiDebugProjectPath(projectID string) string {
	values := url.Values{
		"scope":   {"project"},
		"project": {strings.TrimSpace(projectID)},
	}
	return "/api/v1/ai-debug?" + values.Encode()
}

func aiDebugFleetPath() string {
	return "/api/v1/ai-debug?scope=fleet"
}
