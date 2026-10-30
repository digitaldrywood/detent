package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthProjectionBackfill(t *testing.T) {
	t.Parallel()
	first := newNativeFixture(t, nil, "", "health-projection-first")
	if err := first.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	second := newNativeFixture(t, first.service, first.project.OrganizationID, "health-projection-second")
	now := first.service.config.now().UTC().Truncate(time.Millisecond)
	fixtures := []nativeFixture{first, second}
	var items [2]tracker.NativeIssue
	for i, f := range fixtures {
		items[i] = f.create(t, "historical health observations")
	}
	data := tracker.CollaborationData{FromState: "Backlog", ToState: "Todo", Reason: "worker_progress", ReasonDetail: "permission_wait", Revision: 42, Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "runner_capacity_full", Source: "native_claim", RunnerID: "runner_example"}, Run: &tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(-time.Minute)}}}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := first.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 2*storageConversionRows + 1 {
		f, item := fixtures[i%2], items[i%2]
		kind := "scheduler.decision"
		if i%3 == 0 {
			kind = "run.observed"
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) VALUES(?,?,?,?,?,?,1,?,?,?)`, fmt.Sprintf("evt_health_projection_%03d", i), f.project.OrganizationID, f.project.ID, item.WorkItemID, i+100, kind, `{"kind":"runner","principal_id":"runner_example"}`, string(raw), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	for i, f := range fixtures {
		if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET actor_kind=NULL,actor_principal_id=NULL WHERE native_id=?`, items[i].WorkItemID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,?,?,?,1,1,'historical evidence','{"kind":"human","principal_id":"example_user"}',?,?)`, fmt.Sprintf("comment_health_%d", i), f.project.OrganizationID, f.project.ID, items[i].WorkItemID, formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		report := gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: strconv.Itoa(i + 1), RunAttempt: "1", RunURL: fmt.Sprintf("https://github.com/example-org/example-project/actions/runs/%d/attempts/1", i+1), Conclusion: "failure"}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO scheduled_validation_reports(organization_id,project_id,repository,run_id,run_attempt,record_json,recorded_at) VALUES(?,?,?,?,?,?,?)`, f.project.OrganizationID, f.project.ID, report.Repository, report.RunID, report.RunAttempt, string(encoded), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}

		projects, err := json.Marshal([]string{string(first.project.ID), string(second.project.ID)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO health_findings(id,organization_id,fingerprint,signal,class,subject_json,projects_json,opened_at,last_seen_at,severity,summary,next_action,evidence_json) VALUES(?,?,?,'example','human','{}',?, ?,?,'attention','Example','Inspect','{}')`, fmt.Sprintf("finding_health_%d", i), f.project.OrganizationID, fmt.Sprintf("fingerprint_health_%d", i), string(projects), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `DELETE FROM time_index_progress WHERE owner='health'`); err != nil {
		t.Fatal(err)
	}
	before, err := readHealthTimeEvents(t.Context(), first.service.database.reader, first.project.OrganizationID, now)
	if err != nil || len(before) != 2*storageConversionRows+1 {
		t.Fatalf("unknown events=%d err=%v", len(before), err)
	}
	var findingPages [2]hubhealth.FindingsPage
	var scheduledBefore [2]hubhealth.ScheduledValidation
	for i, f := range fixtures {
		page, err := first.service.readHealthFindings(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, "open", "", "", 100)
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("unknown project membership: findings=%+v err=%v", page, err)
		}
		scheduled, err := readHealthScheduled(t.Context(), first.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now)
		if err != nil || scheduled.Failures != 1 || !scheduled.LastReport.Equal(now) {
			t.Fatalf("unknown scheduled conclusion: %+v err=%v", scheduled, err)
		}
		scheduledBefore[i] = scheduled

		findingPages[i] = page
	}

	countUnknown := func() int {
		t.Helper()
		var n int
		if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT
   (SELECT count(*) FROM collaboration_events WHERE health_projected IS NULL)+
   (SELECT count(*) FROM issues WHERE actor_kind IS NULL)+
   (SELECT count(*) FROM native_comments WHERE actor_kind IS NULL)+
   (SELECT count(*) FROM scheduled_validation_reports WHERE conclusion IS NULL)+
   (SELECT count(*) FROM health_findings WHERE projects_projected IS NULL)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	remaining := countUnknown()
	ticks := (remaining + storageConversionRows - 1) / storageConversionRows
	committed := 0
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		if !strings.HasPrefix(query, "UPDATE collaboration_events SET health_projected=1,") {
			return
		}
		var visible int
		if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE id LIKE 'evt_health_projection_%' AND health_projected=1`).Scan(&visible); err != nil {
			t.Fatal(err)
		}
		if visible != committed {
			t.Fatalf("prior rows were not committed individually: visible=%d want=%d", visible, committed)
		}
		committed++
	}})
	for tick := range ticks {
		if err := first.service.convertHealthProjections(ctx); err != nil {
			t.Fatal(err)
		}
		want := max(0, remaining-storageConversionRows)
		if got := countUnknown(); got != want {
			t.Fatalf("tick %d unknown=%d want=%d", tick, got, want)
		}
		remaining = want
	}
	if committed != 2*storageConversionRows+1 {
		t.Fatalf("converted events=%d", committed)
	}
	after, err := readHealthTimeEvents(t.Context(), first.service.database.reader, first.project.OrganizationID, now)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("typed event evaluation changed: err=%v", err)
	}
	var actor, principal, from, to, reason, detail, at, outcome, refusal, source, runner string
	var revision int
	var landed bool
	if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT actor_kind,actor_principal_id,transition_from,transition_to,transition_reason,transition_detail,transition_revision,event_landed,event_landing_at,scheduler_outcome,scheduler_reason,scheduler_source,scheduler_runner_id FROM collaboration_events WHERE id='evt_health_projection_000'`).Scan(&actor, &principal, &from, &to, &reason, &detail, &revision, &landed, &at, &outcome, &refusal, &source, &runner); err != nil {
		t.Fatal(err)
	}
	if actor != "runner" || principal != "runner_example" || from != "Backlog" || to != "Todo" || reason != "worker_progress" || detail != "" || revision != 42 || !landed || at != formatHubTime(now.Add(-time.Minute)) || outcome != "skipped" || refusal != "runner_capacity_full" || source != "native_claim" || runner != "runner_example" {
		t.Fatalf("incorrect event facts: %s %s %s %s %s %s %d %v %s %s %s %s %s", actor, principal, from, to, reason, detail, revision, landed, at, outcome, refusal, source, runner)
	}
	for _, query := range []string{
		`UPDATE health_findings SET projects_json='[]' WHERE id LIKE 'finding_health_%'`,
		`UPDATE machines SET capabilities_json='{}' WHERE id LIKE 'machine_health_%'`,
	} {
		if _, err := first.service.database.db.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET scheduler_outcome='claimed',scheduler_reason='policy_mismatch',scheduler_source='native_runner_routing',scheduler_runner_id='runner_typed',scheduler_at=?,scheduler_revision=73,event_landing_at=? WHERE id LIKE 'evt_health_projection_%'`, formatHubTime(now), formatHubTime(now.Add(-2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	events, err := readHealthTimeEvents(t.Context(), first.service.database.reader, first.project.OrganizationID, now)
	if err != nil || len(events) != 2*storageConversionRows+1 {
		t.Fatalf("typed events=%d err=%v", len(events), err)
	}
	for _, event := range events {
		var observed tracker.CollaborationData
		if err := json.Unmarshal([]byte(event.Raw), &observed); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "scheduler.decision" {
			want := tracker.NativeSchedulerDecision{Outcome: "claimed", Reason: "policy_mismatch", Source: "native_runner_routing", RunnerID: "runner_typed", At: now, WorkItemRevision: 73}
			if observed.Decision == nil || !reflect.DeepEqual(*observed.Decision, want) {
				t.Fatalf("decision ignored typed fields: %+v", observed.Decision)
			}
		} else if observed.Run == nil || observed.Run.Runtime == nil || observed.Run.Runtime.Landing == nil || !observed.Run.Runtime.Landing.ObservedAt.Equal(now.Add(-2*time.Minute)) {
			t.Fatalf("landing ignored typed timestamp: %+v", observed.Run)
		}
	}
	if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET event_landed=0 WHERE id LIKE 'evt_health_projection_%' AND type='run.observed'`); err != nil {
		t.Fatal(err)
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			for range len(before)/commandReceiptPruneBatch + 1 {
				if err := first.service.database.populateTimeIndexes(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
		}
		got, err := readHealthTimeEvents(t.Context(), first.service.database.reader, first.project.OrganizationID, now)
		if err != nil || len(got) != (2*storageConversionRows+1)*2/3 {
			t.Fatalf("typed landing filter, indexed=%v: events=%d err=%v", indexed, len(got), err)
		}
		for _, event := range got {
			if event.Kind != "scheduler.decision" {
				t.Fatalf("typed non-landing was indexed: %+v", event)
			}
		}
	}
	for i, f := range fixtures {
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		page, err := first.service.readHealthFindings(t.Context(), scope, "open", "", "", 100)
		if err != nil || !reflect.DeepEqual(page.Items, findingPages[i].Items) || page.NextCursor != findingPages[i].NextCursor || page.ChangeSequence < findingPages[i].ChangeSequence {
			t.Fatalf("project %d changed findings after backfill: before=%+v after=%+v err=%v", i, findingPages[i], page, err)
		}
		scheduled, err := readHealthScheduled(t.Context(), first.service.database.reader, scope, now)
		if err != nil || !reflect.DeepEqual(scheduled, scheduledBefore[i]) {
			t.Fatalf("project %d changed scheduled evidence: before=%+v after=%+v err=%v", i, scheduledBefore[i], scheduled, err)
		}
		if _, err := first.service.database.db.ExecContext(t.Context(), `UPDATE scheduled_validation_reports SET conclusion='success' WHERE project_id=?`, f.project.ID); err != nil {
			t.Fatal(err)
		}
		scheduled, err = readHealthScheduled(t.Context(), first.service.database.reader, scope, now)
		if err != nil || scheduled.Failures != 0 || !scheduled.LastGreen.Equal(now) {
			t.Fatalf("typed conclusion did not govern evaluation: %+v err=%v", scheduled, err)
		}

		var conclusion, kind string
		if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT conclusion FROM scheduled_validation_reports WHERE project_id=?`, f.project.ID).Scan(&conclusion); err != nil || conclusion != "success" {
			t.Fatalf("project %d conclusion=%s err=%v", i, conclusion, err)
		}
		if err := first.service.database.reader.QueryRowContext(t.Context(), `SELECT actor_kind FROM native_comments WHERE project_id=?`, f.project.ID).Scan(&kind); err != nil || kind != "human" {
			t.Fatalf("project %d actor=%s err=%v", i, kind, err)
		}
	}
}
