package markdownfence

import "testing"

func TestFenceConsume(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		initial Fence
		line    string
		want    Fence
		marker  bool
	}{
		{name: "prose", line: "hello"},
		{name: "short opening", line: "``"},
		{name: "backticks", line: "  ````markdown  ", want: "````", marker: true},
		{name: "tildes", line: "~~~yaml", want: "~~~", marker: true},
		{name: "content", initial: "```", line: "schema: 1", want: "```"},
		{name: "close", initial: "```", line: "```", marker: true},
		{name: "long close", initial: "```", line: " ````` ", marker: true},
		{name: "short close", initial: "````", line: "```", want: "````", marker: true},
		{name: "wrong marker", initial: "```", line: "~~~", want: "```", marker: true},
		{name: "nested opening", initial: "````", line: "```detent-agent", want: "````", marker: true},
		{name: "closing info", initial: "```", line: "```text", want: "```", marker: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fence := tt.initial
			marker := fence.Consume(tt.line)
			if marker != tt.marker || fence != tt.want {
				t.Fatalf("Consume(%q) = %v, fence %q; want %v, %q", tt.line, marker, fence, tt.marker, tt.want)
			}
		})
	}
}
