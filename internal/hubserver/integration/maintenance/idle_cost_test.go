package maintenance_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIdleMaintenanceFamilyStatements(t *testing.T) {
	h := quietHub(t)
	for _, org := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		native := hubfixture.NewNative(t, h, org, "Example project")
		hubtest.Issue(t, h.Repository, h.Config.RunDetails, native.Project, hubtest.IssueOptions{Title: "Example issue", Now: h.Now()})
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		for _, family := range h.Service.ReadModelMaintenanceFeatures() {
			t.Run(string(org)+"/"+family.Name, func(t *testing.T) {
				budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: family.Bytes}
				ctx := budget.Context(t.Context(), tenant)
				for range 6 {
					if _, err := family.Step(ctx, tenant, budget); err != nil {
						t.Fatal(err)
					}
				}
				begins, statements := 0, 0
				ctx = postgres.WithTransactionTiming(ctx, func(name string, _ time.Time) {
					if name == "pg_begin" {
						begins++
					}
				}, func(time.Time, string) { statements++ })
				out, err := family.Step(ctx, tenant, budget)
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if family.Name == "github-intake" || family.Name == "github-reconciliation" {
					want = 0
				}
				if begins != 0 || statements != want || (out.Progress && !out.CursorOnly) {
					t.Errorf("idle turn began %d transactions, issued %d observed statements and reported progress=%t; want %d statements", begins, statements, out.Progress, want)
				}
			})
		}
	}

	families := h.Service.ReadModelMaintenanceFeatures()
	family := families[slices.IndexFunc(families, func(f hubmaintenance.Feature) bool { return f.Name == "integration-pages" })]
	check := func(org tracker.OrganizationID) int {
		t.Helper()
		var count int
		if err := h.Admin.QueryRowContext(t.Context(), "SELECT count(*) FROM project_integration_pages WHERE organization_id=$1", org).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	step := func(org tracker.OrganizationID) {
		t.Helper()
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: family.Bytes}
		if _, err := family.Step(budget.Context(t.Context(), tenant), tenant, budget); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Admin.ExecContext(t.Context(), "DELETE FROM project_integration_pages WHERE organization_id=$1", hubfixture.Primary); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", hubfixture.Secondary); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT * FROM public.project_sweep_due($1,'integration-pages','')",
		"SELECT public.github_outbound_due($1,now())",
		"SELECT public.github_outbound_deadline($1)",
		"SELECT * FROM public.claim_integration_page($1)",
		"SELECT public.maintenance_family_due($1,'board-dirty',now(),false)",
		"SELECT * FROM public.board_sweep_due($1,'')",
		"SELECT * FROM public.usage_sweep_due($1,'',1)",
	} {
		if _, err := tx.ExecContext(t.Context(), "SAVEPOINT probe"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.ExecContext(t.Context(), query, hubfixture.Primary)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Errorf("mismatched probe %s: %v", query, err)
		}
		cleanup := "RELEASE SAVEPOINT probe"
		if err != nil {
			cleanup = "ROLLBACK TO SAVEPOINT probe"
		}
		if _, err := tx.ExecContext(t.Context(), cleanup); err != nil {
			t.Fatal(err)
		}
	}
	var scope string
	if err := tx.QueryRowContext(t.Context(), "SELECT current_setting('detent.organization_id')").Scan(&scope); err != nil || scope != string(hubfixture.Secondary) {
		t.Fatalf("caller scope %q: %v", scope, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var queued bool
	if err := h.Admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT FROM project_integration_page_dirty WHERE organization_id=$1)", hubfixture.Primary).Scan(&queued); err != nil || queued {
		t.Fatalf("foreign dirty queue changed: queued=%t error=%v", queued, err)
	}
	step(hubfixture.Secondary)
	if check(hubfixture.Primary) != 0 || check(hubfixture.Secondary) != 1 {
		t.Fatal("secondary maintenance changed the primary projection")
	}
	for range 2 {
		step(hubfixture.Primary)
	}
	if check(hubfixture.Primary) != 1 || check(hubfixture.Secondary) != 1 {
		t.Fatal("maintenance did not restore the missing primary projection")
	}

	family = families[slices.IndexFunc(families, func(f hubmaintenance.Feature) bool { return f.Name == "github-webhooks" })]
	for _, org := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		if _, err := h.Admin.ExecContext(t.Context(), `INSERT INTO github_webhook_receipts(organization_id,delivery_id,repository,event_type,action,payload_digest,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,received_at,expires_at,next_attempt_at,status) VALUES($1,'same-delivery','example-org/example-project','issues','opened','digest','example-key','digest',1,1,$2,$2,$2,'completed')`, org, h.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{hubfixture.Secondary, hubfixture.Primary} {
		step(org)
		var primary, secondary string
		if err := h.Admin.QueryRowContext(t.Context(), `SELECT a.payload_object_key,b.payload_object_key FROM github_webhook_receipts a JOIN github_webhook_receipts b USING(delivery_id) WHERE a.organization_id=$1 AND b.organization_id=$2 AND a.delivery_id='same-delivery'`, hubfixture.Primary, hubfixture.Secondary).Scan(&primary, &secondary); err != nil {
			t.Fatal(err)
		}
		if secondary != "" || (primary == "") != (org == hubfixture.Primary) {
			t.Fatalf("webhook retention after serving %s: primary=%q secondary=%q", org, primary, secondary)
		}
	}
}

func TestIdleBoardWithRecordedChange(t *testing.T) {
	h := quietHub(t)
	for _, org := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		change := hubfixture.NewChange(t, h, org)
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		for _, f := range h.Service.ReadModelMaintenanceFeatures() {
			if f.Name != "board" {
				continue
			}
			b := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: f.Bytes}
			ctx := b.Context(t.Context(), tenant)
			for range 4 {
				if _, err := f.Step(ctx, tenant, b); err != nil {
					t.Fatal(err)
				}
			}
			statements, transactions := 0, 0
			ctx = postgres.WithTransactionTiming(ctx, func(name string, _ time.Time) {
				if name == "pg_begin" {
					transactions++
				}
			}, func(time.Time, string) { statements++ })
			out, err := f.Step(ctx, tenant, b)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("board with settled Change org=%s explicit_transactions=%d observed_statements=%d outcome=%+v", org, transactions, statements, out)
			if transactions != 0 || statements != 1 || out.Progress && !out.CursorOnly {
				t.Fatal("settled draft Change did not advance the cursor in one statement")
			}
			if _, err := h.Admin.ExecContext(t.Context(), "UPDATE native_board_items SET card_change_status='stale' WHERE organization_id=$1 AND work_item_id=$2", org, change.Issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Step(ctx, tenant, b); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := h.Admin.QueryRowContext(t.Context(), "SELECT card_change_status FROM native_board_items WHERE organization_id=$1 AND work_item_id=$2", org, change.Issue.WorkItemID).Scan(&status); err != nil || status != "draft" {
				t.Fatalf("unverified Change card status=%q err=%v", status, err)
			}
		}
	}
}
