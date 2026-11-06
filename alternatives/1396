package hubserver

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAccountReceiptsPreserveReplayAndUncertainty(t *testing.T) {
	repository := projectRecordPostgres(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service := &Service{config: Config{AccessRepository: repository, TenantRepository: repository, RunDetails: &memoryRunDetails{}, now: time.Now, Hosted: &HostedConfig{OrganizationID: string(org), PublicURL: "http://example.test"}}}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		credential := apiCredential{Hosted: &auth.HostedIdentity{Subject: "actor", SessionID: "session", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
		metadata := mutation.Metadata{RetryIdentity: "same-retry", InputHash: "same-input"}
		for range 2 {
			output, err := service.executeHostedAccountFor(t.Context(), credential, operatortool.SupportStart, operatoradmin.Input{}, metadata)
			if err != nil || output.ResourceID != string(org) {
				t.Fatalf("output=%+v err=%v", output, err)
			}
		}
		metadata.InputHash = "changed-input"
		if _, err := service.executeHostedAccountFor(t.Context(), credential, operatortool.SupportStart, operatoradmin.Input{}, metadata); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed input error=%v", err)
		}
		metadata.RetryIdentity = "pending-retry"
		receipt := accountReceipt{Kind: "account_administration", Retry: metadata.RetryIdentity, Hash: metadata.InputHash, Outcome: "pending"}
		if err := service.storeAccessAccountReceipt(t.Context(), credential, receipt, metadata); err != nil {
			t.Fatal(err)
		}
		if _, err := service.executeHostedAccountFor(t.Context(), credential, operatortool.SupportStart, operatoradmin.Input{}, metadata); !errors.Is(err, mutation.ErrUncertain) {
			t.Fatalf("pending replay error=%v", err)
		}
	}
}
