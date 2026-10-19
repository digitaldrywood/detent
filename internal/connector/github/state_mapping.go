package github

import (
	"strings"
)

func normalizeStateName(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}
