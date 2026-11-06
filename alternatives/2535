package postgres_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNotificationDeliveryPreservesTenantAndRecipientScope(t *testing.T) {
	var candidate *postgres.Repository
	if _, ok := any(candidate).(accountstore.NotificationRepository); !ok {
		t.Fatal("PostgreSQL notification owner is missing")
	}
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	delivery := any(repository).(accountstore.NotificationRepository)
	now := historyAt()
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedNotificationMembers(t, repository, org)
		for repeat := range 2 {
			for index, item := range []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"} {
				event := tracker.CollaborationEvent{ID: fmt.Sprintf("event_%s_%d", item, repeat), OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: item, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "worker"}, RecordedAt: now.Add(time.Duration(index+repeat*2) * time.Second), Data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}}
				row, err := content.PrepareEvent(t.Context(), org, historyJSON(t, event))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { var err error; row, err = w.AppendEvent(t.Context(), row); return err }); err != nil {
					t.Fatal(err)
				}
			}
		}

		events, err := delivery.NotificationEvents(t.Context(), org)
		if err != nil || len(events) != 4 {
			t.Fatalf("events=%d error=%v", len(events), err)
		}
		for _, event := range events {
			for range 2 {
				done, _, err := delivery.QueueNotification(t.Context(), org, event, accountstore.IssueNotice{StateFrom: "Todo", StateTo: "In Progress"}, "", true, 256)
				if err != nil || !done {
					t.Fatalf("queue complete=%v error=%v", done, err)
				}
			}
		}
		if _, found, err := delivery.ClaimNotification(t.Context(), org, now); err != nil || found {
			t.Fatalf("early claim=%v error=%v", found, err)
		}
	}
	if err := repository.WriteTenant(t.Context(), "org_second", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "second", "prj_first")
	}); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seen := map[string]bool{}
		for range 2 {
			notice, found, err := delivery.ClaimNotification(t.Context(), org, now.Add(2*time.Minute))
			if err != nil || !found {
				t.Fatalf("claim=%v error=%v", found, err)
			}
			if org == "org_second" && notice.Email == "" {
				continue
			}
			if notice.Email != "first@example.com" && notice.Email != "second@example.com" {
				t.Fatalf("recipient=%q", notice.Email)
			}
			if seen[notice.Email] || len(notice.Notices) != 2 || notice.Notices[0].StateTo != "In Progress" {
				t.Fatalf("notice=%+v", notice)
			}
			seen[notice.Email] = true
			other := tracker.OrganizationID("org_first")
			if org == other {
				other = "org_second"
			}
			if err := delivery.UnsubscribeNotification(t.Context(), other, notice.Token); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("cross-tenant unsubscribe=%v", err)
			}
			if err := delivery.UnsubscribeNotification(t.Context(), org, notice.Token); err != nil {
				t.Fatal(err)
			}
		}
		expected := 2
		if org == "org_second" {
			expected = 1
		}
		if len(seen) != expected {
			t.Fatalf("authorized recipients=%v", seen)
		}
		if _, found, err := delivery.ClaimNotification(t.Context(), org, now.Add(time.Hour)); err != nil || found {
			t.Fatalf("duplicate claim=%v error=%v", found, err)
		}
		if events, err := delivery.NotificationEvents(t.Context(), org); err != nil || len(events) != 0 {
			t.Fatalf("remaining events=%d error=%v", len(events), err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events
            (organization_id,project_id,work_item_id,id,sequence,project_sequence,schema_version,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
            SELECT $1,'prj_first','wi_first_1','consumed_'||n,n+2,n+4,1,$2,
            'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
            FROM generate_series(1,30000) n`, org, now); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE issue_notification_cursors SET sequence=30002 WHERE organization_id=$1 AND work_item_id='wi_first_1'`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE project_event_cursors SET sequence=30004 WHERE organization_id=$1 AND project_id='prj_first'`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `ANALYZE collaboration_events`); err != nil {
			t.Fatal(err)
		}
		tx, err := pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
			t.Fatal(err)
		}
		probe := &searchPlanProbe{DBTX: tx}
		if _, err := pgquery.New(probe).NotificationEvents(t.Context(), pgquery.NotificationEventsParams{OrganizationID: org, ProjectID: "prj_first", ProjectSequence: 4}); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.statement, probe.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plan []struct{ Plan searchPlanNode }
		if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
			t.Fatalf("plan: %v", err)
		}
		var visits func(searchPlanNode) float64
		visits = func(node searchPlanNode) float64 {
			var count float64
			if node.Relation == "collaboration_events" {
				count = (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
			}
			for _, child := range node.Plans {
				count += visits(child)
			}
			return count
		}
		examined := visits(plan[0].Plan)
		t.Logf("consumed history=30004 catch-up examined=%.0f", examined)
		if examined > 256 {
			t.Errorf("discovery examined %.0f history rows; budget 256", examined)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		previous := int64(4)
		for range 118 {
			events, err := delivery.NotificationEvents(t.Context(), org)
			if err != nil || len(events) != 0 {
				t.Fatalf("consumed page events=%d error=%v", len(events), err)
			}
			var seek int64
			if err := admin.QueryRowContext(t.Context(), `SELECT notification_sequence FROM project_event_cursors WHERE organization_id=$1 AND project_id='prj_first'`, org).Scan(&seek); err != nil {
				t.Fatal(err)
			}
			if seek <= previous || seek-previous > 256 {
				t.Fatalf("discovery seek advanced %d -> %d", previous, seek)
			}
			previous = seek
		}
		if previous != 30004 {
			t.Fatalf("retained seek=%d", previous)
		}
		tx, err = pool.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
			t.Fatal(err)
		}
		probe = &searchPlanProbe{DBTX: tx}
		projects, err := pgquery.New(probe).LockNotificationProject(t.Context(), org)
		if err != nil || len(projects) != 0 {
			t.Fatalf("idle projects=%d error=%v", len(projects), err)
		}
		if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.statement, probe.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
			t.Fatalf("idle plan: %v", err)
		}
		if examined := visits(plan[0].Plan); examined != 0 {
			t.Fatalf("idle history rows=%.0f", examined)
		}
		t.Log("idle discovery examined 0 history rows")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		value := tracker.CollaborationEvent{ID: "event_after_seek", OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_2", Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "worker"}, RecordedAt: now.Add(time.Hour), Data: tracker.CollaborationData{FromState: "In Progress", ToState: "Done"}}
		record, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), record); return err }); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			events, err := delivery.NotificationEvents(t.Context(), org)
			if err != nil || len(events) != 1 || events[0].ID != "event_after_seek" || events[0].ProjectSequence != 30005 {
				t.Fatalf("after seek events=%+v error=%v", events, err)
			}
		}
		events, err := delivery.NotificationEvents(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		if done, _, err := delivery.QueueNotification(t.Context(), org, events[0], accountstore.IssueNotice{StateTo: "Done"}, "", true, 256); err != nil || !done {
			t.Fatalf("after seek done=%v error=%v", done, err)
		}
		if events, err := delivery.NotificationEvents(t.Context(), org); err != nil || len(events) != 0 {
			t.Fatalf("after seek duplicate events=%d error=%v", len(events), err)
		}
	}
}

func TestReviewNotificationCrossProjectStarvation(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	delivery := any(repository).(accountstore.NotificationRepository)
	content := eventstore.NewContent(newHistoryObjects())
	org := tracker.OrganizationID("org_first")
	seedNotificationMembers(t, repository, org)
	appendEvent := func(id string, project tracker.ProjectID, item tracker.NativeWorkItemID, offset int) {
		value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: item, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "worker"}, RecordedAt: historyAt().Add(time.Duration(offset) * time.Second), Data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}}
		row, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), row); return err }); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("event_second", "prj_second", "wi_second_1", 0)
	for tick := range 20 {
		appendEvent(fmt.Sprintf("event_first_%d", tick), "prj_first", "wi_first_1", tick+1)
		events, err := delivery.NotificationEvents(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.ProjectID == "prj_second" {
				t.Logf("prj_second discovered at tick %d", tick)
				return
			}
			if _, _, err := delivery.QueueNotification(t.Context(), org, event, accountstore.IssueNotice{}, "", false, 256); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("prj_second event never discovered across 20 ticks while prj_first stays active")
}
