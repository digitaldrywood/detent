package nativechange

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
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
			func() {
				setup.Requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
				}))
				defer func() { setup.Requests.Store(original) }()
				if err := setup.Scheduler.ReleaseClaim(t.Context(), setup.Issue.ID, "completed"); !nativeTransportUnavailable(err) {
					t.Fatalf("completion outage = %v", err)
				}
				now = now.Add(2 * time.Minute)
				scenariotest.SetSchedulerNow(setup.Scheduler, now)
				setup.Requests.Store(original)
			}()
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
