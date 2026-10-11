package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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

func TestNextLandingBarrierRerunsRedUnchangedHead(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("e", 40)
	for _, test := range []struct {
		name      string
		exitCode  int
		wantStart bool
	}{
		{name: "green unchanged head is not rerun", exitCode: 0},
		{name: "red unchanged head is rerun", exitCode: 2, wantStart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			starts := 0
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				barrier := tracker.LandingBarrier{ProjectID: "prj_test", Repository: "acme/repo", BaseRef: "develop", Result: &gate.CommandResult{HeadSHA: head, ExitCode: test.exitCode}}
				if request.Method == http.MethodPost {
					var mutation tracker.LandingBarrierRequest
					if err := json.NewDecoder(request.Body).Decode(&mutation); err != nil {
						return nil, err
					}
					starts++
					barrier.ID, barrier.Running = mutation.IdempotencyKey, true
				}
				encoded, err := json.Marshal(barrier)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded)), Request: request}, nil
			})
			client, err := New(Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			scheduler.nativeProjects["project"] = &NativeConnector{client: native}
			_, started, err := scheduler.NextLandingBarrier(t.Context(), "project", "acme/repo", "policy", false, func(context.Context, string) (string, error) { return head, nil })
			if err != nil {
				t.Fatal(err)
			}
			if started != test.wantStart || (starts == 1) != test.wantStart {
				t.Fatalf("started=%v starts=%d, want %v", started, starts, test.wantStart)
			}
		})
	}
}
