package hubserver

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
			finding, active := deadManFinding(now, healthProject{ID: string(f.project.ID), Scheduled: scheduled})
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
					want = healthScheduledValidation{}
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
