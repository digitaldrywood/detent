package prompt

import (
	"fmt"
	"strings"
)

// Describe edits against the context reconstructed by applying all earlier
// updates. Unchanged and deleted text need not be sent again.
func Delta(previous, current string) string {
	const continuation = "Continue the assigned work using the Detent instructions already in this thread."
	if previous == current {
		return continuation
	}
	oldLines, newLines := strings.Split(previous, "\n"), strings.Split(current, "\n")
	var out strings.Builder
	out.WriteString(continuation + "\n\nApply these context updates after all earlier updates. Line numbers refer to that reconstructed context before applying this message; unchanged instructions remain in force.\n")
	var describe func(int, int, int, int)
	describe = func(oldStart, oldEnd, newStart, newEnd int) {
		for oldStart < oldEnd && newStart < newEnd && oldLines[oldStart] == newLines[newStart] {
			oldStart++
			newStart++
		}
		for oldStart < oldEnd && newStart < newEnd && oldLines[oldEnd-1] == newLines[newEnd-1] {
			oldEnd--
			newEnd--
		}
		if oldStart == oldEnd && newStart == newEnd {
			return
		}
		// Anchor on the longest unchanged run, rather than greedily pairing common
		// blank lines that can make a small insertion repeat the rest of the prompt.
		positions := make(map[string][]int)
		for j := newStart; j < newEnd; j++ {
			positions[newLines[j]] = append(positions[newLines[j]], j)
		}
		best, oldMatch, newMatch := 0, oldStart, newStart
		lengths := make(map[int]int)
		for i := oldStart; i < oldEnd; i++ {
			next := make(map[int]int)
			for _, j := range positions[oldLines[i]] {
				n := lengths[j-1] + 1
				next[j] = n
				if n > best {
					best = n
					oldMatch = i - n + 1
					newMatch = j - n + 1
				}
			}
			lengths = next
		}
		if best > 0 {
			describe(oldStart, oldMatch, newStart, newMatch)
			describe(oldMatch+best, oldEnd, newMatch+best, newEnd)
			return
		}
		fmt.Fprintf(&out, "\nReplace %d line(s) starting at line %d with:\n", oldEnd-oldStart, oldStart+1)
		if newStart == newEnd {
			out.WriteString("(deleted)\n")
		} else {
			out.WriteString(strings.Join(newLines[newStart:newEnd], "\n") + "\n")
		}
	}
	describe(0, len(oldLines), 0, len(newLines))
	return out.String()
}
