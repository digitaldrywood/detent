package hubserver

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHotAttemptProjectionCompatibility(t *testing.T) {
	t.Parallel()
	first := newNativeFixture(t, nil, "", "hot-first")
	if err := first.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	second := newNativeFixture(t, first.service, first.project.OrganizationID, "hot-second")
	now := first.service.config.now().UTC().Truncate(time.Millisecond)
	at := now.Add(-time.Hour)
	fixtures := []nativeFixture{first, second}
	cases := []struct {
		name, status string
		data         tracker.NativeRunData
	}{
		{"instance failure", "failed", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "workspace hook after_create failed", Stage: "pre_provider", Attribution: "instance"}}},
		{"recovery", "failed", tracker.NativeRunData{Finalization: &tracker.NativeFinalization{Error: "native checkpoint requires recovery: checkpoint_unavailable"}}},
		{"landing", "succeeded", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(-time.Minute), ChangeID: "change_example", VersionID: "version_example", MergeSHA: "abc"}}}},
		{"conflict", "failed", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict"}}}},
		{"heartbeat", "running", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{HeartbeatAt: at, LocalAttemptID: 42}}},
		{"duplicate landing", "succeeded", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(-time.Minute), ChangeID: "change_example", VersionID: "version_example", MergeSHA: "abc"}}}},
		{"landing observation", "running", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{ObservedAt: now.Add(-time.Minute)}}}},
		{"cancelled", "cancelled", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "cancelled"}}},
	}
	var ids [][]string
	projectItems := map[string]tracker.NativeWorkItemID{}
	for _, f := range fixtures {
		issue := f.create(t, "projection observations")
		projectItems[string(f.project.ID)] = issue.WorkItemID
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		tx, err := f.service.database.db.BeginTx(first.service.runDetailContext(t.Context()), nil)
		if err != nil {
			t.Fatal(err)
		}
		machine := "projection-" + string(f.project.ID)
		if _, err := tx.ExecContext(first.service.runDetailContext(t.Context()), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES(?,'example-runner',1,'test',?,?,?)`, machine, formatHubTime(at), formatHubTime(at), formatHubTime(at)); err != nil {
			t.Fatal(err)
		}
		var projectIDs []string
		for _, c := range cases {
			projectIDs = append(projectIDs, seedHealthRateAttempt(t, tx, scope, issue, machine, c.status, c.data, at, 1))
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, projectIDs)
	}
	window := operatortool.AnalyticsWindow{From: at.Add(-time.Minute), To: now}
	for _, stage := range []string{"legacy", "mixed", "converted", "landing updated", "null document landing updated"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "mixed" {
				if err := first.service.convertAttemptHotProjection(first.service.runDetailContext(t.Context()), ids[0][0]); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "converted" {
				if err := first.service.convertAttemptHotProjections(first.service.runDetailContext(t.Context())); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "landing updated" || stage == "null document landing updated" {
				ctx := first.service.runDetailContext(t.Context())
				if err := first.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
					if stage == "null document landing updated" {
						if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET data_json=?,payload_object_key='' WHERE id=?", "null", ids[0][3]); err != nil {
							return err
						}
					}
					return persistAttemptLanding(ctx, tx, ids[0][3], tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(-time.Minute)})
				}); err != nil {
					t.Fatal(err)
				}
				var key string
				if err := first.service.database.reader.QueryRowContext(ctx, "SELECT payload_object_key FROM native_attempts WHERE id=?", ids[0][3]).Scan(&key); err != nil {
					t.Fatal(err)
				}
				inline, err := resolveStorageJSON(ctx, []byte(key))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := first.service.database.db.ExecContext(ctx, "UPDATE native_attempts SET data_json=?,payload_object_key='' WHERE id=?", string(inline), ids[0][3]); err != nil {
					t.Fatal(err)
				}
				var document string
				if err := first.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM native_attempts WHERE id=?", ids[0][3]).Scan(&document); err != nil {
					t.Fatal(err)
				}
				var data tracker.NativeRunData
				if err := json.Unmarshal([]byte(document), &data); err != nil {
					t.Fatal(err)
				}
				if data.Runtime == nil || data.Runtime.Landing == nil || !data.Runtime.Landing.Landed {
					t.Fatalf("landing receipt not persisted: %s", document)
				}
			}
			for _, f := range fixtures {
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				for _, window := range []operatortool.AnalyticsWindow{window, {From: now.Add(-30 * time.Minute), To: now}, {From: at.Add(100 * time.Microsecond), To: now}} {
					want, err := readJSONHealthRateAttempts(first.service.runDetailContext(t.Context()), f.service.database.reader, scope, window)
					if err != nil {
						t.Fatal(err)
					}
					got, err := readHealthRateAttempts(first.service.runDetailContext(t.Context()), f.service.database.reader, scope, window)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: typed=%+v legacy=%+v", f.project.ID, got, want)
					}
					if stage == "converted" {
						tx, err := f.service.database.db.BeginTx(first.service.runDetailContext(t.Context()), nil)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(first.service.runDetailContext(t.Context()), `UPDATE native_attempts SET data_json='{}' WHERE project_id=?`, f.project.ID); err != nil {
							tx.Rollback()
							t.Fatal(err)
						}
						for _, read := range []struct {
							name string
							run  func() ([]healthRateAttempt, error)
						}{
							{"typed", func() ([]healthRateAttempt, error) {
								return readHealthRateAttempts(first.service.runDetailContext(t.Context()), tx, scope, window)
							}},
							{"mixed", func() ([]healthRateAttempt, error) {
								return readLegacyHealthRateAttempts(first.service.runDetailContext(t.Context()), tx, scope, window)
							}},
						} {
							observed, readErr := read.run()
							if readErr != nil || !reflect.DeepEqual(observed, want) {
								tx.Rollback()
								t.Fatalf("%s %s reads opaque attempt fields: got=%+v want=%+v err=%v", f.project.ID, read.name, observed, want, readErr)
							}
						}
						if err := tx.Rollback(); err != nil {
							t.Fatal(err)
						}
					}
				}
				matches, err := readLocalAttemptMatches(first.service.runDetailContext(t.Context()), f.service.database.reader, scope, string(projectItems[string(f.project.ID)]), 42)

				if err != nil || len(matches) != 1 {
					t.Fatalf("scoped local identity: %v %v", matches, err)
				}
			}
		})
	}
	var converted int
	if err := first.service.database.reader.QueryRowContext(first.service.runDetailContext(t.Context()), "SELECT count(*) FROM native_attempts WHERE hot_projection_version=1").Scan(&converted); err != nil || converted != len(cases)*len(fixtures) {
		t.Fatalf("converted=%d err=%v", converted, err)
	}
	t.Run("legacy history outside window", func(t *testing.T) {
		scope := nativeScope{organization: first.project.OrganizationID, project: first.project.ID}
		item := projectItems[string(first.project.ID)]
		tx, err := first.service.database.db.BeginTx(first.service.runDetailContext(t.Context()), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		legacyIssue := tracker.NativeIssue{}
		legacyIssue.WorkItemID = item
		for i := range healthReadLimit + 1 {
			data := tracker.NativeRunData{CompletionBody: strings.Repeat("x", 4096)}
			if i == 0 {
				data.Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(-time.Minute), ChangeID: "change_legacy", VersionID: "version_legacy", MergeSHA: "def"}}
			}
			seedHealthRateAttempt(t, tx, scope, legacyIssue, "projection-"+string(first.project.ID), "succeeded", data, now.Add(-90*24*time.Hour), -1)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, stage := range []string{"legacy", "partially backfilled"} {
			if stage == "partially backfilled" {
				if err := first.service.convertAttemptHotProjections(first.service.runDetailContext(t.Context())); err != nil {
					t.Fatal(err)
				}
				var after int
				if err := first.service.database.reader.QueryRowContext(first.service.runDetailContext(t.Context()), "SELECT count(*) FROM native_attempts WHERE hot_projection_version=1").Scan(&after); err != nil || after-converted != storageConversionRows {
					t.Fatalf("bounded conversion: before=%d after=%d err=%v", converted, after, err)
				}
			}
			want, err := readJSONHealthRateAttempts(first.service.runDetailContext(t.Context()), first.service.database.reader, scope, window)
			if err != nil {
				t.Fatal(err)
			}
			var statements []string
			ctx := withRequestTiming(first.service.runDetailContext(t.Context()), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
				statements = append(statements, query)
			}})
			got, err := readHealthRateAttempts(ctx, first.service.database.reader, scope, window)
			if len(statements) != 2 {
				t.Fatalf("legacy health read query count=%d want 2", len(statements))
			}
			plan := explainQueryPlan(t, first.service.database.reader, statements[0], []any{scope.organization, scope.project})
			if !strings.Contains(strings.Join(plan, " "), "native_attempts_hot_legacy_identity_idx") {
				t.Fatalf("legacy discovery lacks scoped projection index: %v", plan)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(cases)+1 {
				t.Fatalf("%s lost current observations or the old attempt's recent landing: %d", stage, len(got))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s history changed observations", stage)
			}
			matches, err := readLocalAttemptMatches(first.service.runDetailContext(t.Context()), first.service.database.reader, scope, string(item), 42)
			if err != nil || len(matches) != 1 {
				t.Fatalf("%s history changed local selection: %v %v", stage, matches, err)
			}
		}
	})

}
