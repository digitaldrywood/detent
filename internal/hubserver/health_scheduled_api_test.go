package hubserver

import (
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthScheduledHTTPRecordsScopeAndReopen(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Microsecond)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	service := newPostgresNativeService(t, Config{now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, runMaintenance: waitForMaintenanceFixtureShutdown})
	baseConfig := service.config
	openTenant := func(org tracker.OrganizationID) *Service {
		cfg := baseConfig
		cfg.Hosted = &HostedConfig{OrganizationID: string(org), WorkOSOrganizationID: "provider_" + string(org), PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
		opened, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = opened.Close() })
		opened.config.Hosted = nil
		opened.echo = echo.New()
		opened.echo.Use(opened.measureRequests, opened.policyCacheMiddleware, opened.changeContentMiddleware, opened.runDetailMiddleware)
		opened.registerNativeRoutes(opened.echo)
		opened.registerRunnerRoutes(opened.echo)
		opened.registerOperatorTools(opened.echo)
		return opened
	}
	services := map[tracker.OrganizationID]*Service{"org_first": service, "org_first_second": openTenant("org_first_second")}
	const repository = "example-org/example-project"
	var fixtures []nativeFixture
	var workerTokens []string
	var runnerIDs []string
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		service = services[org]
		for _, name := range []string{"First scheduled route", "Second scheduled route"} {
			f := newNativeFixture(t, service, org, name)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				record.Settings.CheckoutRepository = repository
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			runner := prepareRunner(t, f, runnerauth.Collaborate)
			runner.enroll(t)
			fixtures = append(fixtures, f)
			workerTokens = append(workerTokens, runner.redemption.Credential)
			runnerIDs = append(runnerIDs, runner.binding.RunnerID)
		}
	}
	expected := map[tracker.ProjectID][]healthstore.ScheduledReport{}
	for index, f := range fixtures {
		service = services[f.project.OrganizationID]
		path := f.base + "/work-items"
		var current tracker.CreateIssue
		for step, conclusion := range []string{"success", "failure"} {
			at := base.Add(time.Duration(6*index+step) * time.Second)
			clock.Store(at.UnixNano())
			run := strconv.Itoa(step + 1)
			report := gate.ScheduledValidationReport{Repository: repository, RunID: run, RunAttempt: "1", RunURL: "https://github.com/" + repository + "/actions/runs/" + run + "/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: conclusion}
			current = tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "report-" + run}, ScheduledValidation: &report}
			var recorded scheduledValidationRecord
			response := performHubAPIRequest(t, service, http.MethodPost, path, f.token, current)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &recorded)
			if recorded.ScheduledValidationReport != report || !recorded.RecordedAt.Equal(at) {
				t.Fatalf("HTTP report changed: %+v", recorded)
			}
			expected[f.project.ID] = append(expected[f.project.ID], healthstore.ScheduledReport{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ScheduledValidationReport: report, RecordedAt: at})
		}
		clock.Store(base.Add(time.Duration(6*index+2) * time.Second).UnixNano())
		current.IdempotencyKey = "replay-report"
		var replay scheduledValidationRecord
		response := performHubAPIRequest(t, service, http.MethodPost, path, f.token, current)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &replay)
		if replay.ScheduledValidationReport != *current.ScheduledValidation || !replay.RecordedAt.Equal(expected[f.project.ID][1].RecordedAt) {
			t.Fatalf("replay changed original report: %+v", replay)
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "", current), http.StatusUnauthorized)
		if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			allowed, err := r.RunnerProjectAuthorized(t.Context(), runnerIDs[index], f.project.ID, time.Unix(0, clock.Load()).UTC())
			if err == nil && !allowed {
				t.Fatalf("worker fixture lost current project authority: org=%s project=%s", f.project.OrganizationID, f.project.ID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		workerResponse := performHubAPIRequest(t, service, http.MethodPost, path, workerTokens[index], current)
		if workerResponse.Code != http.StatusForbidden {
			t.Fatalf("org=%s project=%s worker refusal status=%d body=%s", f.project.OrganizationID, f.project.ID, workerResponse.Code, workerResponse.Body.String())
		}

		foreign := fixtures[(index+2)%len(fixtures)]
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, foreign.token, current), http.StatusUnauthorized)
		for _, selected := range fixtures {
			if selected.project.OrganizationID == f.project.OrganizationID && selected.project.ID != f.project.ID {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, selected.token, current), http.StatusForbidden)
			}
		}
		for _, test := range []struct {
			name   string
			mutate func(*tracker.CreateIssue)
			status int
		}{
			{"changed run", func(r *tracker.CreateIssue) { r.ScheduledValidation.HeadSHA = strings.Repeat("b", 40) }, http.StatusConflict},
			{"invalid identity", func(r *tracker.CreateIssue) { r.ScheduledValidation.HeadSHA = "invalid" }, http.StatusUnprocessableEntity},
			{"other repository", func(r *tracker.CreateIssue) {
				r.ScheduledValidation.Repository = "other-org/other-project"
				r.ScheduledValidation.RunURL = "https://github.com/other-org/other-project/actions/runs/2/attempts/1"
			}, http.StatusUnprocessableEntity},
			{"mixed body", func(r *tracker.CreateIssue) { r.Body = "Unexpected issue body" }, http.StatusUnprocessableEntity},
			{"mixed failure", func(r *tracker.CreateIssue) { r.ScheduledFailure = &tracker.ScheduledFailure{} }, http.StatusUnprocessableEntity},
			{"mixed challenge", func(r *tracker.CreateIssue) { r.ChallengeToken = "example-challenge" }, http.StatusUnprocessableEntity},
			{"mixed duplicate decision", func(r *tracker.CreateIssue) { r.NotDuplicateOf = []int{1} }, http.StatusUnprocessableEntity},
		} {
			request := current
			report := *current.ScheduledValidation
			request.ScheduledValidation = &report
			request.IdempotencyKey = test.name
			test.mutate(&request)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, f.token, request), test.status)
		}
	}
	for org, active := range services {
		if err := active.Close(); err != nil {
			t.Fatal(err)
		}
		services[org] = openTenant(org)
	}
	for _, f := range fixtures {
		service = services[f.project.OrganizationID]
		if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
			history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), f.project.ID, base.Add(24*time.Hour))
			if err != nil {
				return err
			}
			want := slices.Clone(expected[f.project.ID])
			slices.Reverse(want)
			if !reflect.DeepEqual(history.Reports, want) || !history.FirstReport.Equal(want[1].RecordedAt) || !history.LastGreen.Equal(want[1].RecordedAt) {
				t.Fatalf("persisted reports changed: got=%+v want=%+v", history, want)
			}
			items, err := r.WorkItems(t.Context(), f.project.ID, 0, 1)
			if len(items) != 0 {
				t.Fatal("scheduled reports created ordinary work items")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		current := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reopen-report"}, ScheduledValidation: &expected[f.project.ID][1].ScheduledValidationReport}
		var retained scheduledValidationRecord
		response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, current)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &retained)
		if retained.ScheduledValidationReport != *current.ScheduledValidation || !retained.RecordedAt.Equal(expected[f.project.ID][1].RecordedAt) {
			t.Fatalf("reopen changed report receipt: %+v", retained)
		}
	}
}
