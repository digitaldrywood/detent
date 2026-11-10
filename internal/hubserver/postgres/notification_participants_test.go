package postgres_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNotificationRecipientsRespectParticipationAndOptOut(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	delivery := any(repository).(accountstore.NotificationRepository)
	content := eventstore.NewContent(newHistoryObjects())
	for index, test := range []struct {
		name, actorKind, actor, commentKind string
		disabled, revoked                   bool
		want                                []string
	}{
		{name: "worker notifies filer and commenter", actorKind: "machine", actor: "worker", commentKind: "human", want: []string{"first@example.com", "second@example.com"}},
		{name: "own action excluded", actorKind: "human", actor: "first", commentKind: "human", want: []string{"second@example.com"}},
		{name: "machine comment excluded", actorKind: "machine", actor: "worker", commentKind: "machine", want: []string{"first@example.com"}},
		{name: "preference disabled", actorKind: "machine", actor: "worker", disabled: true},
		{name: "access revoked", actorKind: "machine", actor: "worker", revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			org := tracker.OrganizationID(fmt.Sprintf("org_notice_%d", index))
			seedNotificationMembers(t, repository, org)
			appendEvent := func(id, kind, actorKind, actor string) eventstore.Event {
				value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: actorKind, PrincipalID: actor}, RecordedAt: historyAt(), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Done", CommentID: "comment_example"}}
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
				return record
			}
			if test.commentKind != "" {
				record := appendEvent("event_comment", "comment.created", test.commentKind, "second")
				if _, _, err := delivery.QueueNotification(t.Context(), org, record, accountstore.IssueNotice{}, "", false, 256); err != nil {
					t.Fatal(err)
				}
			}
			record := appendEvent("event_transition", "workflow.transitioned", test.actorKind, test.actor)
			if _, _, err := delivery.QueueNotification(t.Context(), org, record, accountstore.IssueNotice{StateFrom: "Todo", StateTo: "Done"}, "", true, 256); err != nil {
				t.Fatal(err)
			}
			if test.disabled || test.revoked {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if test.revoked {
						return w.DeleteAccessProjectGrant(t.Context(), "first", "prj_first")
					}
					member, err := w.AccessMember(t.Context(), "first")
					if err != nil {
						return err
					}
					member.NotifyIssues = false
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
			}
			got := []string{}
			for range 3 {
				notice, found, err := claimDueNotification(t, delivery, org, historyAt().Add(2*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if !found {
					break
				}
				if notice.Email != "" {
					got = append(got, notice.Email)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("recipients=%v want=%v", got, test.want)
			}
		})
	}
}
