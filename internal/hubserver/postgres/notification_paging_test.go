package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNotificationRelatedRecipientPagingResumesWithoutDuplicates(t *testing.T) {
	var candidate *postgres.Repository
	if _, ok := any(candidate).(accountstore.NotificationCollector); !ok {
		t.Fatal("PostgreSQL notification collector is missing")
	}
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	delivery := any(repository).(accountstore.NotificationCollector)
	org := tracker.OrganizationID("org_paging")
	seedNotificationMembers(t, repository, org)
	content := eventstore.NewContent(newHistoryObjects())
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for index := range 256 {
			user := fmt.Sprintf("participant_%03d", index)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: user, Name: user, Hash: "hash_" + user, Scope: "operator", NativeOnly: true, CreatedAt: historyAt(), UpdatedAt: historyAt(), ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: "member", Active: true, PrincipalID: user, NotifyIssues: true, CreatedAt: historyAt(), UpdatedAt: historyAt()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: user, ProjectID: "prj_first"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index := range 256 {
		user := fmt.Sprintf("participant_%03d", index)
		value := tracker.CollaborationEvent{ID: "event_" + user, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_2", Type: "comment.created", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: user}, RecordedAt: historyAt(), Data: tracker.CollaborationData{CommentID: "comment_" + user}}
		record, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), record); return err }); err != nil {
			t.Fatal(err)
		}
	}
	value := tracker.CollaborationEvent{ID: "event_related", OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: "issue.edited", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "worker"}, RecordedAt: historyAt().Add(time.Second), Data: tracker.CollaborationData{Operation: "add", Fields: []string{"related"}, RelatedWorkItemID: "wi_first_2"}}
	record, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
		var err error
		record, err = w.AppendEvent(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		done  bool
		count int
	}{{false, 256}, {true, 2}, {true, 0}} {
		done, count, err := delivery.QueueNotification(t.Context(), org, record, accountstore.IssueNotice{Related: true}, "wi_first_2", true, 256)
		if err != nil || done != test.done || count != test.count {
			t.Fatalf("done=%v count=%d error=%v", done, count, err)
		}
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", org); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_notification_notices WHERE organization_id=$1", org).Scan(&notices); err != nil || notices != 258 {
		t.Fatalf("notices=%d error=%v", notices, err)
	}
}
