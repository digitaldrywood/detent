package hubserver

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthprojection"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthScheduledReporterHistory(t *testing.T) {
	t.Run("typed scheduled report command", runScheduledValidationRecordCases)
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	service := newPostgresNativeService(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, deps: hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}}})
	f := newNativeFixture(t, service, "", "scheduled-history")
	if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.CheckoutRepository = "example-org/example-project"
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	item := f.create(t, "existing-report")
	read := func(t *testing.T, now time.Time) healthstore.ScheduledHistory {
		t.Helper()
		var history healthstore.ScheduledHistory
		if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			history, err = r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), f.project.ID, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return history
	}
	var lastComment tracker.NativeComment
	for _, step := range []struct {
		name                string
		hours               int
		run                 string
		green               bool
		wantLast, wantGreen time.Duration
		wantSilent          bool
	}{
		{name: "first failure", run: "1", wantGreen: -1},
		{name: "next failure", hours: 1, run: "2", wantLast: time.Hour, wantGreen: -1},
		{name: "second failing job of same run", hours: 2, run: "2", wantLast: time.Hour, wantGreen: -1},
		{name: "green report", hours: 2, run: "3", green: true, wantLast: 2 * time.Hour, wantGreen: 2 * time.Hour},
		{name: "silence after ordinary comment edit", hours: 8, wantLast: 2 * time.Hour, wantGreen: 2 * time.Hour, wantSilent: true},
		{name: "green clears", hours: 8, run: "4", green: true, wantLast: 8 * time.Hour, wantGreen: 8 * time.Hour},
	} {
		t.Run(step.name, func(t *testing.T) {
			now := base.Add(time.Duration(step.hours) * time.Hour)
			clock.Store(now.UnixNano())
			before := read(t, now)
			if step.run != "" {
				runURL := "https://github.com/example-org/example-project/actions/runs/" + step.run + "/attempts/1"
				evidence := gate.ScheduledEvidence{Schema: 1, Repository: "example-org/example-project", OccurrenceKey: strings.Repeat("a", 64), RunID: step.run, RunAttempt: "1", JobID: "1", HeadSHA: strings.Repeat("a", 40), Conclusion: "failure", RunURL: runURL}
				body := issueorigin.Stamp("Recorded failure", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: strings.TrimSuffix(runURL, "/attempts/1"), Fingerprint: "scheduled-job"}) + evidence.Stamp()
				conclusion := "failure"
				if step.green {
					conclusion = "success"
					body = "Scheduled full validation passed for development commit `" + strings.Repeat("a", 40) + "`.\n\nRun: " + runURL + "\n\nThis is scheduled validation evidence only."
				}
				comment := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: step.name}, Body: body})
				requireNativeStatus(t, comment, http.StatusOK)
				decodeHubResponse(t, comment, &lastComment)
				if observed := read(t, now); !reflect.DeepEqual(observed, before) {
					t.Fatalf("ordinary discussion changed authoritative scheduled reports: got=%+v want=%+v", observed, before)
				}
				report := gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: step.run, RunAttempt: "1", RunURL: runURL, HeadSHA: strings.Repeat("a", 40), Conclusion: conclusion}
				request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: step.name}, ScheduledValidation: &report}
				runner := prepareRunner(t, f, runnerauth.Collaborate)
				runner.enroll(t)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", runner.redemption.Credential, request), http.StatusForbidden)
				if observed := read(t, now); !reflect.DeepEqual(observed, before) {
					t.Fatalf("runner report changed typed history: got=%+v want=%+v", observed, before)
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusOK)
			} else {
				body := lastComment.Body + "\nRecorded evidence retained."
				response := performHubAPIRequest(t, service, http.MethodPatch, f.base+"/work-items/"+string(item.WorkItemID)+"/comments/"+lastComment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-old-report"}, ExpectedRevision: lastComment.Revision, Body: body})
				requireNativeStatus(t, response, http.StatusOK)
				if observed := read(t, now); !reflect.DeepEqual(observed, before) {
					t.Fatalf("editing an old comment refreshed scheduled evidence: got=%+v want=%+v", observed, before)
				}
			}
			scheduled := healthprojection.ScheduledHistory(read(t, now))
			if !scheduled.LastReport.Equal(base.Add(step.wantLast)) || step.wantGreen < 0 && !scheduled.LastGreen.IsZero() || step.wantGreen >= 0 && !scheduled.LastGreen.Equal(base.Add(step.wantGreen)) {
				t.Fatalf("scheduled history=%+v", scheduled)
			}
			finding, active := healthsignals.DeadManFinding(now, healthProject{ID: string(f.project.ID), Scheduled: scheduled})
			if active != step.wantSilent || active && finding.Evidence.Counts["scheduled_silent"] != 1 {
				t.Fatalf("active=%v finding=%+v", active, finding)
			}
		})
	}
}

func runScheduledValidationRecordCases(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	service := newPostgresNativeService(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, deps: hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}}})
	foreignToken := ""
	for index, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		orgBase := base.Add(time.Duration(index) * 4 * time.Minute)
		clock.Store(orgBase.UnixNano())
		f := newNativeFixture(t, service, org, "Scheduled report")
		if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), f.project.ID)
			if err != nil {
				return err
			}
			project.Settings.CheckoutRepository = "example-org/example-project"
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		credential, _, err := service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(f.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		runner := prepareRunner(t, f, runnerauth.Collaborate)
		runner.enroll(t)
		for step, conclusion := range []string{"failure", "success"} {
			now := orgBase.Add(time.Duration(step) * time.Minute)
			clock.Store(now.UnixNano())
			run := string(rune('1' + step))
			report := gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: run, RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/" + run + "/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: conclusion}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "scheduled-" + run}, ScheduledValidation: &report}
			if foreignToken != "" {
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", foreignToken, request), http.StatusUnauthorized)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				allowed, err := r.RunnerProjectAuthorized(t.Context(), runner.binding.RunnerID, f.project.ID, now)
				if err == nil && !allowed {
					t.Fatal("worker fixture lacks current project authority")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", runner.redemption.Credential, request), http.StatusForbidden)
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var recorded scheduledValidationRecord
			decodeHubResponse(t, response, &recorded)
			if recorded.ScheduledValidationReport != report || !recorded.RecordedAt.Equal(now) {
				t.Fatalf("typed report changed: %+v", recorded)
			}
			now = now.Add(time.Second)
			clock.Store(now.UnixNano())
			replay := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("immutable command response changed on replay")
			}
			request.IdempotencyKey = "same-run-" + run
			repeated := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, repeated, http.StatusOK)
			if repeated.Body.String() != response.Body.String() {
				t.Fatal("same run changed first report time")
			}
			changed := report
			changed.HeadSHA = strings.Repeat("b", 40)
			request.ScheduledValidation = &changed
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusConflict)
			request.IdempotencyKey = "changed-run-" + run
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusUnprocessableEntity)
			request.ScheduledValidation = &report
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", "", request), http.StatusUnauthorized)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteAccessTokenGrant(t.Context(), credential.ID, f.project.ID)
			}); err != nil {
				t.Fatal(err)
			}
			request.IdempotencyKey = "scheduled-" + run
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusNotFound)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: f.project.ID})
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), f.project.ID, orgBase.Add(24*time.Hour))
			if err != nil {
				return err
			}
			if len(history.Reports) != 2 || !history.FirstReport.Equal(orgBase) || !history.LastGreen.Equal(orgBase.Add(time.Minute)) || history.Reports[0].Conclusion != "success" {
				t.Fatalf("typed history changed: %+v", history)
			}
			items, err := r.WorkItems(t.Context(), f.project.ID, 0, 1)
			if err == nil && len(items) != 0 {
				t.Fatal("scheduled report created an ordinary issue")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "invalid-repository"}, ScheduledValidation: &gate.ScheduledValidationReport{Repository: "other-org/other-project", RunID: "3", RunAttempt: "1", RunURL: "https://github.com/other-org/other-project/actions/runs/3/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: "success"}}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusUnprocessableEntity)
		if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(healthstore.ScheduledReportReader).HealthScheduledReport(t.Context(), f.project.ID, "other-org/other-project", "3", "1")
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("refused repository wrote report: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		foreignToken = f.token
	}
}
