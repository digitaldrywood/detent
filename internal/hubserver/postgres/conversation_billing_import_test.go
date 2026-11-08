package postgres_test

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationBillingImportPreservesSnapshotWithoutRebilling(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	admin.SetMaxOpenConns(6)
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	copies := map[tracker.OrganizationID]string{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		copy := newNeutralBillingFixture(t, organization, "conversation_prices", "conversation_usage")
		copy.add(t, "conversation_prices", map[string]any{"id": int64(1), "provider": "example-provider", "model": "example-model", "effective_at": int64(1767225600000000), "input": 0.1, "cached_input": 0.01, "output": 0.5})
		copy.add(t, "conversation_prices", map[string]any{"id": int64(2), "provider": "example-provider", "model": "second-model", "effective_at": int64(1767225600000000), "input": 0.2, "cached_input": 0.02, "output": 0.6})
		for n := 0; n < 101; n++ {
			conversation := "conversation"
			if n == 100 {
				conversation = "late-conversation"
			}
			values := map[string]any{"organization_id": organization, "project_id": "prj_shared", "conversation_id": conversation, "turn_id": fmt.Sprintf("turn-%03d", n), "provider": "example-provider", "model": "example-model", "occurred_at": int64(1767225600000001), "input": int64(100), "cached_input": int64(20), "output": int64(30), "reasoning_output": int64(5), "outcome": "delivered", "price_id": int64(1), "cost_usd": 0.0000232, "cache_savings_usd": 0.0000018}
			if n == 50 {
				values["price_id"], values["cost_usd"], values["cache_savings_usd"] = nil, nil, nil
			}
			copy.add(t, "conversation_usage", values)
		}
		copies[organization] = copy.seal(t)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, organization)
		for _, query := range []string{
			`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'owner','owner','hash','fingerprint','operator',true,now(),now())`,
			`INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status) VALUES($1,'conversation','prj_shared','owner','user','private','active')`,
		} {
			if _, err := admin.ExecContext(t.Context(), query, organization); err != nil {
				t.Fatal(err)
			}
		}
		source := postgres.ConversationBillingImportSource{BundlePath: copies[organization], Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Objects: &costImportObjects{historyObjects: newHistoryObjects()}, Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}}
		if _, err := repository.ImportConversationBilling(t.Context(), source); err == nil {
			t.Fatal("missing conversation accepted")
		}
		changedCopy := newNeutralBillingFixture(t, organization, "conversation_prices", "conversation_usage")
		changedCopy.add(t, "conversation_usage", map[string]any{"organization_id": organization, "project_id": "prj_shared", "conversation_id": "conversation", "turn_id": "changed", "provider": "example-provider", "model": "example-model", "occurred_at": int64(1767225600000001), "input": int64(1), "cached_input": int64(0), "output": int64(1), "reasoning_output": int64(0), "outcome": "delivered", "price_id": nil, "cost_usd": nil, "cache_savings_usd": nil})
		changed := source
		changed.BundlePath = changedCopy.seal(t)
		if _, err := repository.ImportConversationBilling(t.Context(), changed); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("partial import accepted changed snapshot: %v", err)
		}
		var partial int
		var completed bool
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_usage WHERE organization_id=$1", organization).Scan(&partial); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRowContext(t.Context(), "SELECT completed FROM conversation_billing_imports WHERE organization_id=$1", organization).Scan(&completed); err != nil {
			t.Fatal(err)
		}
		if partial != 100 || completed {
			t.Fatalf("partial import rows=%d completed=%v", partial, completed)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status) VALUES($1,'late-conversation','prj_shared','owner','user','private','active')`, organization); err != nil {
			t.Fatal(err)
		}
		for replay := 0; replay < 2; replay++ {
			got, err := repository.ImportConversationBilling(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if got.Prices != 2 || got.Usage != 101 {
				t.Fatalf("import counts=%+v", got)
			}
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		repository, _ = repositoryFor(t, admin, smallPool())
		var prices int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_prices WHERE organization_id=$1", organization).Scan(&prices); err != nil || prices != 2 {
			t.Fatalf("tenant prices=%d: %v", prices, err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.ConversationUsage(t.Context(), "turn-100")
			if err != nil {
				return err
			}
			if got.OrganizationID != organization || got.Input != 100 || got.ReasoningOutput != 5 || got.CostUSD == nil || *got.CostUSD != 0.0000232 || got.PriceID == nil || *got.PriceID != 1 || got.OccurredAt.UnixMicro() != 1767225600000001 {
				t.Fatalf("historical usage changed: %+v", got)
			}
			unpriced, err := r.ConversationUsage(t.Context(), "turn-050")
			if err != nil {
				return err
			}
			if unpriced.PriceID != nil || unpriced.CostUSD != nil || unpriced.CacheSavingsUSD != nil {
				t.Fatal("unpriced historical usage was repriced")
			}
			summary, err := r.ConversationUsageSummary(t.Context(), time.UnixMicro(1767225600000000), time.UnixMicro(1767225600000010), nil)
			if err != nil {
				return err
			}
			if summary.Turns != 101 {
				t.Fatalf("snapshot/replay turns=%d", summary.Turns)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var debits int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM ai_credit_transactions WHERE organization_id=$1", organization).Scan(&debits); err != nil {
			t.Fatal(err)
		}
		if debits != 0 {
			t.Fatalf("historical usage rebilled: %d", debits)
		}
		if _, err := repository.ImportConversationBilling(t.Context(), changed); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed source snapshot error=%v", err)
		}
		var priceRows, usageRows int64
		if err := admin.QueryRowContext(t.Context(), "SELECT completed,price_rows,usage_rows FROM conversation_billing_imports WHERE organization_id=$1", organization).Scan(&completed, &priceRows, &usageRows); err != nil {
			t.Fatal(err)
		}
		if !completed || priceRows != 2 || usageRows != 101 {
			t.Fatalf("completion receipt completed=%v prices=%d usage=%d", completed, priceRows, usageRows)
		}
		if _, err := repository.ImportConversationBilling(t.Context(), changed); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed source copy accepted after completion: %v", err)
		}
	}
}
