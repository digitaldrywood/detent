package githuboutbound

import (
	"context"
	"errors"
	"testing"
)

func TestOutboxStepStopsAfterOneDeliveryOrRejection(t *testing.T) {
	failure := errors.New("delivery unavailable")
	for _, test := range []struct {
		name      string
		delivered bool
		failure   error
		canceled  bool
		github    int
	}{
		{name: "delivered", delivered: true, github: 1},
		{name: "rejected", failure: failure, github: 1},
		{name: "idle", github: 1},
		{name: "canceled", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			github := 0
			delivered, err := processOutboxStep(ctx, func(context.Context) (bool, error) {
				github++
				return test.delivered, test.failure
			})
			if github != test.github {
				t.Fatalf("delivery budget: github=%d", github)
			}
			if test.canceled {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			} else if !errors.Is(err, test.failure) || delivered != test.delivered {
				t.Fatalf("delivery outcome: delivered=%t err=%v", delivered, err)
			}
		})
	}
}
