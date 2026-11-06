package postgres_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNotificationSchemaKeepsTwoRecipientsPerTenant(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
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
		q := pgquery.New(tx)
		for _, user := range []string{"first", "second"} {
			if err := q.EnsureIssueSubscription(t.Context(), pgquery.EnsureIssueSubscriptionParams{OrganizationID: org, WorkItemID: "wi_first_1", UserID: user, Token: "token_" + user}); err != nil {
				t.Fatal(err)
			}
			if err := q.AppendIssueNotification(t.Context(), pgquery.AppendIssueNotificationParams{OrganizationID: org, WorkItemID: "wi_first_1", UserID: user, EventID: "event_example", SourceItem: "wi_first_1", DueAt: historyAt(), StateTo: "Done"}); err != nil {
				t.Fatal(err)
			}
		}
		for _, user := range []string{"first", "second"} {
			rows, err := q.TakeIssueNotices(t.Context(), pgquery.TakeIssueNoticesParams{OrganizationID: org, WorkItemID: "wi_first_1", UserID: user})
			if err != nil || len(rows) != 1 || rows[0].UserID != user || rows[0].StateTo != "Done" {
				t.Fatalf("notices=%+v error=%v", rows, err)
			}
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func seedNotificationMembers(t *testing.T, repository *postgres.Repository, org tracker.OrganizationID) {
	t.Helper()
	seedRepository(t, repository, org)
	seedTenantAccess(t, repository, org)
	err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, user := range []string{"first", "second"} {
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: user, ProjectID: "prj_first"}); err != nil {
				return err
			}
		}
		for index, user := range []string{"first", "second"} {
			id := tracker.NativeWorkItemID("wi_first_1")
			if index == 1 {
				id = "wi_first_2"
			}
			item, err := w.WorkItem(t.Context(), "prj_first", id)
			if err != nil {
				return err
			}
			item.ActorPrincipalID = &user
			if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
