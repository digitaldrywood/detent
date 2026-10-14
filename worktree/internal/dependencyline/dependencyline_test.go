package dependencyline

import "testing"

func TestMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		line     string
		wantText string
		wantOK   bool
	}{
		{name: "none with prose", line: "Depends on: none. Order: #2, #3 (#2 → #3).", wantOK: true},
		{name: "not applicable", line: "Blocked by: n/a, see #9", wantOK: true},
		{name: "dash empty", line: "Depends on: - #9", wantOK: true},
		{name: "sentence boundary", line: "Depends on: #2, #3. See #9", wantText: "#2, #3", wantOK: true},
		{name: "and before prose", line: "Depends on: #2 and #3 — see #9 for context", wantText: "#2 and #3", wantOK: true},
		{name: "exclamation boundary", line: "Blocked by: #2! Then #9", wantText: "#2", wantOK: true},
		{name: "question boundary", line: "Depends on: #2? Ask #9", wantText: "#2", wantOK: true},
		{name: "dotted repo", line: "Depends on: owner/repo.name#2, #3.", wantText: "owner/repo.name#2, #3", wantOK: true},
		{name: "prose after comma", line: "Depends on: #2, see #9", wantText: "#2", wantOK: true},
		{name: "depends on no colon", line: "Depends on #1443", wantText: "#1443", wantOK: true},
		{name: "blocked by no colon", line: "Blocked by #1447", wantText: "#1447", wantOK: true},
		{name: "depends on colon", line: "Depends on: #1443", wantText: "#1443", wantOK: true},
		{name: "depends on colon no space", line: "Depends on:#1443", wantText: "#1443", wantOK: true},
		{name: "depends hyphen owner repo", line: "depends-on digitaldrywood/detent#1443", wantText: "digitaldrywood/detent#1443", wantOK: true},
		{name: "bold colon inside", line: "**Depends on:** #1443", wantText: "#1443", wantOK: true},
		{name: "bold colon outside", line: "**Depends on**: owner/repo#1443", wantText: "owner/repo#1443", wantOK: true},
		{name: "italic", line: "_Blocked by:_ https://github.com/owner/repo/issues/1443", wantText: "https://github.com/owner/repo/issues/1443", wantOK: true},
		{name: "list with prose", line: "- **Depends on:** #1443 so the schema exists", wantText: "#1443", wantOK: true},
		{name: "code label", line: "`Depends on:` #1443", wantText: "#1443", wantOK: true},
		{name: "backticked declaration", line: "`Blocked by: #1447`", wantOK: false},
		{name: "quoted declaration", line: "> Blocked by: #1447", wantOK: false},
		{name: "mention only", line: "Mention only #1443", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotText, gotOK := Match(tt.line)
			if gotOK != tt.wantOK {
				t.Fatalf("Match() ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotText != tt.wantText {
				t.Fatalf("Match() text = %q, want %q", gotText, tt.wantText)
			}
		})
	}
}
