package testfixture

import (
	"time"

	"github.com/digitaldrywood/detent/internal/displayorder"
)

type Case struct {
	Name     string
	Terminal bool
	Left     displayorder.Item
	Right    displayorder.Item
	Want     int
}

func Comparisons() []Case {
	urgent, high, normal, low := displayorder.PriorityUrgent, displayorder.PriorityHigh, displayorder.PriorityNormal, displayorder.PriorityLow
	invalidLow, invalidHigh := -1, 4
	older := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	zero := time.Time{}

	return []Case{
		{
			Name:  "non-terminal urgent before high despite activity",
			Left:  displayorder.Item{Priority: &urgent, LastActivityAt: older, Identifier: "B"},
			Right: displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
			Want:  -1,
		},
		{
			Name:  "non-terminal high before normal despite activity",
			Left:  displayorder.Item{Priority: &high, LastActivityAt: older},
			Right: displayorder.Item{Priority: &normal, LastActivityAt: newer},
			Want:  -1,
		},
		{
			Name:  "non-terminal normal before low despite activity",
			Left:  displayorder.Item{Priority: &normal, LastActivityAt: older},
			Right: displayorder.Item{Priority: &low, LastActivityAt: newer},
			Want:  -1,
		},
		{
			Name:  "non-terminal low before missing priority despite activity",
			Left:  displayorder.Item{Priority: &low, LastActivityAt: older},
			Right: displayorder.Item{LastActivityAt: newer},
			Want:  -1,
		},
		{
			Name:  "non-terminal priority before missing activity",
			Left:  displayorder.Item{Priority: &urgent},
			Right: displayorder.Item{Priority: &low, LastActivityAt: newer},
			Want:  -1,
		},
		{
			Name:  "non-terminal equal priority uses newest activity",
			Left:  displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "B"},
			Right: displayorder.Item{Priority: &high, LastActivityAt: older, Identifier: "A"},
			Want:  -1,
		},
		{
			Name:  "non-terminal equal activity uses identifier",
			Left:  displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
			Right: displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "B"},
			Want:  -1,
		},
		{
			Name:  "non-terminal missing priorities use activity",
			Left:  displayorder.Item{LastActivityAt: newer, Identifier: "B"},
			Right: displayorder.Item{LastActivityAt: older, Identifier: "A"},
			Want:  -1,
		},
		{
			Name:  "non-terminal missing activity follows known activity",
			Left:  displayorder.Item{Priority: &high, LastActivityAt: newer},
			Right: displayorder.Item{Priority: &high},
			Want:  -1,
		},
		{
			Name:  "non-terminal missing activities use identifier",
			Left:  displayorder.Item{Identifier: "A"},
			Right: displayorder.Item{Identifier: "B"},
			Want:  -1,
		},
		{
			Name:  "non-terminal negative priority acts as none",
			Left:  displayorder.Item{Priority: &low, LastActivityAt: older},
			Right: displayorder.Item{Priority: &invalidLow, LastActivityAt: newer},
			Want:  -1,
		},
		{
			Name:  "non-terminal out of range priority acts as none",
			Left:  displayorder.Item{Priority: &invalidHigh, LastActivityAt: newer},
			Right: displayorder.Item{LastActivityAt: older},
			Want:  -1,
		},
		{
			Name:     "terminal ignores priority",
			Terminal: true,
			Left:     displayorder.Item{Priority: &urgent, LastActivityAt: older, Identifier: "A"},
			Right:    displayorder.Item{Priority: &low, LastActivityAt: newer, Identifier: "B"},
			Want:     1,
		},
		{
			Name:     "terminal equal activity uses identifier despite priority",
			Terminal: true,
			Left:     displayorder.Item{Priority: &low, LastActivityAt: newer, Identifier: "A"},
			Right:    displayorder.Item{Priority: &urgent, LastActivityAt: newer, Identifier: "B"},
			Want:     -1,
		},
		{
			Name:     "terminal missing priority still uses activity",
			Terminal: true,
			Left:     displayorder.Item{LastActivityAt: newer},
			Right:    displayorder.Item{Priority: &urgent, LastActivityAt: older},
			Want:     -1,
		},
		{
			Name:     "terminal missing activity follows known activity despite priority",
			Terminal: true,
			Left:     displayorder.Item{Priority: &urgent},
			Right:    displayorder.Item{Priority: &low, LastActivityAt: newer},
			Want:     1,
		},
		{
			Name:     "terminal missing activities use identifier despite priority",
			Terminal: true,
			Left:     displayorder.Item{Priority: &low, Identifier: "A"},
			Right:    displayorder.Item{Priority: &urgent, Identifier: "B"},
			Want:     -1,
		},
		{
			Name:  "missing activity follows activity before zero time",
			Left:  displayorder.Item{LastActivityAt: zero.Add(-time.Hour)},
			Right: displayorder.Item{},
			Want:  -1,
		},
		{
			Name:  "equal instants in different zones use identifier",
			Left:  displayorder.Item{LastActivityAt: newer, Identifier: "A"},
			Right: displayorder.Item{LastActivityAt: newer.In(time.FixedZone("offset", 3600)), Identifier: "B"},
			Want:  -1,
		},
		{
			Name:  "identical items compare equal",
			Left:  displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
			Right: displayorder.Item{Priority: &high, LastActivityAt: newer, Identifier: "A"},
		},
		{
			Name: "zero values compare equal",
		},
	}

}
