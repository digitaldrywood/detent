package postgres_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCreditHistoryImportPreservesTenantLedgerAndAdvancesSequence(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	objects := newHistoryObjects()
	organizations := []tracker.OrganizationID{"org_credit_first", "org_credit_second"}
	imports := make(map[tracker.OrganizationID]postgres.CreditHistoryImportSource, len(organizations))
	for _, organization := range organizations {
		seedTenantAccess(t, repository, organization)
		copyPath := creditHistorySourceCopy(t, catalog, organization)
		imports[organization] = creditHistoryImportSource(copyPath, catalog, organization, objects)
	}
	for _, organization := range organizations {
		source := imports[organization]
		for replay := range 2 {
			result, err := repository.ImportCreditHistory(t.Context(), source)
			if err != nil {
				t.Fatalf("import %s replay %d: %v", organization, replay, err)
			}
			if result != (postgres.CreditHistoryImportResult{Accounts: 2, Packs: 2, Transactions: 2}) {
				t.Fatalf("import %s replay %d result=%+v", organization, replay, result)
			}
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		repository, _ = repositoryFor(t, admin, smallPool())
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			second, err := reader.BillingCreditAccount(t.Context(), "test")
			if err != nil {
				return err
			}
			if second.BalanceMicros != 900 || second.PriceID != "price_second" {
				return fmt.Errorf("second credit account=%+v", second)
			}
			secondPack, err := reader.CreditPack(t.Context(), "price_second", "test")
			if err != nil {
				return err
			}
			if secondPack.USDCents != 48 {
				return fmt.Errorf("second credit pack=%+v", secondPack)
			}
			secondTransaction, err := reader.BillingCreditTransactionBySource(t.Context(), "test", "legacy_same_source")
			if err != nil {
				return err
			}
			if secondTransaction.ID != 44 || secondTransaction.AmountMicros != 80 || secondTransaction.IncludedAmountMicros != 6 || secondTransaction.ReasonReference.Key == "" {
				return fmt.Errorf("second credit transaction=%+v", secondTransaction)
			}
			account, err := reader.BillingCreditAccount(t.Context(), "live")
			if err != nil {
				return err
			}
			if account.OrganizationID != organization || account.BalanceMicros != 650 || !account.AutoEnabled || account.ThresholdCents != 24 || account.PriceID != "price_example" || account.PaymentMethod != "method_example" || account.Failure != "" {
				return fmt.Errorf("imported credit account=%+v", account)
			}
			pack, err := reader.CreditPack(t.Context(), "price_example", "live")
			if err != nil {
				return err
			}
			if pack.OrganizationID != organization || pack.USDCents != 24 {
				return fmt.Errorf("imported credit pack=%+v", pack)
			}
			rows, err := reader.BillingCreditTransactions(t.Context(), "live", 0, 10)
			if err != nil {
				return err
			}
			if len(rows) != 1 || rows[0].ID != 43 || rows[0].Source != "legacy_same_source" || rows[0].AmountMicros != 70 || rows[0].IncludedAmountMicros != 5 || rows[0].Kind != "complimentary" || rows[0].Actor != "operator_example" || !rows[0].RecordedAt.Equal(historyAt()) || rows[0].ReasonReference.Key == "" {
				return fmt.Errorf("imported credit ledger=%+v", rows)
			}
			reasonBytes, err := objects.ReadDetail(t.Context(), string(organization), rows[0].ReasonReference.ArchiveReference())
			if err != nil {
				return err
			}
			var reason struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(reasonBytes, &reason); err != nil {
				return err
			}
			if reason.Reason != "legacy credit reason" {
				return fmt.Errorf("imported archived reason=%q", reason.Reason)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		transaction := billingstore.CreditTransaction{OrganizationID: organization, Mode: "live", Source: "post_import", AmountMicros: 7, Kind: "purchase", Actor: "operator_example", RecordedAt: historyAt().Add(time.Second)}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			applied, err := writer.PostBillingCredit(t.Context(), transaction)
			if err == nil && !applied {
				return fmt.Errorf("post-import transaction was not applied")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			row, err := reader.BillingCreditTransactionBySource(t.Context(), "live", "post_import")
			if err != nil {
				return err
			}
			if row.ID <= 44 || row.AmountMicros != 7 {
				return fmt.Errorf("post-import credit transaction=%+v", row)
			}
			account, err := reader.BillingCreditAccount(t.Context(), "live")
			if err != nil {
				return err
			}
			if account.BalanceMicros != 657 {
				return fmt.Errorf("post-import credit balance=%d", account.BalanceMicros)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreditHistoryImportRejectsUnsupportedPeriodsAndPurchasesBeforeWrites(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		table string
	}{
		{name: "periods", table: "ai_credit_periods"},
		{name: "purchases", table: "ai_credit_purchases"},
	} {
		t.Run(test.name, func(t *testing.T) {
			organization := tracker.OrganizationID("org_credit_unsupported_" + test.name)
			seedTenantAccess(t, repository, organization)
			copy := newNeutralBillingFixture(t, organization, "ai_credit_accounts", "ai_credit_packs", "ai_credit_transactions", "ai_credit_periods", "ai_credit_purchases")
			seedCreditHistorySource(t, copy, organization, "")
			switch test.table {
			case "ai_credit_periods":
				copy.add(t, "ai_credit_periods", map[string]any{"organization_id": organization, "mode": "live", "period_start": int64(1), "period_end": int64(2), "allowance_micros": int64(100), "used_micros": int64(0)})
			case "ai_credit_purchases":
				copy.add(t, "ai_credit_purchases", map[string]any{"purchase_key": "purchase_example", "organization_id": organization, "mode": "live", "price_id": "price_example", "usd_cents": int64(24), "automatic": int64(0), "payment_method": "method_example", "state": "pending", "payment_id": "", "session_json": "{}", "created_at": historyAt().UnixMicro()})
			}
			path := copy.seal(t)
			objects := newHistoryObjects()
			_, err = repository.ImportCreditHistory(t.Context(), creditHistoryImportSource(path, catalog, organization, objects))
			if err == nil {
				t.Fatal("unsupported source rows were accepted")
			}
			var accounts, transactions, receipts int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM ai_credit_accounts WHERE organization_id=$1`, organization).Scan(&accounts); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM ai_credit_transactions WHERE organization_id=$1`, organization).Scan(&transactions); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM credit_history_imports WHERE organization_id=$1`, organization).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if accounts != 0 || transactions != 0 || receipts != 0 {
				t.Fatalf("unsupported import wrote accounts=%d transactions=%d import receipts=%d", accounts, transactions, receipts)
			}
		})
	}
}

func creditHistorySourceCopy(t *testing.T, catalog storagecatalog.Catalog, organization tracker.OrganizationID) string {
	t.Helper()
	copy := newNeutralBillingFixture(t, organization, "ai_credit_accounts", "ai_credit_packs", "ai_credit_transactions", "ai_credit_periods", "ai_credit_purchases")
	seedCreditHistorySource(t, copy, organization, "legacy credit reason")
	return copy.seal(t)
}

func seedCreditHistorySource(t *testing.T, fixture *neutralBillingFixture, organization tracker.OrganizationID, reason string) {
	t.Helper()
	fixture.add(t, "ai_credit_accounts", map[string]any{"organization_id": organization, "mode": "live", "balance_micros": int64(650), "auto_enabled": int64(1), "threshold_cents": int64(24), "price_id": "price_example", "payment_method": "method_example", "failure": ""})
	fixture.add(t, "ai_credit_packs", map[string]any{"price_id": "price_example", "mode": "live", "usd_cents": int64(24)})
	fixture.add(t, "ai_credit_transactions", map[string]any{"id": int64(43), "organization_id": organization, "mode": "live", "source": "legacy_same_source", "amount_micros": int64(70), "included_amount_micros": int64(5), "kind": "complimentary", "actor": "operator_example", "reason": reason, "recorded_at": historyAt().UnixMicro()})
	fixture.add(t, "ai_credit_accounts", map[string]any{"organization_id": organization, "mode": "test", "balance_micros": int64(900), "auto_enabled": int64(0), "threshold_cents": int64(48), "price_id": "price_second", "payment_method": "method_second", "failure": ""})
	fixture.add(t, "ai_credit_packs", map[string]any{"price_id": "price_second", "mode": "test", "usd_cents": int64(48)})
	fixture.add(t, "ai_credit_transactions", map[string]any{"id": int64(44), "organization_id": organization, "mode": "test", "source": "legacy_same_source", "amount_micros": int64(80), "included_amount_micros": int64(6), "kind": "complimentary", "actor": "operator_example", "reason": reason, "recorded_at": historyAt().UnixMicro()})
}

func creditHistoryImportSource(copyPath string, catalog storagecatalog.Catalog, organization tracker.OrganizationID, objects attachment.DetailStore) postgres.CreditHistoryImportSource {
	return postgres.CreditHistoryImportSource{
		BundlePath: copyPath,
		Catalog:    catalog,
		Schema:     schemaFromCatalogForTest(catalog),
		Objects:    objects,
		Source: storagecatalog.Source{
			Schema:         "hub_copy",
			Store:          "hub",
			CatalogVersion: catalog.Version,
			OrganizationID: string(organization),
		},
	}
}
