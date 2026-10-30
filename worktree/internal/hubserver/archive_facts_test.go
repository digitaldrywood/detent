package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArchiveFactsBackfill(t *testing.T) {
	f := newStorageArchiveFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	d := f.service.database
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	items := make([]string, 2)
	records := make([]conversationRecord, 2)
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-user", ViaKey: "example-key", DisplayName: "Example User", Agent: "example-agent"}
	for index := range 2 {
		issue := f.nativeFixture.create(t, fmt.Sprintf("Historical archive %d", index))
		item := string(issue.WorkItemID)
		items[index] = item
		record := f.messages(t, item, 1, conversation.VisibilityShared)
		records[index] = record
		f.transition(t, item, "Done", f.now.Add(-73*time.Hour))
		change := tracker.ChangeRequest{ID: "archive-change-" + item, CurrentVersion: "version-" + item, Landed: &tracker.ChangeLanding{VersionID: "version-" + item, MergeSHA: "merge-" + item, BaseRef: "develop", Actor: actor, LandedAt: f.now.Add(-74 * time.Hour)}}
		raw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id,claim_head_sha) VALUES(?,?,?,?,?,?,'head')`, change.ID, scope.organization, scope.project, item, string(raw), change.CurrentVersion); err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.ExecContext(t.Context(), `INSERT INTO change_issue_links(change_id,organization_id,project_id,work_item_id) VALUES(?,?,?,?)`, change.ID, scope.organization, scope.project, item); err != nil {
			t.Fatal(err)
		}
		execution, err := json.Marshal(record.Execution)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.ExecContext(t.Context(), `UPDATE conversations SET status='settled',execution_json=?,content_attachment_id='',conversion_state=0,execution_status=NULL,execution_lease_id=NULL WHERE id=?`, string(execution), record.ID); err != nil {
			t.Fatal(err)
		}
	}
	event := `{"operation":"restore","from_state":"Rework","to_state":"Done","run":{"attempt_id":"example-attempt","outcome":"succeeded","finalization":{"publication":{"external":{"head_sha":"example-commit"}}}}}`
	if _, err := d.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<=?) INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) SELECT 'archive-event-'||x,?,?,?,1000+x,'run.finished',1,?,?,? FROM n`, archiveFactsRowsPerTick, scope.organization, scope.project, items[0], `{"kind":"human","principal_id":"example-user"}`, event, formatHubTime(f.now)); err != nil {
		t.Fatal(err)
	}
	remaining := func() int {
		t.Helper()
		var count int
		if err := d.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM change_requests WHERE archive_landed_version_id IS NULL)+(SELECT count(*) FROM conversations WHERE execution_status IS NULL OR execution_lease_id IS NULL)+(SELECT count(*) FROM collaboration_events WHERE archive_operation IS NULL)`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	safe := func(item string) bool {
		t.Helper()
		var value bool
		if err := d.reader.QueryRowContext(t.Context(), `SELECT `+storageArchiveSafe+` FROM issues i WHERE i.native_id=?`, formatHubTime(f.now), item).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, item := range items {
		if safe(item) {
			t.Fatal("unknown facts authorized archival")
		}
	}
	before, err := readArchiveTimeline(t.Context(), d.reader, scope, items[0])
	if err != nil {
		t.Fatal(err)
	}
	total := remaining()
	if total != archiveFactsRowsPerTick+5 {
		t.Fatalf("unknown rows=%d", total)
	}
	changed, err := d.populateArchiveFact(t.Context())
	if err != nil || !changed {
		t.Fatalf("single backfill: changed=%v err=%v", changed, err)
	}
	if got := remaining(); got != total-1 {
		t.Fatalf("single transaction changed %d rows", total-got)
	}
	if err := d.populateArchiveFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := remaining(); got != 4 {
		t.Fatalf("tick left %d rows, want 4", got)
	}
	resumed := &database{db: d.db, reader: d.reader}
	if err := resumed.populateArchiveFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := remaining(); got != 0 {
		t.Fatalf("durable progress left %d unknown rows", got)
	}
	after, err := readArchiveTimeline(t.Context(), d.reader, scope, items[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("backfill changed timeline fields")
	}
	for index, item := range items {
		if !safe(item) {
			t.Fatalf("backfilled item %s refused archival", item)
		}
		var version, at, merge string
		var gotActor tracker.Actor
		if err := d.reader.QueryRowContext(t.Context(), `SELECT archive_landed_version_id,archive_landed_at,archive_landed_merge_sha,archive_actor_kind,archive_actor_principal_id,archive_actor_via_key,archive_actor_display_name,archive_actor_agent FROM change_requests WHERE id=?`, "archive-change-"+item).Scan(&version, &at, &merge, &gotActor.Kind, &gotActor.PrincipalID, &gotActor.ViaKey, &gotActor.DisplayName, &gotActor.Agent); err != nil {
			t.Fatal(err)
		}
		if version != "version-"+item || at != formatHubTime(f.now.Add(-74*time.Hour)) || merge != "merge-"+item || gotActor != actor {
			t.Fatalf("landing facts: %s %s %s %+v", version, at, merge, gotActor)
		}
		var status, lease string
		if err := d.reader.QueryRowContext(t.Context(), `SELECT execution_status,execution_lease_id FROM conversations WHERE id=?`, records[index].ID).Scan(&status, &lease); err != nil {
			t.Fatal(err)
		}
		if status != "idle" || lease != "" {
			t.Fatalf("execution=%s lease=%s", status, lease)
		}
	}
	var operation, from, to, attempt, outcome, commit string
	if err := d.reader.QueryRowContext(t.Context(), `SELECT archive_operation,archive_from_state,archive_to_state,archive_attempt_id,archive_outcome,archive_landed_commit FROM collaboration_events WHERE id='archive-event-1'`).Scan(&operation, &from, &to, &attempt, &outcome, &commit); err != nil {
		t.Fatal(err)
	}
	if operation != "restore" || from != "Rework" || to != "Done" || attempt != "example-attempt" || outcome != "succeeded" || commit != "example-commit" {
		t.Fatalf("event facts: %s %s %s %s %s %s", operation, from, to, attempt, outcome, commit)
	}
	if _, err := d.db.ExecContext(t.Context(), `UPDATE collaboration_events SET archive_from_state='Todo',archive_to_state='Rework',archive_attempt_id='typed-attempt',archive_outcome='failed',archive_landed_commit='typed-commit' WHERE id='archive-event-1'`); err != nil {
		t.Fatal(err)
	}
	timeline, err := readArchiveTimeline(t.Context(), d.reader, scope, items[0])
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, raw := range timeline {
		var value struct {
			Attempt string `json:"attempt_id"`
			From    string `json:"from_state"`
			To      string `json:"to_state"`
			Outcome string `json:"outcome"`
			Commit  string `json:"landed_commit"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if value.Attempt == "typed-attempt" {
			found = true
			if value.From != "Todo" || value.To != "Rework" || value.Outcome != "failed" || value.Commit != "typed-commit" {
				t.Fatalf("timeline ignored typed fields: %+v", value)
			}
		}
	}
	if !found {
		t.Fatal("timeline ignored typed attempt")
	}
	typedAt := formatHubTime(f.now.Add(-75 * time.Hour))
	typedActor := tracker.Actor{Kind: "integration", PrincipalID: "typed-principal", ViaKey: "typed-key", DisplayName: "Typed Actor", Agent: "typed-agent"}
	if _, err := d.db.ExecContext(t.Context(), `UPDATE change_requests SET archive_landed_at=?,archive_landed_merge_sha='typed-merge',archive_actor_kind=?,archive_actor_principal_id=?,archive_actor_via_key=?,archive_actor_display_name=?,archive_actor_agent=? WHERE id=?`, typedAt, typedActor.Kind, typedActor.PrincipalID, typedActor.ViaKey, typedActor.DisplayName, typedActor.Agent, "archive-change-"+items[0]); err != nil {
		t.Fatal(err)
	}
	timeline, err = readArchiveTimeline(t.Context(), d.reader, scope, items[0])
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, raw := range timeline {
		var value struct {
			Change string        `json:"change_id"`
			At     string        `json:"recorded_at"`
			Commit string        `json:"landed_commit"`
			Actor  tracker.Actor `json:"actor"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if value.Change == "archive-change-"+items[0] {
			found = true
			if value.At != typedAt || value.Commit != "typed-merge" || value.Actor != typedActor {
				t.Fatalf("timeline ignored typed landing: %+v", value)
			}
		}
	}
	if !found {
		t.Fatal("typed landing missing from timeline")
	}
	for _, test := range []struct {
		status any
		lease  any
		want   bool
	}{{"completed", "", true}, {"running", "", false}, {nil, "", false}, {"completed", nil, false}} {
		if _, err := d.db.ExecContext(t.Context(), `UPDATE conversations SET execution_status=?,execution_lease_id=? WHERE id=?`, test.status, test.lease, records[1].ID); err != nil {
			t.Fatal(err)
		}
		_, got, err := storageConversionStamp(t.Context(), d.reader, scope, storageConversionFamily{name: "messages"}, records[1].ID, f.now)
		if err != nil || got != test.want {
			t.Fatalf("conversion status=%v lease=%v safe=%v err=%v", test.status, test.lease, got, err)
		}
	}
	leaseIssue := f.nativeFixture.create(t, "Execution lease")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	lease := claimNativeAttempt(t, f.nativeFixture, f.worker(t, "worker"), "example-machine", "example-session", leaseIssue.WorkItemID)
	if _, err := d.db.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) SELECT 'archive-attempt-'||lease_id,?,?,?,lease_id,fencing_token,'example-run',1,'running','{}',?,? FROM leases WHERE lease_id=?`, scope.organization, scope.project, leaseIssue.WorkItemID, formatHubTime(f.now), formatHubTime(f.now), lease.ID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		released bool
		status   string
		want     bool
	}{{false, "running", false}, {true, "running", false}, {true, "succeeded", true}} {
		var released any
		if test.released {
			released = formatHubTime(f.now)
		}
		if _, err := d.db.ExecContext(t.Context(), "UPDATE leases SET released_at=? WHERE lease_id=?", released, lease.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.ExecContext(t.Context(), "UPDATE native_attempts SET status=? WHERE lease_id=?", test.status, lease.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.ExecContext(t.Context(), "UPDATE conversations SET execution_status='completed',execution_lease_id=? WHERE id=?", lease.ID, records[1].ID); err != nil {
			t.Fatal(err)
		}
		_, got, err := storageConversionStamp(t.Context(), d.reader, scope, storageConversionFamily{name: "messages"}, records[1].ID, f.now)
		if err != nil || got != test.want {
			t.Fatalf("execution lease released=%v attempt=%s safe=%v err=%v", test.released, test.status, got, err)
		}
	}
	for _, test := range []struct {
		current, landed any
		want            bool
	}{{"version-" + items[1], "version-" + items[1], true}, {"next-version", "version-" + items[1], false}, {nil, "", false}, {"version-" + items[1], nil, false}, {"", "", true}} {
		if _, err := d.db.ExecContext(t.Context(), `UPDATE conversations SET execution_status='idle',execution_lease_id='' WHERE id=?`, records[1].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.db.ExecContext(t.Context(), `UPDATE change_requests SET claim_version_id=?,archive_landed_version_id=? WHERE id=?`, test.current, test.landed, "archive-change-"+items[1]); err != nil {
			t.Fatal(err)
		}
		if got := safe(items[1]); got != test.want {
			t.Fatalf("current=%v landed=%v safe=%v", test.current, test.landed, got)
		}
	}
	tx := f.tx(t)
	var stale tracker.ChangeRequest
	if err := json.Unmarshal([]byte(`{"change_id":"archive-change-`+items[0]+`","landed":{"version_id":"stale"}}`), &stale); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveChangeFacts(t.Context(), tx, stale, `{"old":true}`); err != nil {
		t.Fatal(err)
	}
	f.commit(t, tx)
	var version sql.NullString
	if err := d.reader.QueryRowContext(t.Context(), `SELECT archive_landed_version_id FROM change_requests WHERE id=?`, stale.ID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version.String == "stale" {
		t.Fatal("stale backfill overwrote newer facts")
	}
}
