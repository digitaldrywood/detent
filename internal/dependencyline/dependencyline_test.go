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
		{name: "depends on no colon", line: "Depends on #1443", wantText: "#1443", wantOK: true},
		{name: "blocked by no colon", line: "Blocked by #1447", wantText: "#1447", wantOK: true},
		{name: "depends on colon", line: "Depends on: #1443", wantText: "#1443", wantOK: true},
		{name: "depends on colon no space", line: "Depends on:#1443", wantText: "#1443", wantOK: true},
		{name: "depends hyphen owner repo", line: "depends-on digitaldrywood/detent#1443", wantText: "digitaldrywood/detent#1443", wantOK: true},
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
