package displayorder_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/displayorder"
)

func TestCompare(t *testing.T) {
	t.Parallel()

	urgent, high, normal, low := displayorder.PriorityUrgent, displayorder.PriorityHigh, displayorder.PriorityNormal, displayorder.PriorityLow
	invalidLow, invalidHigh := -1, 4
	older := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	zero := time.Time{}

	tests := []struct {
		name     string
		terminal bool
		left     displayorder.Item
		right    displayorder.Item
		want     int
	}{
		{
			name:  "non-terminal urgent before high despite activity",
			left:  displayorder.Item{Priority: &urgent, LastActivityAt: older, Identifier: "B"},
			right: displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
			want:  -1,
		},
		{
			name:  "non-terminal high before normal despite activity",
			left:  displayorder.Item{Priority: &high, LastActivityAt: older},
			right: displayorder.Item{Priority: &normal, LastActivityAt: newer},
			want:  -1,
		},
		{
			name:  "non-terminal normal before low despite activity",
			left:  displayorder.Item{Priority: &normal, LastActivityAt: older},
			right: displayorder.Item{Priority: &low, LastActivityAt: newer},
			want:  -1,
		},
		{
			name:  "non-terminal low before missing priority despite activity",
			left:  displayorder.Item{Priority: &low, LastActivityAt: older},
			right: displayorder.Item{LastActivityAt: newer},
			want:  -1,
		},
		{
			name:  "non-terminal priority before missing activity",
			left:  displayorder.Item{Priority: &urgent},
			right: displayorder.Item{Priority: &low, LastActivityAt: newer},
			want:  -1,
		},
		{
			name:  "non-terminal equal priority uses newest activity",
			left:  displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "B"},
			right: displayorder.Item{Priority: &high, LastActivityAt: older, Identifier: "A"},
			want:  -1,
		},
		{
			name:  "non-terminal equal activity uses identifier",
			left:  displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
			right: displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "B"},
			want:  -1,
		},
		{
			name:  "non-terminal missing priorities use activity",
			left:  displayorder.Item{LastActivityAt: newer, Identifier: "B"},
			right: displayorder.Item{LastActivityAt: older, Identifier: "A"},
			want:  -1,
		},
		{
			name:  "non-terminal missing activity follows known activity",
			left:  displayorder.Item{Priority: &high, LastActivityAt: newer},
			right: displayorder.Item{Priority: &high},
			want:  -1,
		},
		{
			name:  "non-terminal missing activities use identifier",
			left:  displayorder.Item{Identifier: "A"},
			right: displayorder.Item{Identifier: "B"},
			want:  -1,
		},
		{
			name:  "non-terminal negative priority acts as none",
			left:  displayorder.Item{Priority: &low, LastActivityAt: older},
			right: displayorder.Item{Priority: &invalidLow, LastActivityAt: newer},
			want:  -1,
		},
		{
			name:  "non-terminal out of range priority acts as none",
			left:  displayorder.Item{Priority: &invalidHigh, LastActivityAt: newer},
			right: displayorder.Item{LastActivityAt: older},
			want:  -1,
		},
		{
			name:     "terminal ignores priority",
			terminal: true,
			left:     displayorder.Item{Priority: &urgent, LastActivityAt: older, Identifier: "A"},
			right:    displayorder.Item{Priority: &low, LastActivityAt: newer, Identifier: "B"},
			want:     1,
		},
		{
			name:     "terminal equal activity uses identifier despite priority",
			terminal: true,
			left:     displayorder.Item{Priority: &low, LastActivityAt: newer, Identifier: "A"},
			right:    displayorder.Item{Priority: &urgent, LastActivityAt: newer, Identifier: "B"},
			want:     -1,
		},
		{
			name:     "terminal missing priority still uses activity",
			terminal: true,
			left:     displayorder.Item{LastActivityAt: newer},
			right:    displayorder.Item{Priority: &urgent, LastActivityAt: older},
			want:     -1,
		},
		{
			name:     "terminal missing activity follows known activity despite priority",
			terminal: true,
			left:     displayorder.Item{Priority: &urgent},
			right:    displayorder.Item{Priority: &low, LastActivityAt: newer},
			want:     1,
		},
		{
			name:     "terminal missing activities use identifier despite priority",
			terminal: true,
			left:     displayorder.Item{Priority: &low, Identifier: "A"},
			right:    displayorder.Item{Priority: &urgent, Identifier: "B"},
			want:     -1,
		},
		{
			name:  "missing activity follows activity before zero time",
			left:  displayorder.Item{LastActivityAt: zero.Add(-time.Hour)},
			right: displayorder.Item{},
			want:  -1,
		},
		{
			name:  "equal instants in different zones use identifier",
			left:  displayorder.Item{LastActivityAt: newer, Identifier: "A"},
			right: displayorder.Item{LastActivityAt: newer.In(time.FixedZone("offset", 3600)), Identifier: "B"},
			want:  -1,
		},
		{
			name:  "identical items compare equal",
			left:  displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
			right: displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
		},
		{
			name: "zero values compare equal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := displayorder.Compare(tt.terminal, tt.left, tt.right); got != tt.want {
				t.Fatalf("Compare(left, right) = %d, want %d", got, tt.want)
			}
			if got := displayorder.Compare(tt.terminal, tt.right, tt.left); got != -tt.want {
				t.Fatalf("Compare(right, left) = %d, want %d", got, -tt.want)
			}
		})
	}
}
