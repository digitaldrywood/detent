package displayorder_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/displayorder"
	"github.com/digitaldrywood/detent/internal/displayorder/testfixture"
)

func TestCompare(t *testing.T) {
	t.Parallel()

	for _, tt := range testfixture.Comparisons() {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			if got := displayorder.Compare(tt.Terminal, tt.Left, tt.Right); got != tt.Want {
				t.Fatalf("Compare(left, right) = %d, want %d", got, tt.Want)
			}
			if got := displayorder.Compare(tt.Terminal, tt.Right, tt.Left); got != -tt.Want {
				t.Fatalf("Compare(right, left) = %d, want %d", got, -tt.Want)
			}
		})
	}
}
