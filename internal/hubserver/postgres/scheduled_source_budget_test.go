package postgres

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func TestScheduledSourcePageSeparatesOversizeFromRemainingBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                    string
		compressed, content, remainingContent, remainingArchive int64
		fits, invalid                                           bool
	}{
		{name: "maximum legal archive", compressed: 32 << 20, content: 32 << 20, remainingContent: 32 << 20, remainingArchive: 64 << 20, fits: true},
		{name: "decoded allowance is not archive allowance", compressed: 1, content: 32 << 20, remainingContent: 32 << 20, remainingArchive: 32 << 20},
		{name: "page content exhausted", compressed: 1, content: 2, remainingContent: 1, remainingArchive: 64 << 20},
		{name: "page archive exhausted", compressed: 2, content: 1, remainingContent: 32 << 20, remainingArchive: 2},
		{name: "single content oversized", compressed: 1, content: (32 << 20) + 1, remainingContent: 32 << 20, remainingArchive: 64 << 20, invalid: true},
		{name: "single compressed oversized", compressed: (32 << 20) + 1, content: 1, remainingContent: 32 << 20, remainingArchive: 64 << 20, invalid: true},
		{name: "invalid empty object", compressed: 0, content: 1, remainingContent: 32 << 20, remainingArchive: 64 << 20, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fits, err := scheduledSourceFitsPage(test.compressed, test.content, test.remainingContent, test.remainingArchive)
			if fits != test.fits || errors.Is(err, artifact.ErrIntegrity) != test.invalid {
				t.Fatalf("source budget fits=%v error=%v", fits, err)
			}
		})
	}
}
