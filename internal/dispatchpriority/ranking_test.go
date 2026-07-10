package dispatchpriority

import "testing"

func TestRanker(t *testing.T) {
	t.Parallel()

	ranker := New(
		[]string{" Merging ", "Rework", "merging", ""},
		[]string{"hotfix", " Bug ", "HOTFIX", ""},
	)
	tests := []struct {
		name   string
		state  string
		labels []string
		want   int
	}{
		{name: "first configured state", state: "merging", want: 0},
		{name: "second configured state", state: " REWORK ", want: 1},
		{name: "unconfigured state follows configured states", state: "Todo", want: 2},
		{name: "first configured label", labels: []string{"enhancement", "HOTFIX"}, want: 0},
		{name: "second configured label", labels: []string{"bug"}, want: 1},
		{name: "unconfigured label follows configured labels", labels: []string{"enhancement"}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ranker.State(tt.state)
			if tt.labels != nil {
				got = ranker.Label(tt.labels)
			}
			if got != tt.want {
				t.Fatalf("rank = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		priority *int
		want     int
	}{
		{name: "missing", want: UnmappedPriorityRank},
		{name: "top", priority: intPointer(1), want: 1},
		{name: "lowest mapped", priority: intPointer(4), want: 4},
		{name: "zero", priority: intPointer(0), want: UnmappedPriorityRank},
		{name: "above supported range", priority: intPointer(5), want: UnmappedPriorityRank},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Priority(tt.priority); got != tt.want {
				t.Fatalf("Priority() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRankerMatchLabelReturnsConfiguredDisplayLabel(t *testing.T) {
	t.Parallel()

	match, ok := New(nil, []string{"Hotfix", "bug"}).MatchLabel([]string{"BUG", "hotfix"})
	if !ok {
		t.Fatal("MatchLabel() = false, want true")
	}
	if match.Label != "Hotfix" || match.Rank != 0 {
		t.Fatalf("MatchLabel() = %#v, want Hotfix rank 0", match)
	}
}

func intPointer(value int) *int {
	return &value
}
