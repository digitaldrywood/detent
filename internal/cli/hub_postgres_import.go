package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newHubPostgresImportCommand(lookupEnv func(string) string) *cobra.Command {
	var configPath, copyPath, organization, repositoryRoot string
	cmd := &cobra.Command{
		Use:          "import-core-postgres",
		Short:        "Import tenant core records from a sealed production copy",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			output, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			config, archiveConfig, err := readTenantImportConfig(configPath, lookupEnv)
			if err != nil {
				return err
			}
			catalog, err := readPlatformImportCatalog(repositoryRoot)
			if err != nil {
				return err
			}
			storage, err := attachment.NewStorage(cmd.Context(), archiveConfig, nil)
			if err != nil {
				return err
			}
			archive, ok := storage.(attachment.ArchiveStorage)
			if !ok {
				return errors.New("tenant import requires immutable archive storage")
			}
			result, err := hubserver.ImportTenantCoreCopy(cmd.Context(), *config, hubserver.TenantCoreImportSource{CopyPath: copyPath, OrganizationID: tracker.OrganizationID(organization), Catalog: catalog, Objects: tenantImportDetails{organization: organization, storage: archive}})
			if err != nil {
				return err
			}
			return output.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "Imported tenant core records: projects=%d access_rows=%d policy_revisions=%d current_policies=%d review_policies=%d monthly_policies=%d work_items=%d dependencies=%d cost_source_rows=%d cost_current_sources=%d cost_history_rows=%d execution_rows=%d monthly_admissions=%d monthly_leases=%d\n", result.Projects, result.AccessRows, result.PolicyRevisions, result.CurrentPolicies, result.ReviewPolicies, result.MonthlyPolicies, result.WorkItems, result.Dependencies, result.CostSourceRows, result.CostCurrentSources, result.CostHistoryRows, result.ExecutionRows, result.MonthlyAdmissions, result.MonthlyLeases)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&configPath, "entry-config", "", "shared entry PostgreSQL and archive configuration")
	cmd.Flags().StringVar(&copyPath, "database-copy", "", "sealed read-only tenant database copy")
	cmd.Flags().StringVar(&organization, "organization", "", "source organization identifier")
	cmd.Flags().StringVar(&repositoryRoot, "repository-root", "", "checked-out Detent source tree for the mapping catalog")
	for _, flag := range []string{"entry-config", "database-copy", "organization", "repository-root"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
	return cmd
}

func readTenantImportConfig(path string, lookupEnv func(string) string) (*postgres.Config, attachment.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 64*1024 {
		return nil, attachment.Config{}, errors.New("tenant import configuration is unavailable or too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var file cloudFileConfig
	if err := decoder.Decode(&file); err != nil {
		return nil, attachment.Config{}, errors.New("tenant import configuration is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || file.Storage.Backend != "postgres" || file.TenantPostgres == nil || file.Attachments == nil || lookupEnv == nil {
		return nil, attachment.Config{}, errors.New("tenant import requires PostgreSQL tenant and archive configuration")
	}
	config, err := file.TenantPostgres.runtime(lookupEnv)
	if err != nil {
		return nil, attachment.Config{}, err
	}
	archive := *file.Attachments
	archive.AccessKeyID = lookupEnv("DETENT_ATTACHMENTS_ACCESS_KEY_ID")
	archive.SecretAccessKey = lookupEnv("DETENT_ATTACHMENTS_SECRET_ACCESS_KEY")
	return config, archive, nil
}

type tenantImportDetails struct {
	organization string
	storage      attachment.ArchiveStorage
}

func (d tenantImportDetails) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if organization != d.organization {
		return attachment.ArchiveReference{}, attachment.ErrInvalid
	}
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if err := attachment.StoreArchive(ctx, d.storage, organization, ref, compressed); err != nil {
		return attachment.ArchiveReference{}, err
	}
	return ref, nil
}

func (d tenantImportDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if organization != d.organization {
		return nil, attachment.ErrInvalid
	}
	return attachment.ReadArchive(ctx, d.storage, organization, ref)
}
