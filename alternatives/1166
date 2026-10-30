package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestActivityChangeWritersCompleteLegacyProjection(t *testing.T) {
	for _, operation := range []string{"publish", "land"} {
		t.Run(operation, func(t *testing.T) {
			created := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
			now := created
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			plan := explainQueryPlan(t, f.service.database.reader, activityLatestChangeQuery, []any{f.project.OrganizationID, f.project.ID, f.issue.WorkItemID})
			if violations := hotReadPlanViolations(activityLatestChangeQuery, plan); len(violations) != 0 {
				t.Fatalf("latest Change lookup scans or sorts history: %v", plan)
			}
			input := changeTestInput()
			input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/example-org/example-project/pull/1"}
			publish := func() tracker.ChangeVersion {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "activity-version"}, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				var version tracker.ChangeVersion
				decodeHubResponse(t, response, &version)
				return version
			}
			var version tracker.ChangeVersion
			if operation == "land" {
				version = publish()
				path := f.path + "/versions/" + version.ID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "activity-approval"}, Decision: "approved"}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, changeTestResult(version)), http.StatusOK)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE change_requests SET activity_created_at=NULL,activity_current_version_id=NULL WHERE id=?`, f.change.ID); err != nil {
				t.Fatal(err)
			}
			now = created.Add(2 * time.Minute)
			if operation == "publish" {
				version = publish()
			} else {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version.ID+"/landing", f.token, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "activity-landing"}, MergeSHA: input.HeadSHA, BaseRef: "develop", Method: "squash"})
				requireNativeStatus(t, response, http.StatusOK)
			}
			read := func(t *testing.T, wantID, wantURL string) {
				t.Helper()
				a := operatortool.ActivityAttempt{ProjectID: string(f.project.ID), WorkItemID: string(f.issue.WorkItemID)}
				if err := readActivityChangeLink(t.Context(), f.service.database.reader, string(f.project.OrganizationID), &a); err != nil {
					t.Fatal(err)
				}
				if a.ChangeID != wantID || a.PullRequestURL != wantURL {
					t.Fatalf("activity change=%q url=%q, want change=%q url=%q", a.ChangeID, a.PullRequestURL, wantID, wantURL)
				}
			}
			for _, phase := range []string{"before maintenance", "after maintenance"} {
				t.Run(phase, func(t *testing.T) {
					if phase == "after maintenance" {
						if err := f.service.convertActivityProjections(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					var correctTime bool
					var current, landed string
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT activity_created_at=julianday(?),activity_current_version_id,landed_version_id FROM change_requests WHERE id=?`, formatHubTime(created), f.change.ID).Scan(&correctTime, &current, &landed); err != nil {
						t.Fatal(err)
					}
					if !correctTime || current != version.ID {
						t.Fatalf("projection preserves original creation time=%v current version=%q, want %q", correctTime, current, version.ID)
					}
					if operation == "land" && landed != version.ID || operation == "publish" && landed != "" {
						t.Fatalf("%s projection has landed version %q for current version %q", operation, landed, version.ID)
					}
					read(t, f.change.ID, input.External.URL)
				})
			}
			now = created.Add(time.Minute)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "newer-change"}, Title: "Newer example change"})
			requireNativeStatus(t, response, http.StatusOK)
			var newer tracker.ChangeRequest
			decodeHubResponse(t, response, &newer)
			read(t, newer.ID, "")
		})
	}
}

func TestActivityProjectionCompatibility(t *testing.T) {
	first := newNativeFixture(t, nil, "", "activity-first")
	if err := first.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	second := newNativeFixture(t, first.service, first.project.OrganizationID, "activity-second")
	now := first.service.config.now().UTC().Truncate(time.Microsecond)
	window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Minute)}
	for _, f := range []nativeFixture{first, second} {
		issue := f.create(t, "Example activity")
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		tx, err := first.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		machine := "activity-" + string(f.project.ID)
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES(?,'example-runner',1,'test',?,?,?)`, machine, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET actor_json=?,activity_actor_principal_id=NULL WHERE native_id=?`, `{"principal_id":"example-reporter"}`, issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		older, newer := "z-old-"+string(f.project.ID), "a-new-"+string(f.project.ID)
		for i, id := range []string{older, newer} {
			c := tracker.ChangeRequest{ID: id, CurrentVersion: "version-" + id, CreatedAt: now.Add(time.Duration(i-2) * time.Minute)}
			raw, err := marshalNative(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,?)`, id, scope.organization, scope.project, issue.WorkItemID, raw); err != nil {
				t.Fatal(err)
			}
			v := tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{External: &tracker.ChangeExternalReference{URL: "https://example.test/" + id}}}
			raw, err = marshalNative(v)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,?)`, c.CurrentVersion, id, raw); err != nil {
				t.Fatal(err)
			}
		}
		for i := range 3 {
			data := tracker.NativeRunData{RunnerID: "example-runner", SessionID: "example-session", Identity: &tracker.NativeExecutionIdentity{Role: "plan"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "implement", Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", now), Phases: []tracker.NativePhase{{Name: "setup", StartedAt: now.Add(-3 * time.Minute), FinishedAt: now.Add(-2 * time.Minute)}, {Name: "implement", StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-30 * time.Second)}}, PhasesDropped: i}}
			if i == 2 {
				data.Finalization = &tracker.NativeFinalization{ChangeID: older, Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{URL: "https://example.test/publication"}}}
			}
			id := seedHealthRateAttempt(t, tx, scope, issue, machine, "succeeded", data, now.Add(-time.Second), -1)
			if i > 0 {
				cp, err := marshalNative(&tracker.NativeCheckpoint{Change: &tracker.NativeChangeReference{ChangeID: older}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_json=? WHERE id=?`, cp, id); err != nil {
					t.Fatal(err)
				}
			}
			if i == 1 {
				if err := updateActivityAttemptProjection(t.Context(), tx, id, data, nil); err != nil {
					t.Fatal(err)
				}
			}

		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	type snapshot struct {
		attempts []operatortool.ActivityAttempt
		typical  []operatortool.ActivityTypicalDuration
		partial  bool
	}
	read := func(ctx context.Context, f nativeFixture) snapshot {
		t.Helper()
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		a, err := readActivityAttempts(ctx, first.service.database.reader, string(scope.organization), []string{string(scope.project)}, operatortool.ActivityRequest{Limit: 3, RunnerID: "example-runner"}, window, now, false)
		if err != nil {
			t.Fatal(err)
		}
		typical, partial, err := first.service.readActivityTypicalDurations(ctx, first.service.database.reader, scope, window, "example-runner")
		if err != nil {
			t.Fatal(err)
		}
		return snapshot{a, typical, partial}
	}
	before := make(map[tracker.ProjectID]snapshot)
	for _, f := range []nativeFixture{first, second} {
		got := read(t.Context(), f)
		if len(got.attempts) != 3 || len(got.typical) != 1 || got.typical[0].P50Seconds != 90 || !got.partial {
			t.Fatalf("legacy=%+v", got)
		}
		changes := map[string]bool{}
		for _, a := range got.attempts {
			if a.RunnerID != "example-runner" || a.SessionID != "example-session" || a.Stage != "code" || a.Phase != "implement" || a.StageDurationSeconds == nil || *a.StageDurationSeconds != 90 || a.StageStartedAt == nil || !a.StageStartedAt.Equal(now.Add(-time.Minute)) || a.PullRequestURL == "" {
				t.Fatalf("legacy attempt=%+v", a)
			}
			changes[a.ChangeID] = true
		}
		if !changes["z-old-"+string(f.project.ID)] || !changes["a-new-"+string(f.project.ID)] {
			t.Fatalf("change precedence=%v", changes)
		}
		before[f.project.ID] = got
		ownContext := context.WithValue(t.Context(), reportsScopeKey{}, reportsScope{reporterPrincipal: "example-reporter"})
		if own := read(ownContext, f); !reflect.DeepEqual(own, got) {
			t.Fatalf("unknown ownership hid reporter activity: got=%+v want=%+v", own, got)
		}
		ctx := context.WithValue(t.Context(), reportsScopeKey{}, reportsScope{reporterPrincipal: "another-reporter"})
		denied := read(ctx, f)
		if len(denied.attempts) != 0 || len(denied.typical) != 0 {
			t.Fatalf("unknown ownership authorized another reporter: %+v", denied)
		}
	}
	if err := first.service.convertActivityProjections(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []nativeFixture{first, second} {
		if got := read(t.Context(), f); !reflect.DeepEqual(got, before[f.project.ID]) {
			t.Fatalf("typed=%+v legacy=%+v", got, before[f.project.ID])
		}
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json='{}',checkpoint_json='{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE change_requests SET record_json='{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE issues SET actor_json='{}'`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []nativeFixture{first, second} {
		ctx := context.WithValue(t.Context(), reportsScopeKey{}, reportsScope{reporterPrincipal: "example-reporter"})
		if got := read(ctx, f); !reflect.DeepEqual(got, before[f.project.ID]) {
			t.Fatalf("opaque data changed typed reads: got=%+v want=%+v", got, before[f.project.ID])
		}
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE change_versions SET activity_external_url='https://example.test/typed-url'`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []nativeFixture{first, second} {
		typedURLs := 0
		for _, a := range read(t.Context(), f).attempts {
			switch a.PullRequestURL {
			case "https://example.test/typed-url":
				typedURLs++
			case "https://example.test/publication":
			default:
				t.Fatalf("version URL ignored typed column: %+v", a)
			}
		}
		if typedURLs != 2 {
			t.Fatalf("typed version links=%d want=2", typedURLs)
		}
	}
}

func TestActivityBackfillBoundAndTransactions(t *testing.T) {
	for _, reportsOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("reportsOnly=%t", reportsOnly), func(t *testing.T) {
			first := newNativeFixture(t, nil, "", "activity-backfill")
			if err := first.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			second := newNativeFixture(t, first.service, first.project.OrganizationID, "activity-backfill-other")
			now := first.service.config.now().UTC()
			for _, f := range []nativeFixture{first, second} {
				issue := f.create(t, "Example legacy activity")
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				tx, err := first.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				machine := "activity-" + string(f.project.ID)
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES(?,'example-runner',1,'test',?,?,?)`, machine, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				for range storageConversionRows/2 + 1 {
					data := tracker.NativeRunData{Identity: &tracker.NativeExecutionIdentity{Role: "code"}}
					id := seedHealthRateAttempt(t, tx, scope, issue, machine, "succeeded", data, now, -1)
					if reportsOnly {
						if err := updateActivityAttemptProjection(t.Context(), tx, id, data, nil); err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET report_projection_version=NULL WHERE id=?", id); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			var stop string
			if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT id FROM native_attempts ORDER BY id LIMIT 1 OFFSET 1`).Scan(&stop); err != nil {
				t.Fatal(err)
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER activity_conversion_failure BEFORE UPDATE OF report_projection_version ON native_attempts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'conversion interrupted'); END`, stop)
			if _, err := first.service.database.db.ExecContext(t.Context(), trigger); err != nil {
				t.Fatal(err)
			}
			if err := first.service.convertActivityProjections(t.Context()); err == nil {
				t.Fatal("expected interrupted conversion")
			}
			count := func() int {
				t.Helper()
				var n int
				if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempts WHERE activity_projection_version=1 AND report_projection_version=1`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				var partial int
				if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempts WHERE report_projection_version=1 AND activity_projection_version IS NULL`).Scan(&partial); err != nil || partial != 0 {
					t.Fatalf("attempt and report projections committed separately: rows=%d error=%v", partial, err)
				}
				return n
			}
			if got := count(); got != storageConversionRows-1 {
				t.Fatalf("failed item blocks healthy conversions: %d", got)
			}
			if _, err := first.service.database.db.ExecContext(t.Context(), `DROP TRIGGER activity_conversion_failure`); err != nil {
				t.Fatal(err)
			}
			if err := first.service.convertActivityProjections(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := 2 * (storageConversionRows/2 + 1)
			if reportsOnly {
				want = 1 + storageConversionRows
			}
			if got := count(); got != want {
				t.Fatalf("tick exceeded or missed fixed budget: %d want=%d", got, want)
			}
			if err := first.service.convertActivityProjections(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := count(); got != 2*(storageConversionRows/2+1) {
				t.Fatalf("backfill did not complete: %d", got)
			}
			for _, f := range []nativeFixture{first, second} {
				var n int
				if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempts WHERE project_id=? AND activity_projection_version=1 AND report_projection_version=1`, f.project.ID).Scan(&n); err != nil || n != storageConversionRows/2+1 {
					t.Fatalf("project rows=%d err=%v", n, err)
				}
			}
		})
	}
}
