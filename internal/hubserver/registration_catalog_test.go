package hubserver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRegistrationCatalogGolden(t *testing.T) {
	for _, mode := range []string{"local", "hosted", "shared", "local_conversations", "hosted_conversations", "shared_conversations", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			var service *Service
			base := strings.TrimSuffix(mode, "_conversations")
			conversations := strings.HasSuffix(mode, "_conversations")
			if base == "local" {
				admin := postgresStartupTemplate.Database(t)
				cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
				cfg.Hosted = nil
				cfg.InitialAdminToken = []byte(testHubAdminToken)
				cfg.SecretKeys = secretTestKeys(t, "1", "1")
				cfg.Logger = discardLogger()
				if conversations {
					cfg.Conversation = &ConversationConfig{Enabled: true}
					cfg.Content = (&changeObjectFixture{}).call
					cfg.RunDetails = &memoryRunDetails{}
				}
				var err error
				service, err = Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := service.Close(); err != nil {
						t.Error(err)
					}
				})
			} else {
				cfg := hostedSharedTestConfig(newHostedSecurityProvider(), hostedSharedKey(7), 1)
				if base == "hosted" || base == "maintenance" {
					cfg.Hosted.SharedEntry = nil
				}
				if conversations {
					cfg.Conversation = &ConversationConfig{Enabled: true}
				}
				if base == "maintenance" {
					cfg.CredentialMaintenance = true
					cfg.ListenAddress = "127.0.0.1:0"
				}
				service = openHostedPostgresService(t, cfg)
			}
			var lines []string
			for _, route := range service.echo.Routes() {
				lines = append(lines, route.Method+" "+route.Path)
			}
			registrationCatalogGolden(t, mode+"_routes", lines)
			if base == "local" && !conversations {
				organization := tracker.OrganizationID(service.localOrganization)
				credential, _, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(testHubAdminToken), "", "")
				if err != nil {
					t.Fatal(err)
				}
				ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: hubscope.OperatorIdentity(credential, string(organization))})
				ctx = service.withOperatorCatalog(ctx, credential, string(organization))
				definitions, err := (hostedOperatorExecutor{service}).ListTools(ctx)
				if err != nil {
					t.Fatal(err)
				}
				lines = nil
				for _, definition := range definitions {
					lines = append(lines, definition.Meta.Toolset+" "+definition.Name)
				}
				registrationCatalogGolden(t, "local_admin_tools", lines)
			}
		})
	}
	for _, mode := range []string{"dedicated", "shared"} {
		t.Run(mode+"_owner_tools", func(t *testing.T) {
			fixture, ctx := newOperatorKeyFixture(t, mode, "owner")
			definitions, err := (hostedOperatorExecutor{fixture.service}).ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, definition := range definitions {
				lines = append(lines, definition.Meta.Toolset+" "+definition.Name)
			}
			registrationCatalogGolden(t, mode+"_owner_tools", lines)
		})
	}
	t.Run("catalog_parts", func(t *testing.T) {
		var lines []string
		for _, part := range []struct {
			name        string
			definitions []operatortool.Definition
		}{
			{"core", operatortool.Catalog()},
			{"work", operatortool.WorkCatalog()},
			{"work_reads", operatortool.WorkReadCatalog()},
			{"commands", operatortool.CommandCatalog()},
			{"projects", operatortool.ProjectCatalog()},
			{"local_projects", operatortool.LocalProjectCatalog()},
			{"model_selection", operatortool.ModelSelectionCatalog()},
			{"fleet", operatortool.FleetCatalog()},
			{"workspaces", operatortool.WorkspaceCatalog()},
			{"changes", operatortool.ChangeCatalog()},
			{"threads", operatortool.ThreadCatalog()},
			{"attachments", operatortool.AttachmentCatalog()},
			{"administration", operatortool.AdministrationCatalog()},
			{"billing", operatortool.BillingCatalog()},
			{"hosted_context", operatortool.HostedContextCatalog()},
			{"operator_chat", operatortool.OperatorChatCatalog()},
			{"platform_credits", operatortool.PlatformCreditCatalog()},
			{"platform_entitlements", operatortool.PlatformEntitlementCatalog()},
			{"oauth", operatortool.OAuthGrantCatalog()},
		} {
			for _, definition := range part.definitions {
				lines = append(lines, part.name+" "+definition.Name)
			}
		}
		registrationCatalogGolden(t, "catalog_parts", lines)
	})
}

func registrationCatalogGolden(t *testing.T, name string, lines []string) {
	t.Helper()
	slices.Sort(lines)
	actual := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "registration_catalog", name+".golden")
	if os.Getenv("UPDATE_ROUTE_GOLDENS") == "1" && strings.HasSuffix(name, "_routes") {
		if err := os.WriteFile(path, []byte(actual), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual == string(expected) {
		return
	}
	var diff strings.Builder
	want := strings.Split(strings.TrimSuffix(string(expected), "\n"), "\n")
	for _, line := range want {
		if !slices.Contains(lines, line) {
			diff.WriteString("- " + line + "\n")
		}
	}
	for _, line := range lines {
		if !slices.Contains(want, line) {
			diff.WriteString("+ " + line + "\n")
		}
	}
	if diff.Len() == 0 {
		diff.WriteString("line order, duplicate count or final newline differs\n")
	}
	t.Errorf("registration catalog differs from %s (- golden, + current):\n%s", path, diff.String())
}
