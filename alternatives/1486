package hubserver

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func TestIncludedAICreditMigration(t *testing.T) {
	t.Parallel()
	f, base := newHostedBillingFixture(t)
	configureTestCredits(t, f, base)
	d := f.service.database
	const name = "20261022151000_included_ai_credits.sql"
	data, err := migrationFiles.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(data), "-- +goose Down")
	if !ok {
		t.Fatal("migration has no rollback")
	}
	if _, err := d.db.ExecContext(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test','existing-purchase',5000000,'purchase',?)`, d.hostedOrganization, d.now().UnixMicro()); err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, d.db, fstest.MapFS{name: &fstest.MapFile{Data: data}}, goose.WithDisableGlobalRegistry(true), goose.WithTableName("included_migration_version"), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var included int64
	if err := d.reader.QueryRowContext(t.Context(), `SELECT included_amount_micros FROM ai_credit_transactions WHERE source='existing-purchase'`).Scan(&included); err != nil || included != 0 {
		t.Fatalf("existing included=%d err=%v", included, err)
	}
	if _, err := provider.DownTo(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	var amount int64
	if err := d.reader.QueryRowContext(t.Context(), `SELECT amount_micros FROM ai_credit_transactions WHERE source='existing-purchase'`).Scan(&amount); err != nil || amount != 5000000 {
		t.Fatalf("existing amount=%d err=%v", amount, err)
	}
	if _, err := d.reader.ExecContext(t.Context(), `SELECT included_amount_micros FROM ai_credit_transactions`); err == nil {
		t.Fatal("included column survived rollback")
	}
}
