package dispatchpriority

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

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

func TestCompareOrderingLaws(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	candidates := []Candidate{
		{Issue: connector.Issue{Identifier: "merging", State: " MERGING ", Priority: intPointer(4)}},
		{Issue: connector.Issue{Identifier: "urgent", State: "Todo", Priority: intPointer(1)}},
		{Issue: connector.Issue{Identifier: "hotfix", State: "Todo", Labels: []string{"HOTFIX"}}},
		{Issue: connector.Issue{Identifier: "bug", State: "Todo", Labels: []string{"bug"}}},
		{Issue: connector.Issue{Identifier: "rework", State: "Rework"}},
		{Issue: connector.Issue{Identifier: "unblocker", State: "Todo", UnblockerCount: 2}},
		{Issue: connector.Issue{Identifier: "rank-a", State: "Todo"}, Rank: " a "},
		{Issue: connector.Issue{Identifier: "rank-b", State: "Todo"}, Rank: "b"},
		{Issue: connector.Issue{Identifier: "old", State: "Todo", CreatedAt: &old}},
		{Issue: connector.Issue{Identifier: "new", State: "Todo", CreatedAt: &newer}},
		{Issue: connector.Issue{Identifier: "missing-a", State: "Todo"}},
		{Issue: connector.Issue{Identifier: "missing-b", State: "Todo"}, Rank: " "},
	}
	for _, mergingFirst := range []bool{false, true} {
		states := []string{"Rework", "Merging", "Todo"}
		if mergingFirst {
			states = []string{"Merging", "Rework", "Todo"}
		}
		ranker := New(states, []string{"hotfix", "bug"})
		for _, prioritizeUnblockers := range []bool{false, true} {
			for _, a := range candidates {
				if got := ranker.Compare(a, a, prioritizeUnblockers); got != 0 {
					t.Fatalf("self comparison of %s = %d", a.Issue.Identifier, got)
				}
				for _, b := range candidates {
					ab := ranker.Compare(a, b, prioritizeUnblockers)
					ba := ranker.Compare(b, a, prioritizeUnblockers)
					if a.Issue.Identifier != b.Issue.Identifier && ab == 0 {
						t.Fatalf("distinct candidates %s and %s compare equal", a.Issue.Identifier, b.Issue.Identifier)
					}
					if ab != -ba {
						t.Fatalf("asymmetric comparison of %s and %s: %d, %d", a.Issue.Identifier, b.Issue.Identifier, ab, ba)
					}
					for _, c := range candidates {
						if ab <= 0 && ranker.Compare(b, c, prioritizeUnblockers) <= 0 && ranker.Compare(a, c, prioritizeUnblockers) > 0 {
							t.Fatalf("nontransitive order: %s <= %s <= %s", a.Issue.Identifier, b.Issue.Identifier, c.Issue.Identifier)
						}
					}
				}
			}
		}
	}
}
