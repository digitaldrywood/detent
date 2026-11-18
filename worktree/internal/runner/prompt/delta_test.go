package prompt

import (
	"fmt"
	"strings"
	"testing"
)

func TestPromptDelta(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, before, after, want string }{
		{"unchanged", "same", "same", "Continue the assigned work"},
		{"replacement", "first\nold\nlast", "first\nnew\nlast", "Replace 1 line(s) starting at line 2 with:\nnew"},
		{"insertion", "first\nlast", "first\nnew\nlast", "Replace 0 line(s) starting at line 2 with:\nnew"},
		{"deletion", "first\nold\nlast", "first\nlast", "Replace 1 line(s) starting at line 2 with:\n(deleted)"},
		{"append", "first", "first\nnew", "Replace 0 line(s) starting at line 2 with:\nnew"},
		{"trailing deletion", "first\nold", "first", "Replace 1 line(s) starting at line 2 with:\n(deleted)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Delta(tt.before, tt.after); !strings.Contains(got, tt.want) {
				t.Fatalf("delta = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPromptDeltaInsertedParagraphs(t *testing.T) {
	t.Parallel()
	var workflow strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&workflow, "Standing instruction %d\n", i)
	}
	before := "head\n\n" + workflow.String() + "\nend"
	for _, after := range []string{
		"head\nnew handoff\n\n\n" + workflow.String() + "\nend",
		"head\nnew handoff\n\n\n" + workflow.String() + "\nnew summary\nend",
	} {
		got := Delta(before, after)
		if len(got) >= 4096 || strings.Contains(got, "Standing instruction") {
			t.Fatalf("small update repeats workflow (%d bytes)", len(got))
		}
		if !strings.Contains(got, "new handoff") {
			t.Fatal("missing handoff")
		}
	}
}
