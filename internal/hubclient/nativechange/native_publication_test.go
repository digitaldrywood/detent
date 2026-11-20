package nativechange

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDetachedPublicationStopsOnLeaseLoss(t *testing.T) {
	for _, route := range []string{"phases", "progress"} {
		for _, scenario := range []string{"renewal rejected", "revoked"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					lost := errors.New(scenario)
					calls := 0
					client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response := httptest.NewRecorder()
						switch {
						case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/validate"):
							io.WriteString(response, "{}")
						case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/"+route):
							calls++
							if calls == 1 {
								response.Header().Set("Retry-After", "60")
								response.WriteHeader(http.StatusTooManyRequests)
								io.WriteString(response, `{"code":"runner_request_budget"}`)
							} else {
								response.WriteHeader(http.StatusBadRequest)
							}
						default:
							t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
						}
						return response.Result(), nil
					})}})
					if err != nil {
						t.Fatal(err)
					}
					native, err := client.Native("org_example", "prj_example")
					if err != nil {
						t.Fatal(err)
					}
					scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, HeartbeatInterval: time.Second, LeaseTTL: time.Hour, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
					if err != nil {
						t.Fatal(err)
					}
					descriptor := clientTestPolicy()
					lease := tracker.NativeLease{WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}
					deadline := start.Add(time.Hour)
					if scenario == "renewal rejected" {
						deadline = start.Add(15 * time.Second)
					}
					fixtureInstallClaim(t, scheduler, nativeClaim{source: fixtureConnector(native), lease: lease, deadline: deadline}, descriptor, "project")
					host := publicationRenewalHost{ExecutionHost: scheduler, err: lost}
					execution := NewExecution(host, executioncontract.ExecutionClaim{Source: fixtureConnector(native), Lease: lease}).(*nativeExecution)
					guarded, stop, err := execution.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer stop()
					if scenario == "revoked" {
						timer := time.AfterFunc(10*time.Second, func() { execution.Cancel(errors.Join(runner.ErrExecutionAuthorityUnavailable, lost)) })
						defer timer.Stop()
					}
					finalization, cancel := context.WithTimeout(context.WithoutCancel(guarded), 3*time.Minute)
					defer cancel()
					if route == "phases" {
						err = execution.RecordPhaseReceipt(finalization, "project_gate", "branch", "head", "tree", "gate")
					} else {
						err = execution.RecordRoundVerdict(finalization, progresscheck.RoundInput{RoundID: "round"})
					}
					if !errors.Is(err, lost) || !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || calls != 1 || time.Since(start) != 10*time.Second || finalization.Err() != nil {
						t.Fatalf("publication returned %v after %s and %d POSTs, finalization error = %v", err, time.Since(start), calls, finalization.Err())
					}
				})
			})
		}
	}
}

type publicationRenewalHost struct {
	executioncontract.ExecutionHost
	err error
}

func (h publicationRenewalHost) RenewExecution(context.Context, tracker.NativeLease, int) error {
	return h.err
}
