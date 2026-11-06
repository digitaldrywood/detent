package postgres_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSummaryDeliveryClaimsAndTenantIsolation(t *testing.T) {
	var candidate *postgres.Repository
	if _, ok := any(candidate).(accountstore.SummaryDeliveryRepository); !ok {
		t.Fatal("PostgreSQL summary delivery owner is missing")
	}
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	delivery, ok := any(repository).(accountstore.SummaryDeliveryRepository)
	if !ok {
		t.Fatal("PostgreSQL summary delivery owner is missing")
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		for _, user := range []string{"first", "second"} {
			err := repository.SetDailySummaryPreference(t.Context(), org, user, "membership_"+user, user, accountstore.DailySummaryPreference{Enabled: true, TimeZone: "UTC", WindowEnd: now.Unix()}, string(org)+user, now.Unix())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		due, err := delivery.DueSummaries(t.Context(), org, now)
		if err != nil || len(due) != 2 {
			t.Fatalf("due=%+v error=%v", due, err)
		}
		for _, recipient := range due {
			if recipient.Organization != string(org) || !recipient.Eligible {
				t.Fatalf("recipient=%+v", recipient)
			}
			receipt := accountstore.SummaryReceipt{User: recipient.User, End: now, Status: "uncertain", RecordedAt: now, NextEnd: now.Add(24 * time.Hour), NextDue: now.Add(24 * time.Hour)}
			claimed, err := delivery.ClaimSummary(t.Context(), org, receipt)
			if err != nil || !claimed {
				t.Fatalf("claim=%v error=%v", claimed, err)
			}
			claimed, err = delivery.ClaimSummary(t.Context(), org, receipt)
			if err != nil || claimed {
				t.Fatalf("duplicate claim=%v error=%v", claimed, err)
			}
			receipt.Status = "rejected"
			if err := delivery.FinishSummary(t.Context(), org, receipt); err != nil {
				t.Fatal(err)
			}
			due, err := delivery.DueSummaries(t.Context(), org, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range due {
				if r.User == recipient.User {
					t.Fatal("rejected delivery ignored retry delay")
				}
			}
			receipt.RecordedAt = now.Add(time.Hour)
			receipt.Status = "uncertain"
			claimed, err = delivery.ClaimSummary(t.Context(), org, receipt)
			if err != nil || !claimed {
				t.Fatalf("retry claim=%v error=%v", claimed, err)
			}
			receipt.Status = "sent"
			if err := delivery.FinishSummary(t.Context(), org, receipt); err != nil {
				t.Fatal(err)
			}
			status, err := delivery.SummaryStatus(t.Context(), org, receipt.User, now)
			if err != nil || status != "sent" {
				t.Fatalf("status=%q error=%v", status, err)
			}
		}
	}

	for index, status := range []string{"quiet", "uncertain", "sent"} {
		t.Run(status, func(t *testing.T) {
			end := now.Add(time.Duration(index+2) * 24 * time.Hour)
			preference := accountstore.DailySummaryPreference{Enabled: true, TimeZone: "UTC", WindowEnd: end.Unix()}
			if err := repository.SetDailySummaryPreference(t.Context(), "org_first", "first", "membership_first", "first", preference, "unused_token", end.Unix()); err != nil {
				t.Fatal(err)
			}
			receipt := accountstore.SummaryReceipt{User: "first", End: end, Status: "uncertain", RecordedAt: end, NextEnd: end.Add(24 * time.Hour), NextDue: end.Add(24 * time.Hour)}
			if status == "quiet" {
				receipt.Status = status
			}
			if claimed, err := delivery.ClaimSummary(t.Context(), "org_first", receipt); err != nil || !claimed {
				t.Fatalf("claim=%v error=%v", claimed, err)
			}
			receipt.Status = status
			var err error
			if status == "quiet" {
				err = delivery.AdvanceSummary(t.Context(), "org_first", receipt)
			} else {
				err = delivery.FinishSummary(t.Context(), "org_first", receipt)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := repository.DailySummaryPreference(t.Context(), "org_first", "first", "membership_first", "first")
			if err != nil || got.WindowEnd != receipt.NextEnd.Unix() {
				t.Fatalf("preference=%+v error=%v", got, err)
			}
			receipt.Status = "uncertain"
			if claimed, err := delivery.ClaimSummary(t.Context(), "org_first", receipt); err != nil || claimed {
				t.Fatalf("replayed claim=%v error=%v", claimed, err)
			}
		})
	}
}
