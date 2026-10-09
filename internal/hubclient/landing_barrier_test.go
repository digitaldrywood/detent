package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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

func TestIsolationProbeKeepsLastReportOnTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		good := isolation.Report{"detent/codex": {"native-trusted", "sandbox"}}
		slow := false
		s := &Scheduler{isolationReport: func(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
			if slow {
				<-ctx.Done()
				return isolation.Report{"detent/codex": {}}, nil
			}
			return good, nil
		}}
		for _, test := range []struct {
			name string
			slow bool
			want []string
		}{
			{name: "completed probe is reported", want: []string{"native-trusted", "sandbox"}},
			{name: "timed-out probe keeps the last report", slow: true, want: []string{"native-trusted", "sandbox"}},
		} {
			slow = test.slow
			report, _ := s.probeIsolation(t.Context())
			if got := report["detent/codex"]; !slices.Equal(got, test.want) {
				t.Fatalf("%s: report = %v, want %v", test.name, got, test.want)
			}
		}
	})
}
