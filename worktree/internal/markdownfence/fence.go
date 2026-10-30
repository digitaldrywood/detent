// Package markdownfence shares fence boundaries used by issue-body parsers.
package markdownfence

import "strings"

// Fence is the active fence marker. Its zero value is outside a fence.
type Fence string

// Consume advances the fence state and reports whether line is a fence marker.
// Shorter or mismatched markers do not close an active fence.
func (f *Fence) Consume(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") && !strings.HasPrefix(trimmed, "~~~") {
		return false
	}
	if *f == "" {
		end := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
		*f = Fence(trimmed[:end])
	} else if len(trimmed) >= len(*f) && strings.Trim(trimmed, string((*f)[:1])) == "" {
		*f = ""
	}
	return true
}
