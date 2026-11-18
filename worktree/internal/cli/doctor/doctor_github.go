package doctor

import (
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func UniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func TrackerUsesGitHubReads(kind string) bool {
	return kind == workflowconfig.TrackerGitHub
}
