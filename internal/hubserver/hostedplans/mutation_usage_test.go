package hostedplans

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentAPIMutationChargesPostgresAreTenantScoped(t *testing.T) {
	t.Parallel()
	repository, _ := postgresWithAdmin(t, 3)
	ref := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"collaboration_bytes": 10, "api_mutations": 2}
	first := tracker.OrganizationID("org_allowance_first")
	second := tracker.OrganizationID("org_allowance_second")
	seedConversationAllowancePostgres(t, repository, first, ref, allowances)
	seedConversationAllowancePostgres(t, repository, second, ref, allowances)
	now := time.Unix(120, 0).UTC()
	usage := func(t *testing.T, organization tracker.OrganizationID) int64 {
		t.Helper()
		var used int64
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			used, err = conversationAllowanceUsage(t.Context(), reader, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return used
	}
	charge := func(organization, session tracker.OrganizationID, receipt string) error {
		database := conversationAllowanceDatabase(organization, ref, allowances, repository)
		return repository.WriteTenant(t.Context(), session, corestore.Serving, func(writer tenantstore.Writer) error {
			return database.ChargeAttachmentAPIMutations(t.Context(), writer, organization, receipt, now, false, "mutation")
		})
	}
	for _, direction := range []struct{ named, session tracker.OrganizationID }{{first, second}, {second, first}} {
		err := charge(direction.named, direction.session, "receipt_cross_tenant_"+string(direction.named))
		if !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("attachment mutation for %s in %s: cross-tenant allowance write=%v", direction.named, direction.session, err)
		}
	}
	for _, organization := range []tracker.OrganizationID{first, second} {
		if used := usage(t, organization); used != 0 {
			t.Fatalf("organization %s usage=%d after cross-tenant write", organization, used)
		}
	}
	for index, organization := range []tracker.OrganizationID{first, second} {
		if err := charge(organization, organization, "receipt_own_tenant"); err != nil {
			t.Fatalf("own-organization allowance write for %s: %v", organization, err)
		}
		for later, other := range []tracker.OrganizationID{first, second} {
			want := int64(0)
			if later <= index {
				want = 1
			}
			if used := usage(t, other); used != want {
				t.Fatalf("after charging %s, organization %s usage=%d want %d", organization, other, used, want)
			}
		}
	}
}

func conversationAllowanceUsage(ctx context.Context, reader billingstore.Reader, window time.Time) (int64, error) {
	used, err := reader.BillingUsage(ctx, window, "api_mutations")
	if errors.Is(err, corestore.ErrNotFound) {
		return 0, nil
	}
	return used, err
}

func TestAttachmentAPIMutationChargesPostgres(t *testing.T) {
	t.Parallel()
	repository, _ := postgresWithAdmin(t, 3)
	ref := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"collaboration_bytes": 10, "api_mutations": 3}
	now := time.Unix(120, 0).UTC()
	organizations := []tracker.OrganizationID{"org_attachment_first", "org_attachment_second"}
	for _, organization := range organizations {
		seedConversationAllowancePostgres(t, repository, organization, ref, allowances)
	}
	charge := func(organization tracker.OrganizationID, receiptID string) error {
		database := conversationAllowanceDatabase(organization, ref, allowances, repository)
		return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			return database.ChargeAttachmentAPIMutations(t.Context(), writer, organization, receiptID, now, false, "record", "mutation")
		})
	}
	for _, step := range []struct {
		organization tracker.OrganizationID
		receiptID    string
		wantRefused  int64
		wantUsage    int64
	}{
		{organizations[0], "upload_example", -1, 2},
		{organizations[0], "upload_example", -1, 2},
		{organizations[0], "upload_second", 3, 2},
		{organizations[1], "upload_example", -1, 2},
	} {
		err := charge(step.organization, step.receiptID)
		var limit *LimitError
		switch {
		case step.wantRefused < 0 && err != nil:
			t.Fatalf("%s %s charge=%v", step.organization, step.receiptID, err)
		case step.wantRefused >= 0 && (!errors.As(err, &limit) || limit.Resource != "api_mutations" || limit.Consumption != step.wantRefused):
			t.Fatalf("%s %s refusal=%v", step.organization, step.receiptID, err)
		}
		if err := repository.ReadTenant(t.Context(), step.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			used, err := conversationAllowanceUsage(t.Context(), reader, now)
			if err != nil {
				return err
			}
			if used != step.wantUsage {
				return fmt.Errorf("organization %s usage=%d want=%d", step.organization, used, step.wantUsage)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
