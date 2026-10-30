package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledValidationDeliveryHealth(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := base
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	f := newNativeFixture(t, service, "", "scheduled-health")
	other := newNativeFixture(t, service, f.project.OrganizationID, "other-scheduled-health")
	for _, fixture := range []nativeFixture{f, other} {
		if _, err := service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository='example-org/example-project' WHERE id=?", fixture.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	report := gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1", RunAttempt: "1", HeadSHA: strings.Repeat("a", 40), Conclusion: "success"}
	type healthStep struct {
		name       string
		hours      int
		run        string
		conclusion string
		signal     string
		greenHour  int
	}
	steps := []healthStep{
		{name: "never reported"},
		{name: "first green", run: "1", conclusion: "success"},
		{name: "observed hourly cadence", hours: 1, run: "2", conclusion: "success"},
		{name: "last green before gap", hours: 2, run: "3", conclusion: "success"},
		{name: "before silent window", hours: 7},
		{name: "silent window crossed", hours: 8, signal: "scheduled_silent"},
		{name: "fourteen hour gap", hours: 16, signal: "scheduled_silent"},
		{name: "green clears gap", hours: 16, run: "4", conclusion: "success"},
		{name: "failures keep reporting", hours: 17, run: "5", conclusion: "failure"},
		{name: "before red threshold", hours: 39, run: "6", conclusion: "failure"},
		{name: "red for twenty-four hours", hours: 40, run: "7", conclusion: "failure", signal: "scheduled_red"},
		{name: "green clears red", hours: 41, run: "8", conclusion: "success"},
	}
	for hour := 42; hour <= 87; hour++ {
		conclusion, signal := "skipped", ""
		if hour == 64 || hour == 65 {
			conclusion = "failure"
		}
		if hour >= 65 {
			signal = "scheduled_red"
		}
		steps = append(steps, healthStep{fmt.Sprintf("unchanged commit hour %d", hour), hour, strconv.Itoa(hour), conclusion, signal, 41})
	}
	steps = append(steps, healthStep{"green clears red after skipped reports", 88, "88", "success", "", 88})
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			now = base.Add(time.Duration(step.hours) * time.Hour)
			if step.run != "" {
				report.RunID, report.Conclusion = step.run, step.conclusion
				report.RunURL = "https://github.com/" + report.Repository + "/actions/runs/" + report.RunID + "/attempts/1"
				for _, fixture := range []nativeFixture{f, other} {
					request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("report-%s-%s", fixture.project.ID, step.run)}, ScheduledValidation: &report}
					response := performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/work-items", fixture.token, request)
					requireNativeStatus(t, response, http.StatusOK)
					var record scheduledValidationRecord
					decodeHubResponse(t, response, &record)
					if record.ScheduledValidationReport != report || !record.RecordedAt.Equal(now) {
						t.Fatalf("record = %+v", record)
					}
					request.IdempotencyKey += "-replay"
					response = performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/work-items", fixture.token, request)
					decodeHubResponse(t, response, &record)
					if !record.RecordedAt.Equal(now) {
						t.Fatalf("replay changed report time: %+v", record)
					}
				}
			}
			if step.conclusion == "skipped" {
				scheduled, err := readHealthScheduled(t.Context(), service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now)
				if err != nil {
					t.Fatal(err)
				}
				if !scheduled.LastReport.Equal(now) || !scheduled.LastGreen.Equal(base.Add(41*time.Hour)) {
					t.Fatalf("skipped report changed validation evidence: %+v", scheduled)
				}
			}
			if step.name == "before silent window" {
				request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "late-replay"}, ScheduledValidation: &report}
				response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
				requireNativeStatus(t, response, http.StatusOK)
				var record scheduledValidationRecord
				decodeHubResponse(t, response, &record)
				if !record.RecordedAt.Equal(base.Add(2 * time.Hour)) {
					t.Fatalf("late replay refreshed reporting: %+v", record)
				}
			}
			if err := service.evaluateOrganizationHealth(t.Context(), f.project.OrganizationID, now); err != nil {
				t.Fatal(err)
			}
			for _, fixture := range []nativeFixture{f, other} {
				response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/health/findings", fixture.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page hubhealth.FindingsPage
				decodeHubResponse(t, response, &page)
				if step.signal == "" {
					if len(page.Items) != 0 {
						t.Fatalf("unexpected finding: %+v", page.Items)
					}
					continue
				}
				if len(page.Items) != 1 || page.Items[0].Signal != "dead_man" || page.Items[0].Subject.ID != string(fixture.project.ID) || page.Items[0].Class != "flow" || page.Items[0].Evidence.Counts[step.signal] != 1 {
					t.Fatalf("finding = %+v", page.Items)
				}
				evidence := page.Items[0].Evidence.ScheduledValidation
				if evidence == nil || evidence.LastRunURL != report.RunURL || evidence.LastGreenAt == nil || evidence.WindowSeconds != 21600 || evidence.CadenceSeconds != 3600 {
					t.Fatalf("evidence = %+v", evidence)
				}
				if step.signal == "scheduled_silent" && (!evidence.LastReportAt.Equal(base.Add(2*time.Hour)) || !strings.Contains(page.Items[0].Summary, "2026-10-01T02:00:00Z")) {
					t.Fatalf("silent evidence = %+v", page.Items[0])
				}
				greenHour := step.greenHour
				if greenHour == 0 {
					greenHour = 16
				}
				if step.signal == "scheduled_red" && (!evidence.LastGreenAt.Equal(base.Add(time.Duration(greenHour)*time.Hour)) || !strings.Contains(page.Items[0].Summary, base.Add(time.Duration(greenHour)*time.Hour).Format(time.RFC3339))) {
					t.Fatalf("red evidence = %+v", page.Items[0])
				}
			}
		})
	}
	var issues, comments, records int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM issues), (SELECT count(*) FROM native_comments), (SELECT count(*) FROM scheduled_validation_reports)").Scan(&issues, &comments, &records); err != nil {
		t.Fatal(err)
	}
	if issues != 0 || comments != 0 || records != 110 {
		t.Fatalf("issues=%d comments=%d reports=%d", issues, comments, records)
	}
	for _, test := range []struct {
		name string
		edit func(*tracker.CreateIssue)
	}{
		{name: "foreign repository", edit: func(r *tracker.CreateIssue) {
			r.ScheduledValidation.Repository = "owner/repository"
			r.ScheduledValidation.RunURL = "https://github.com/owner/repository/actions/runs/8/attempts/1"
		}},
		{name: "mixed issue and report", edit: func(r *tracker.CreateIssue) { r.Title = "Unexpected issue" }},
		{name: "invalid run identity", edit: func(r *tracker.CreateIssue) { r.ScheduledValidation.RunID = "invalid" }},
		{name: "changed conclusion", edit: func(r *tracker.CreateIssue) { r.ScheduledValidation.Conclusion = "failure" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copyReport := report
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ScheduledValidation: &copyReport}
			test.edit(&request)
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		})
	}
	response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/health/findings?state=resolved", f.token, nil)
	var page hubhealth.FindingsPage
	decodeHubResponse(t, response, &page)
	if len(page.Items) != 3 || !slices.ContainsFunc(page.Items, func(f hubhealth.Finding) bool { return f.ResolvedAt != nil && f.ResolvedAt.Equal(now) }) {
		t.Fatalf("resolved findings = %+v", page.Items)
	}
}

func TestHealthScheduledReporterHistory(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := base
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	f := newNativeFixture(t, service, "", "scheduled-history")
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository='example-org/example-project' WHERE id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	item := f.create(t, "existing-report")
	var lastComment tracker.NativeComment
	for _, step := range []struct {
		name       string
		hours      int
		run        string
		green      bool
		wantLast   time.Duration
		wantGreen  time.Duration
		wantSilent bool
	}{
		{name: "first failure", run: "1", wantGreen: -1},
		{name: "next failure", hours: 1, run: "2", wantLast: time.Hour, wantGreen: -1},
		{name: "second failing job of same run", hours: 2, run: "2", wantLast: time.Hour, wantGreen: -1},
		{name: "legacy green comment", hours: 2, run: "3", green: true, wantLast: 2 * time.Hour, wantGreen: 2 * time.Hour},
		{name: "legacy silence", hours: 8, wantLast: 2 * time.Hour, wantGreen: 2 * time.Hour, wantSilent: true},
		{name: "legacy green clears", hours: 8, run: "4", green: true, wantLast: 8 * time.Hour, wantGreen: 8 * time.Hour},
	} {
		t.Run(step.name, func(t *testing.T) {
			now = base.Add(time.Duration(step.hours) * time.Hour)
			if step.run != "" {
				runURL := "https://github.com/example-org/example-project/actions/runs/" + step.run + "/attempts/1"
				evidence := gate.ScheduledEvidence{Schema: 1, Repository: "example-org/example-project", OccurrenceKey: strings.Repeat("a", 64), RunID: step.run, RunAttempt: "1", JobID: "1", HeadSHA: strings.Repeat("a", 40), Conclusion: "failure", RunURL: runURL}
				body := issueorigin.Stamp("Recorded failure", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: strings.TrimSuffix(runURL, "/attempts/1"), Fingerprint: "scheduled-job"}) + evidence.Stamp()
				if step.green {
					body = "Scheduled full validation passed for development commit `" + strings.Repeat("a", 40) + "`.\n\nRun: " + runURL + "\n\nThis is scheduled validation evidence only."
				}
				request := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: step.name}, Body: body}
				response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/comments", f.token, request)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &lastComment)
			}
			if step.name == "legacy silence" {
				body := lastComment.Body + "\nRecorded evidence retained."
				request := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-old-report"}, ExpectedRevision: lastComment.Revision, Body: body}
				response := performHubAPIRequest(t, service, http.MethodPatch, f.base+"/work-items/"+string(item.WorkItemID)+"/comments/"+lastComment.ID, f.token, request)
				requireNativeStatus(t, response, http.StatusOK)
			}
			scheduled, err := readHealthScheduled(t.Context(), service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now)
			if err != nil {
				t.Fatal(err)
			}
			if !scheduled.LastReport.Equal(base.Add(step.wantLast)) || (step.wantGreen < 0 && !scheduled.LastGreen.IsZero()) || (step.wantGreen >= 0 && !scheduled.LastGreen.Equal(base.Add(step.wantGreen))) {
				t.Fatalf("scheduled history = %+v", scheduled)
			}
			finding, active := hubhealth.DeadManFinding(now, hubhealth.Project{ID: string(f.project.ID), Scheduled: scheduled})
			if active != step.wantSilent || active && finding.Evidence.Counts["scheduled_silent"] != 1 {
				t.Fatalf("active=%v finding=%+v", active, finding)
			}
			for _, phase := range []struct {
				name, query string
				excluded    bool
			}{
				{"unknown actor", `UPDATE native_comments SET actor_kind=NULL WHERE project_id=?`, false},
				{"typed actor", `UPDATE native_comments SET actor_json='{"kind":"runner"}' WHERE project_id=?`, false},
				{"runner excluded", `UPDATE native_comments SET actor_kind='runner' WHERE project_id=?`, true},
			} {
				tx, err := service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), phase.query, f.project.ID); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				observed, readErr := readHealthScheduled(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now)
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				want := scheduled
				if phase.excluded {
					want = hubhealth.ScheduledValidation{}
				}
				if readErr != nil || !reflect.DeepEqual(observed, want) {
					t.Fatalf("%s changed scheduled comments: got=%+v want=%+v err=%v", phase.name, observed, want, readErr)
				}
			}
		})
	}
	var failureBody, failureAt string
	if err := service.database.reader.QueryRowContext(t.Context(), `SELECT body,created_at FROM native_comments WHERE project_id=? AND instr(body,'detent-checks')>0 ORDER BY created_at LIMIT 1`, f.project.ID).Scan(&failureBody, &failureAt); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []struct {
		name, kind, raw string
		want            bool
	}{
		{"typed issue actor", "human", `{"kind":"runner"}`, true},
		{"runner issue excluded", "runner", `{"kind":"human"}`, false},
		{"unknown human issue actor", "", `{"kind":"human"}`, true},
		{"unknown runner issue actor", "", `{"kind":"runner"}`, false},
	} {
		t.Run(phase.name, func(t *testing.T) {
			tx, err := service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), `DELETE FROM native_comments WHERE project_id=?`, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET body=?,created_at=?,actor_kind=nullif(?,''),actor_json=? WHERE native_id=?`, failureBody, failureAt, phase.kind, phase.raw, item.WorkItemID); err != nil {
				t.Fatal(err)
			}
			scheduled, err := readHealthScheduled(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now)
			if err != nil || (!scheduled.LastReport.IsZero()) != phase.want || phase.want && formatHubTime(scheduled.LastReport) != failureAt {
				t.Fatalf("issue actor filter: got=%+v wantReport=%v err=%v", scheduled, phase.want, err)
			}
		})
	}
}
