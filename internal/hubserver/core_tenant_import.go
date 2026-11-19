package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TenantCoreImportSource = coreimport.TenantCoreImportSource
type TenantCoreImportResult = coreimport.TenantCoreImportResult

func ImportTenantCoreCopy(ctx context.Context, config postgres.Config, source TenantCoreImportSource) (TenantCoreImportResult, error) {
	return coreimport.ImportTenantCoreCopy(ctx, config, source, coreimport.TenantImportDeps{
		OpenRepository: func(ctx context.Context, config postgres.Config) (*postgres.Repository, func() error, error) {
			database, err := openPostgresDatabase(ctx, config)
			if err != nil {
				return nil, nil, err
			}
			return database.postgres, database.Close, nil
		},
		ImportGitHubOutbox:     ImportCoreGitHubOutbox,
		ImportExecutions:       coreExecutions().Import,
		ValidateArchiveSection: validateNativeArchiveSection,
	})
}

func coreExecutions() *coreimport.Executions {
	return coreimport.NewExecutions(coreimport.Deps{CleanupRequired: nativeRunCleanupRequired, DecodeCheckpoint: hubrecords.DecodeAttemptCheckpoint})
}

func prepareCoreExecutionReceipt[T any](ctx context.Context, content *eventstore.Content, organization tracker.OrganizationID, copyHash, kind, id string, value T) (eventstore.Receipt, error) {
	return coreimport.PrepareExecutionReceipt(ctx, content, organization, copyHash, kind, id, value)
}
