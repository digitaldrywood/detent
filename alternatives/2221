package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeCommandReplayUsesTenantReceipt(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			principal := "principal_" + string(organization)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", CreatedAt: now}); err != nil {
					return err
				}
				return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Hash: principal, Name: "Example principal", Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: organization, credential: apiCredential{ID: principal, Hash: principal, Scope: apiScopeAdmin}}
			input := map[string]string{"name": "example"}
			hash, err := nativeCommandHash(input)
			if err != nil {
				t.Fatal(err)
			}
			content := eventstore.NewContent(service.runDetailStore())
			receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: "example", CommandKey: "example-key", RequestHash: hash, CreatedAt: service.config.now()}, json.RawMessage(`{"result":"saved"}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.PutReceipt(t.Context(), receipt); return err }); err != nil {
				t.Fatal(err)
			}
			receipt.Operation, receipt.CommandKey = "example session_legacy", "historical-key"
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.PutReceipt(t.Context(), receipt); return err }); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name, key      string
				input          map[string]string
				found, failure bool
			}{
				{"replay", "example-key", input, true, false},
				{"historical operation", "historical-key", input, true, false},
				{"missing", "missing-key", input, false, false},
				{"conflicting", "example-key", map[string]string{"name": "changed"}, false, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					raw, found, err := service.nativeCommandReplay(t.Context(), scope, "example", test.key, test.input)
					if found != test.found || (err != nil) != test.failure {
						t.Fatalf("found=%v err=%v", found, err)
					}
					if found && err == nil && string(raw) != `{"result":"saved"}` {
						t.Fatalf("response=%s", raw)
					}
				})
			}
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessCredential(t.Context(), principal, now)
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := service.nativeCommandReplay(t.Context(), scope, "example", "example-key", input); err == nil {
				t.Fatal("revoked credential replayed receipt")
			}
		})
	}
}
