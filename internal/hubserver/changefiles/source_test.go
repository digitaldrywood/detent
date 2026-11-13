package changefiles_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestChangeSourceVerificationSlot(t *testing.T) {
	t.Parallel()
	input := hubtest.ChangeInput(hubtest.PolicyDescriptor().ID)
	bundle := sourcetest.Source(t, &input, bytes.Repeat([]byte("retained source\n"), 100000))
	slot := make(chan struct{}, 1)
	t.Run("occupied verification slot honors cancellation", func(t *testing.T) {
		slot <- struct{}{}
		defer func() { <-slot }()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := changefiles.VerifySource(ctx, slot, *input.Source, bundle); !errors.Is(err, context.Canceled) {
			t.Fatalf("occupied verification slot ignored cancellation: %v", err)
		}
	})
}
