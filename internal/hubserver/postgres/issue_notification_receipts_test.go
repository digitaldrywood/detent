package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNotificationReceiptsRetryOnlyDefiniteRejections(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	now := historyAt()
	claimed := map[tracker.OrganizationID][]accountstore.IssueNotification{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedNotificationMembers(t, repository, org)
		tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
		if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", string(org)); err != nil {
			t.Fatal(err)
		}
		queries := pgquery.New(tx)
		for _, user := range []string{"first", "second"} {
			if err := queries.EnsureIssueSubscription(t.Context(), pgquery.EnsureIssueSubscriptionParams{OrganizationID: org, WorkItemID: "wi_first_1", UserID: user, Token: "token_" + user}); err != nil {
				t.Fatal(err)
			}
			if err := queries.AppendIssueNotification(t.Context(), pgquery.AppendIssueNotificationParams{OrganizationID: org, WorkItemID: "wi_first_1", UserID: user, EventID: "event_example", SourceItem: "wi_first_1", DueAt: now, StateTo: "Done"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, status := range []string{"rejected", "uncertain"} {
			notification, found, err := repository.ClaimNotification(t.Context(), org, now)
			if err != nil || !found || notification.DeliveryID == "" || notification.Attempts != 1 || len(notification.Notices) != 1 {
				t.Fatalf("claim=%+v found=%v err=%v", notification, found, err)
			}
			claimed[org] = append(claimed[org], notification)
			if err := repository.FinishNotification(t.Context(), org, notification.DeliveryID, notification.Attempts, status, now.Add(5*time.Minute), now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for index, notification := range claimed[org] {
			receipt, err := repository.NotificationReceipt(t.Context(), org, notification.DeliveryID)
			if err != nil || receipt.Status != []string{"rejected", "uncertain"}[index] || receipt.Attempts != 1 || receipt.NoticeCount != 1 {
				t.Fatalf("reopened receipt=%+v err=%v", receipt, err)
			}
			foreign := tracker.OrganizationID("org_first")
			if org == foreign {
				foreign = "org_second"
			}
			if _, err := repository.NotificationReceipt(t.Context(), foreign, notification.DeliveryID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("foreign receipt read=%v", err)
			}
			if err := repository.FinishNotification(t.Context(), foreign, notification.DeliveryID, 1, "sent", now, now); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("foreign receipt write=%v", err)
			}
		}
		if _, found, err := repository.ClaimNotification(t.Context(), org, now.Add(4*time.Minute)); err != nil || found {
			t.Fatalf("early retry found=%v err=%v", found, err)
		}
		if org == "org_second" {
			if err := repository.UnsubscribeNotification(t.Context(), org, claimed[org][0].Token); err != nil {
				t.Fatal(err)
			}
		}
		retry, found, err := repository.ClaimNotification(t.Context(), org, now.Add(6*time.Minute))
		if err != nil || !found || retry.DeliveryID != claimed[org][0].DeliveryID || retry.Attempts != 2 {
			t.Fatalf("retry=%+v found=%v err=%v", retry, found, err)
		}
		if err := repository.FinishNotification(t.Context(), org, retry.DeliveryID, 1, "sent", now, now); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale send result=%v", err)
		}
		status := "suppressed"
		if org == "org_first" {
			if retry.Email != claimed[org][0].Email || len(retry.Notices) != 1 || retry.Notices[0].StateTo != "Done" {
				t.Fatalf("retry lost recipient or content: %+v", retry)
			}
			if err := repository.FinishNotification(t.Context(), org, retry.DeliveryID, 2, "sent", now, now); err != nil {
				t.Fatal(err)
			}
			status = "sent"
		} else if retry.Email != "" {
			t.Fatal("unsubscribed recipient was delivered")
		}
		receipt, err := repository.NotificationReceipt(t.Context(), org, retry.DeliveryID)
		if err != nil || receipt.Status != status || receipt.Attempts != 2 {
			t.Fatalf("final receipt=%+v err=%v", receipt, err)
		}
		var retained int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_notification_receipt_notices WHERE organization_id=$1 AND receipt_id=$2`, org, retry.DeliveryID).Scan(&retained); err != nil || retained != 0 {
			t.Fatalf("completed payload rows=%d err=%v", retained, err)
		}
		if _, found, err := repository.ClaimNotification(t.Context(), org, now.Add(24*time.Hour)); err != nil || found {
			t.Fatalf("uncertain or completed replay found=%v err=%v", found, err)
		}
	}
}
