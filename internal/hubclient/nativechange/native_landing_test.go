package nativechange

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeExecutionLandsReviewedVersion(t *testing.T) {
	t.Parallel()
	t.Run("published passing source lands after completion outage", func(t *testing.T) {
		t.Parallel()
		scenariotest.RunLandingScenario(t, scenariotest.Constructors{New: NewExecution, Recovered: NewRecoveredExecution}, "completion_outage", func(t *testing.T, setup *scenariotest.LandingSetup) {

			setup.DropVersionReply()
			if err := setup.Execution.(runner.CompletionExecution).PrepareFinish(setup.Context, "succeeded", "", nil); err != nil {
				t.Fatal(err)
			}
			published := setup.Execution.(runner.ChangeExecution).NativeChange()
			if published == nil || published.ChangeID == "" || published.Error == "" {
				t.Fatalf("publication acknowledgment was not lost: %+v", published)
			}
			detail, err := setup.Admin.Change(t.Context(), tracker.NativeWorkItemID(setup.Issue.ID), published.ChangeID)
			if err != nil || len(detail.Versions) != 1 || detail.Versions[0].HeadSHA != setup.Head || !reflect.DeepEqual(detail.Versions[0].Validation, &setup.Validation) {
				t.Fatalf("outage requires published source and passing gate: %+v, %v", detail, err)
			}
			setup.Stop()
			setup.Context = t.Context()
			execution := scenariotest.Underlying(setup.Execution).(*nativeExecution)
			execution.data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: time.Now().Add(-11 * time.Minute)}
			now := time.Now()
			scenariotest.SetSchedulerNow(setup.Scheduler, now)
			original := setup.Requests.Load()
			synctest.Test(t, func(t *testing.T) {
				setup.Requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
				}))
				defer func() { setup.Requests.Store(original) }()
				if err := setup.Scheduler.ReleaseClaim(t.Context(), setup.Issue.ID, "completed"); !nativeTransportUnavailable(err) {
					t.Fatalf("completion outage = %v", err)
				}
				time.Sleep(2 * time.Minute)
				now = now.Add(2 * time.Minute)
				scenariotest.SetSchedulerNow(setup.Scheduler, now)
				setup.Requests.Store(original)
			})
			if _, err := setup.Scheduler.RenewClaim(t.Context(), setup.Issue.ID, now); err != nil {
				t.Fatalf("outage discarded published completion: %v", err)
			}
			scenariotest.SetSchedulerNow(setup.Scheduler, time.Time{})

		})
	})
}

func (h *nativeChangeHub) candidatesIn(t *testing.T, states ...string) []connector.Issue {
	return scenariotest.CandidatesIn(t, h.scheduler, h.descriptor, states...)
}

func TestStartLandingRefusesNonMergeAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		identity    *tracker.NativeExecutionIdentity
		reservation *providercapacity.Reservation
	}{
		{name: "started code execution", identity: &tracker.NativeExecutionIdentity{Role: runner.RoleCode}},
		{name: "reserved code execution", reservation: &providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: runner.RoleCode}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			execution := &nativeExecution{role: runner.RoleCode, data: tracker.NativeRunData{Identity: test.identity}}
			execution.claim.lease.ProviderReservation = test.reservation
			err := execution.StartLanding(t.Context(), 1, 1)
			var refusal *workspace.LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != "" {
				t.Fatalf("wrong role must use existing landing wait: %v", err)
			}
			if execution.role != runner.RoleCode || execution.data.Identity != test.identity || execution.data.Runtime != nil {
				t.Fatal("wrong role changed execution authority or started landing")
			}
		})
	}
}
