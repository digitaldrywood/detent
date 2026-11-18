package hubtest

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func PlanCatalog(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, plans []billingstore.Plan) []billingstore.Plan {
	t.Helper()
	rows := make([]billingstore.Plan, len(plans))
	copy(rows, plans)
	write(t, repository, organization, "plan catalog", func(w tenantstore.Writer) error {
		for i := range rows {
			rows[i].OrganizationID = organization
			if err := w.CreateBillingPlan(t.Context(), rows[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return rows
}

type PlanAssignmentOptions struct {
	Base         billingstore.PlanReference
	Subscription *billingstore.PlanReference
	StartsAt     time.Time
	EndsAt       time.Time
	AccessUntil  time.Time
	Mode         string
}

func PlanAssignment(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, options PlanAssignmentOptions) billingstore.Assignment {
	t.Helper()
	assignment := billingstore.Assignment{OrganizationID: organization, Base: options.Base, Subscription: options.Subscription}
	if options.Subscription != nil {
		assignment.ExpiresAt = &options.AccessUntil
	}
	write(t, repository, organization, "plan assignment", func(w tenantstore.Writer) error {
		current, err := w.BillingAssignment(t.Context())
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		assignment.Revision = current.Revision
		if options.Subscription != nil {
			mode := options.Mode
			if mode == "" {
				mode = billing.ModeTest
			}
			account := billingstore.Account{OrganizationID: organization, Mode: mode, Binding: billing.Binding{OrganizationID: string(organization), AccountID: "acct_" + string(organization), CustomerID: "cus_" + string(organization)}, Status: "active", Plan: *options.Subscription, AccessUntil: options.AccessUntil, PaidThrough: options.EndsAt, Snapshot: billing.Snapshot{SubscriptionID: "sub_" + string(organization), Status: "active", PeriodStart: options.StartsAt, PeriodEnd: options.EndsAt}}
			if err := w.SetBillingAccount(t.Context(), account); err != nil {
				return err
			}
		}
		if err := w.SetBillingAssignment(t.Context(), assignment, current.Revision); err != nil {
			return err
		}
		assignment, err = w.BillingAssignment(t.Context())
		return err
	})
	return assignment
}

func CreditGrant(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, row billingstore.CreditTransaction) billingstore.CreditTransaction {
	t.Helper()
	row.OrganizationID = organization
	if row.Mode == "" {
		row.Mode = billing.ModeTest
	}
	if row.Source == "" {
		row.Source = "grant:" + hubapi.NewID("credit")
	}
	if row.Kind == "" {
		row.Kind = "complimentary"
	}
	row.RecordedAt = seedTime(row.RecordedAt)
	write(t, repository, organization, "credit grant", func(w tenantstore.Writer) error {
		if _, err := w.BillingCreditAccount(t.Context(), row.Mode); errors.Is(err, corestore.ErrNotFound) {
			if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: row.Mode}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := w.PostBillingCredit(t.Context(), row); err != nil {
			return err
		}
		var err error
		row, err = w.BillingCreditTransactionBySource(t.Context(), row.Mode, row.Source)
		return err
	})
	return row
}
