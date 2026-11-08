package postgres_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func TestNeutralImportFamiliesRefuseLegacyCopySelector(t *testing.T) {
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "preserved-source.db")
	original := []byte("preserved historical source")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	repository := new(postgres.Repository)
	objects := newHistoryObjects()
	for _, bundle := range []string{"", t.TempDir()} {
		suffix := "_copy"
		if bundle != "" {
			suffix = "_both"
		}
		for _, tc := range []struct {
			name string
			run  func() error
		}{
			{"projects", func() error {
				_, err := repository.ImportCoreProjects(t.Context(), postgres.CoreProjectImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", Objects: objects})
				return err
			}},
			{"policies", func() error {
				_, err := repository.ImportCorePolicies(t.Context(), postgres.CorePolicyImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", Objects: objects})
				return err
			}},
			{"budget_facts", func() error {
				_, err := repository.ImportCoreMonthlyBudgetFacts(t.Context(), postgres.CorePolicyImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", Objects: objects})
				return err
			}},
			{"access", func() error {
				_, err := repository.ImportAccessCopy(t.Context(), postgres.AccessCopyImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", Catalog: catalog, Objects: objects})
				return err
			}},
			{"cost", func() error {
				_, err := repository.ImportCostCopy(t.Context(), postgres.CostImportSource{CopyPath: path, BundlePath: bundle, Catalog: catalog, Source: storagecatalog.Source{Store: "hub", OrganizationID: "org_example", CatalogVersion: catalog.Version}, Objects: objects})
				return err
			}},
			{"credit_history", func() error {
				_, err := repository.ImportCreditHistory(t.Context(), postgres.CreditHistoryImportSource{CopyPath: path, BundlePath: bundle, Catalog: catalog, Source: storagecatalog.Source{Store: "hub", OrganizationID: "org_example", CatalogVersion: catalog.Version}, Objects: objects})
				return err
			}},
			{"conversation_billing", func() error {
				_, err := repository.ImportConversationBilling(t.Context(), postgres.ConversationBillingImportSource{CopyPath: path, BundlePath: bundle, Catalog: catalog, Source: storagecatalog.Source{Store: "hub", OrganizationID: "org_example", CatalogVersion: catalog.Version}, Objects: objects})
				return err
			}},
			{"conversation_history", func() error {
				_, err := repository.ImportConversationHistory(t.Context(), postgres.ConversationHistoryImportSource{CopyPath: path, BundlePath: bundle, Catalog: catalog, Source: storagecatalog.Source{Store: "hub", OrganizationID: "org_example", CatalogVersion: catalog.Version}, Objects: objects})
				return err
			}},
			{"hub_identity", func() error {
				_, err := repository.ImportCoreHubIdentity(t.Context(), postgres.CoreHubIdentityImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", Objects: objects})
				return err
			}},
		} {
			t.Run(tc.name+suffix, func(t *testing.T) {
				if err := tc.run(); !errors.Is(err, postgres.ErrNeutralImport) {
					t.Fatalf("legacy selector reached source decoding: %v", err)
				}
			})
		}
	}
	if err := postgres.VerifyNeutralCoreCutover(t.Context(), path, "org_example"); err == nil {
		t.Fatal("cutover verifier accepted a historical database file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("legacy source changed: %v", err)
	}
}
