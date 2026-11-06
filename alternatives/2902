package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDailySummaryTimestampMigrationPreservesOrdering(t *testing.T) {
	admin := template.Database(t)
	for table, columns := range map[string][]string{"daily_summary_preferences": {"window_end", "due_at"}, "daily_summary_receipts": {"window_end", "recorded_at"}} {
		for _, column := range columns {
			var kind string
			if err := admin.QueryRowContext(t.Context(), "SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2", table, column).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			if kind != "timestamp with time zone" {
				t.Fatalf("%s.%s type=%s", table, column, kind)
			}
		}
	}
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261106024240, false); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for index, user := range []string{"first", "second"} {
			due := now.Add(time.Duration(1-index) * time.Second)
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO daily_summary_preferences(organization_id,user_id,enabled,token,window_end,due_at) VALUES($1,$2,true,$3,$4,$5)", org, user, string(org)+user, now.Unix(), due.Unix()); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO daily_summary_receipts(organization_id,user_id,window_end,status,recorded_at) VALUES($1,$2,$3,'sent',$3)", org, user, now.Unix()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261106024240, true); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		rows, err := admin.QueryContext(t.Context(), "SELECT user_id FROM daily_summary_preferences WHERE organization_id=$1 AND due_at<=$2 ORDER BY due_at,user_id", org, now)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for rows.Next() {
			var user string
			if err := rows.Scan(&user); err != nil {
				t.Fatal(err)
			}
			got = append(got, user)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "second" {
			t.Fatalf("due boundary=%v", got)
		}
		for _, user := range []string{"first", "second"} {
			preference, err := repository.DailySummaryPreference(t.Context(), org, user, "membership_"+user, user)
			if err != nil || preference.WindowEnd != now.Unix() {
				t.Fatalf("preference=%+v error=%v", preference, err)
			}
			var end, recorded time.Time
			if err := admin.QueryRowContext(t.Context(), "SELECT window_end,recorded_at FROM daily_summary_receipts WHERE organization_id=$1 AND user_id=$2", org, user).Scan(&end, &recorded); err != nil || !end.Equal(now) || !recorded.Equal(now) {
				t.Fatalf("receipt=%s/%s error=%v", end, recorded, err)
			}
		}
	}
}
