package postgres_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

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
	schema := storagecatalog.Schema{}
	for name, table := range catalog.Tables {
		if name, ok := strings.CutPrefix(name, "hub/"); ok {
			schema[name] = map[string]string{}
			for column, definition := range table.Columns {
				schema[name][column] = definition.Source
			}
		}
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
		sourceSchema := "billing_" + string(organization)
		if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+pq.QuoteIdentifier(sourceSchema)); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"conversation_prices", "conversation_usage"} {
			var columns []string
			for column := range schema[table] {
				columns = append(columns, pq.QuoteIdentifier(column)+` text COLLATE "C"`)
			}
			key := "id"
			if table == "conversation_usage" {
				key = "organization_id,turn_id"
			}
			if _, err := admin.ExecContext(t.Context(), "CREATE TABLE "+pq.QuoteIdentifier(sourceSchema)+"."+table+" ("+strings.Join(columns, ",")+",PRIMARY KEY ("+key+"))"); err != nil {
				t.Fatal(err)
			}
		}
		prices := pq.QuoteIdentifier(sourceSchema) + ".conversation_prices"
		usage := pq.QuoteIdentifier(sourceSchema) + ".conversation_usage"
		if _, err := admin.ExecContext(t.Context(), "INSERT INTO "+prices+"(id,provider,model,effective_at,input,cached_input,output) VALUES('1','openai','example','1767225600000000','0.1','0.01','0.5')"); err != nil {
			t.Fatal(err)
		}
		insertUsage := "INSERT INTO " + usage + "(organization_id,project_id,conversation_id,turn_id,provider,model,occurred_at,input,cached_input,output,reasoning_output,outcome,price_id,cost_usd,cache_savings_usd) VALUES($1,'prj_shared','conversation',$2,'openai','example','1767225600000001','100','20','30','5','delivered','1','0.0000232','0.0000018')"
		for n := 0; n < 101; n++ {
			if _, err := admin.ExecContext(t.Context(), insertUsage, organization, fmt.Sprintf("turn-%03d", n)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE "+usage+" SET price_id=NULL,cost_usd=NULL,cache_savings_usd=NULL WHERE turn_id='turn-050'"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE "+usage+" SET conversation_id='late-conversation' WHERE turn_id='turn-100'"); err != nil {
			t.Fatal(err)
		}
		source := postgres.ConversationBillingImportSource{DB: admin, Catalog: catalog, Schema: schema, Source: storagecatalog.Source{Schema: sourceSchema, Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}, SnapshotID: exportCostTestSnapshot(t, admin)}
		if _, err := admin.ExecContext(t.Context(), insertUsage, organization, "after-snapshot"); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.ImportConversationBilling(t.Context(), source); err == nil {
			t.Fatal("missing conversation accepted")
		}
		changed := source
		changed.SnapshotID = exportCostTestSnapshot(t, admin)
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
			if got.Prices != 1 || got.Usage != 101 {
				t.Fatalf("import counts=%+v", got)
			}
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
		if !completed || priceRows != 1 || usageRows != 101 {
			t.Fatalf("completion receipt completed=%v prices=%d usage=%d", completed, priceRows, usageRows)
		}
		source.SnapshotID = "expired-snapshot"
		if _, err := repository.ImportConversationBilling(t.Context(), source); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
}
