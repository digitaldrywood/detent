package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerReceiptRollsBackWhenArchiveDisappears(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION remove_unowned_receipt_archive() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN DELETE FROM archive_objects a USING native_commands c WHERE a.organization_id=NEW.organization_id AND c.organization_id=a.organization_id AND c.command_key='archive-race' AND a.object_key=c.payload_object_key AND NOT EXISTS(SELECT 1 FROM archive_object_owners o WHERE o.organization_id=a.organization_id AND o.archive_id=a.id); RETURN NEW; END$$; CREATE TRIGGER remove_unowned_receipt_archive BEFORE INSERT ON archive_usage_counters FOR EACH ROW EXECUTE FUNCTION remove_unowned_receipt_archive()`); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		seedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", historyAt(), true)
		lease := claimLease(t, repository, org, leaseClaim(1))
		receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "runner_shared", Operation: "archive-race", CommandKey: "archive-race", RequestHash: strings.Repeat("a", 64), CreatedAt: historyAt()}, json.RawMessage(`{"fixture":true}`))
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			facts, ok := w.(interface {
				WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
			})
			if !ok {
				t.Fatal("runner command facts unavailable")
			}
			return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, ActorID: receipt.ActorID, Operation: receipt.Operation, Key: receipt.CommandKey}, func(w tenantstore.Writer) error { _, err := w.PutReceipt(ctx, receipt); return err })
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("tenant=%s archive disappearance result=%v,want rejection", org, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.Receipt(t.Context(), receipt.ActorID, receipt.Operation, receipt.CommandKey)
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("tenant=%s failed publication left a receipt: %v", org, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
