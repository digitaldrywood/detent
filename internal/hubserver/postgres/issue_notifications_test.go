package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNotificationDeliveryPreservesTenantAndRecipientScope(t *testing.T) {
	var candidate *postgres.Repository
	if _, ok := any(candidate).(accountstore.NotificationRepository); !ok {
		t.Fatal("PostgreSQL notification owner is missing")
	}
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
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
	}
}
