package hubclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestSettledBarrierFinish(t *testing.T) {
	t.Parallel()
	unavailable := errors.New("hub unavailable")
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "accepted", err: nil, want: nil},
		{name: "superseded barrier is settled", err: fmt.Errorf("finish: %w", &APIError{Status: http.StatusConflict, Code: "revision_conflict"}), want: nil},
		{name: "unavailable hub is retried", err: unavailable, want: unavailable},
		{name: "server error is retried", err: &APIError{Status: http.StatusBadGateway}, want: &APIError{Status: http.StatusBadGateway}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := settledBarrierFinish(test.err)
			if (got == nil) != (test.want == nil) || got != nil && got.Error() != test.want.Error() {
				t.Fatalf("settledBarrierFinish(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
