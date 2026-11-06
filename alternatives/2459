package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBillingCreditsKeepTenantBalancesAndReceipts(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: org, Mode: "live"})
		}); err != nil {
			t.Fatal(err)
		}
		payment := billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "payment_example", AmountMicros: 1500, Kind: "purchase", RecordedAt: historyAt()}
		for _, test := range []struct {
			name    string
			amount  int64
			applied bool
			wantErr error
		}{
			{name: "first payment", amount: 1500, applied: true},
			{name: "same payment", amount: 1500},
			{name: "changed payment", amount: 1600, wantErr: mutation.ErrConflict},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				payment.AmountMicros = test.amount
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					applied, err := w.PostBillingCredit(t.Context(), payment)
					if applied != test.applied {
						t.Errorf("applied=%v want %v", applied, test.applied)
					}
					return err
				})
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("payment error=%v want %v", err, test.wantErr)
				}
			})
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {

			account, err := r.BillingCreditAccount(t.Context(), "live")
			if err != nil {
				return err
			}
			if account.OrganizationID != org || account.BalanceMicros != 1500 {
				return fmt.Errorf("unexpected balance or tenant: %+v", account)
			}
			rows, err := r.BillingCreditTransactions(t.Context(), "live", 0, 20)
			if err == nil && (len(rows) != 1 || rows[0].AmountMicros != 1500) {
				return fmt.Errorf("payment replay changed ledger: %+v", rows)
			}
			if err != nil {
				return err
			}
			recent, err := r.BillingRecentCreditTransactions(t.Context(), "live", 50)
			if err == nil && (len(recent) != 1 || recent[0].AmountMicros != 1500 || recent[0].Kind != "purchase") {
				return fmt.Errorf("recent billing history=%+v", recent)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBillingCreditReasonUsesImmutableObjectReference(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_credit_reason")
	seedTenantAccess(t, repository, org)
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: org, Mode: "live"})
	}); err != nil {
		t.Fatal(err)
	}
	makeReference := func(value string) billingstore.AuditReference {
		digest := sha256.Sum256([]byte(value))
		digestText := hex.EncodeToString(digest[:])
		key, err := attachment.ArchiveKey(string(org), digestText)
		if err != nil {
			t.Fatal(err)
		}
		return billingstore.AuditReference{Key: key, SHA256: digestText, CompressedBytes: 1, ContentBytes: int64(len(value)), Codec: "gzip", FormatVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	}
	reference := makeReference("Example credit adjustment reason")
	row := billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "adjustment:example", AmountMicros: 500, Kind: "complimentary", Actor: "operator_example", ReasonReference: reference, RecordedAt: historyAt()}
	for _, test := range []struct {
		row     billingstore.CreditTransaction
		applied bool
		wantErr error
	}{
		{row: row, applied: true},
		{row: row},
		{row: func() billingstore.CreditTransaction {
			invalid := row
			invalid.Source = "adjustment:invalid-reference"
			invalid.ReasonReference = billingstore.AuditReference{SHA256: reference.SHA256}
			return invalid
		}(), wantErr: billingstore.ErrInvalidAmount},
		{row: func() billingstore.CreditTransaction {
			changed := row
			changed.ReasonReference = makeReference("Changed reason")
			return changed
		}(), wantErr: mutation.ErrConflict},
	} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			applied, err := w.PostBillingCredit(t.Context(), test.row)
			if applied != test.applied {
				t.Errorf("applied=%t want %t", applied, test.applied)
			}
			return err
		})
		if !errors.Is(err, test.wantErr) {
			t.Fatalf("reasoned credit write error=%v want %v", err, test.wantErr)
		}
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.BillingRecentCreditTransactions(t.Context(), "live", 10)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ReasonReference != reference || rows[0].Reason != "" {
			return fmt.Errorf("credit reason storage=%+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingIncludedCreditsSerializeSpending(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_first")
	seedTenantAccess(t, repository, org)
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: org, Mode: "live"})
	}); err != nil {
		t.Fatal(err)
	}
	period := billingstore.CreditPeriod{OrganizationID: org, Mode: "live", StartsAt: historyAt(), EndsAt: historyAt().Add(24 * time.Hour), AllowanceMicros: 100}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.SpendBillingIncludedCredit(t.Context(), period, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var second *repositoryTask
	var firstUsed, secondUsed int64
	err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		firstUsed, err = w.SpendBillingIncludedCredit(t.Context(), period, 80)
		if err != nil {
			return err
		}
		second = startRepositoryTask(t, func(ctx context.Context) error {
			return repository.WriteTenant(ctx, org, corestore.Background, func(other tenantstore.Writer) error {
				var err error
				secondUsed, err = other.SpendBillingIncludedCredit(ctx, period, 80)
				return err
			})
		})
		waitLeaseLock(t, admin, 1, second)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.wait(t); err != nil {
		t.Fatal(err)
	}
	if firstUsed != 80 || secondUsed != 20 {
		t.Fatalf("concurrent included spends=%d,%d want 80,20", firstUsed, secondUsed)
	}
}

func TestBillingUsageCreditReplayKeepsAllowanceAndLedgerAtomic(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_usage_receipts")
	seedTenantAccess(t, repository, org)
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, mode := range []string{"live", "test"} {
			if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: org, Mode: mode, BalanceMicros: 1000}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := historyAt()
	period := billingstore.CreditPeriod{OrganizationID: org, Mode: "live", StartsAt: start, EndsAt: start.AddDate(0, 1, 0), AllowanceMicros: 100}
	makeReasonReference := func(value string) billingstore.AuditReference {
		digest := sha256.Sum256([]byte(value))
		digestText := hex.EncodeToString(digest[:])
		key, err := attachment.ArchiveKey(string(org), digestText)
		if err != nil {
			t.Fatal(err)
		}
		return billingstore.AuditReference{Key: key, SHA256: digestText, CompressedBytes: 1, ContentBytes: int64(len(value)), Codec: "gzip", FormatVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	}
	reasonReference := makeReasonReference("Example usage reason")
	first := billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "usage:turn_first", AmountMicros: -80, Kind: "usage", Actor: "worker_example", ReasonReference: reasonReference, RecordedAt: start.Add(time.Hour)}
	second := billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "usage:turn_second", AmountMicros: -80, Kind: "usage", Actor: "worker_example", ReasonReference: reasonReference, RecordedAt: start.Add(2 * time.Hour)}
	for _, item := range []struct {
		row     billingstore.CreditTransaction
		applied bool
	}{{first, true}, {first, false}, {second, true}} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			applied, err := w.PostBillingUsageCredit(t.Context(), period, item.row, -item.row.AmountMicros)
			if applied != item.applied {
				t.Errorf("applied=%v want %v", applied, item.applied)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		name   string
		period billingstore.CreditPeriod
		row    billingstore.CreditTransaction
		amount int64
		want   error
	}{
		{name: "cross-mode allowance", period: billingstore.CreditPeriod{OrganizationID: org, Mode: "test", StartsAt: period.StartsAt, EndsAt: period.EndsAt, AllowanceMicros: 100}, row: billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "usage:cross_mode", AmountMicros: -50, Kind: "usage", RecordedAt: start.Add(3 * time.Hour)}, want: billingstore.ErrInvalidAmount},
		{name: "gross amount mismatch", period: period, row: billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "usage:wrong_amount", AmountMicros: -50, Kind: "usage", RecordedAt: start.Add(3 * time.Hour)}, amount: 49, want: billingstore.ErrInvalidAmount},
		{name: "minimum integer amount", period: period, row: billingstore.CreditTransaction{OrganizationID: org, Mode: "live", Source: "usage:minimum_amount", AmountMicros: -1 << 63, Kind: "usage", RecordedAt: start.Add(3 * time.Hour)}, want: billingstore.ErrInvalidAmount},
		{name: "changed replay actor", period: period, row: func() billingstore.CreditTransaction { row := first; row.Actor = "different_actor"; return row }(), want: mutation.ErrConflict},
		{name: "changed replay reason", period: period, row: func() billingstore.CreditTransaction {
			row := first
			row.ReasonReference = makeReasonReference("Different usage reason")
			return row
		}(), want: mutation.ErrConflict},
	} {
		t.Run(item.name, func(t *testing.T) {
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				amount := item.amount
				if amount == 0 {
					amount = -item.row.AmountMicros
				}
				_, err := w.PostBillingUsageCredit(t.Context(), item.period, item.row, amount)
				return err
			})
			if !errors.Is(err, item.want) {
				t.Fatalf("invalid usage credit write error=%v want %v", err, item.want)
			}
		})
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		account, err := r.BillingCreditAccount(t.Context(), "live")
		if err != nil {
			return err
		}
		if account.BalanceMicros != 940 {
			return fmt.Errorf("balance=%d want 940", account.BalanceMicros)
		}
		period, err := r.BillingCreditPeriod(t.Context(), "live", period.StartsAt, period.EndsAt)
		if err != nil {
			return err
		}
		if period.UsedMicros != 100 {
			return fmt.Errorf("included used=%d want 100", period.UsedMicros)
		}
		_, total, history, err := r.BillingCreditHistory(t.Context(), "live", 0, start.UnixMicro(), start.AddDate(0, 1, 0).UnixMicro(), 253402300799999999, int64(^uint64(0)>>1), 10)
		if err != nil {
			return err
		}
		if total != 1 || len(history) != 1 || history[0].AmountMicros != -160 || history[0].IncludedAmountMicros != 100 || history[0].Turns != 2 || history[0].ReasonReference != reasonReference {
			return fmt.Errorf("unexpected grouped history total=%d rows=%+v", total, history)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingUsageReceiptAppliesAllowanceOnce(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_first")
	seedTenantAccess(t, repository, org)
	window := historyAt().Truncate(time.Hour)
	first := billingstore.UsageReceipt{OrganizationID: org, ID: "mutation_example", WindowStart: window, Metric: "api_mutations", Amount: 7}
	denied := billingstore.UsageReceipt{OrganizationID: org, ID: "mutation_denied", WindowStart: window, Metric: "api_mutations", Amount: 4}
	var result billingstore.UsageResult
	err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		result, err = w.ApplyBillingUsage(t.Context(), first, 10)
		if err != nil || !result.Applied || !result.Allowed || result.Used != 7 {
			return fmt.Errorf("first usage result=%+v err=%v", result, err)
		}
		result, err = w.ApplyBillingUsage(t.Context(), first, 10)
		if err != nil || result.Applied || !result.Allowed || result.Used != 7 {
			return fmt.Errorf("usage replay result=%+v err=%v", result, err)
		}
		result, err = w.ApplyBillingUsage(t.Context(), denied, 10)
		if err != nil || result.Applied || result.Allowed || result.Used != 7 {
			return fmt.Errorf("over allowance result=%+v err=%v", result, err)
		}
		result, err = w.ApplyBillingUsage(t.Context(), denied, 12)
		if err != nil || !result.Applied || !result.Allowed || result.Used != 11 {
			return fmt.Errorf("admitted usage result=%+v err=%v", result, err)
		}
		conflict := first
		conflict.Amount++
		if _, err := w.ApplyBillingUsage(t.Context(), conflict, 20); !errors.Is(err, billingstore.ErrUsageReceiptConflict) {
			return fmt.Errorf("changed receipt error=%v", err)
		}
		used, err := w.BillingUsage(t.Context(), window, "api_mutations")
		if err == nil && used != 11 {
			return fmt.Errorf("usage window=%d want 11", used)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
