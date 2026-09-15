package orchestrator

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCandidatesMissingFromBoard(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name              string
		candidates, board []connector.Issue
		want              int
	}{
		{name: "empty"},
		{name: "missing", candidates: []connector.Issue{{ID: "a"}, {ID: "b"}, {ID: "c"}}, want: 3},
		{name: "retained and transitioned", candidates: []connector.Issue{{ID: "a", State: "Rework"}, {ID: "b", State: "Todo"}}, board: []connector.Issue{{ID: "a", State: "Rework"}, {ID: "b", State: "In Progress"}}},
		{name: "duplicates and empty identities", candidates: []connector.Issue{{ID: "a"}, {ID: "a"}, {}}, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := candidatesMissingFromBoard(tt.candidates, tt.board); got != tt.want {
				t.Fatalf("missing=%d want %d", got, tt.want)
			}
		})
	}
}
