package hubserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TenantCoreImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Catalog        storagecatalog.Catalog
	Objects        attachment.DetailStore
}

type TenantCoreImportResult struct {
	Projects        int64 `json:"projects"`
	AccessRows      int64 `json:"access_rows"`
	WorkItems       int64 `json:"work_items"`
	Dependencies    int64 `json:"dependencies"`
	PolicyRevisions int64 `json:"policy_revisions"`
	CurrentPolicies int64 `json:"current_policies"`
	ReviewPolicies  int64 `json:"review_policies"`
	MonthlyPolicies int64 `json:"monthly_policies"`
}

func ImportTenantCoreCopy(ctx context.Context, config postgres.Config, source TenantCoreImportSource) (result TenantCoreImportResult, resultErr error) {
	if source.CopyPath == "" || source.OrganizationID == "" || source.Objects == nil {
		return result, errors.New("tenant import requires a source copy, organization and archive storage")
	}
	if err := source.Catalog.Validate(); err != nil {
		return result, err
	}
	database, err := openPostgresDatabase(ctx, config)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, database.Close()) }()
	repository := database.postgres
	accessSource := postgres.AccessCopyImportSource{CopyPath: source.CopyPath, OrganizationID: source.OrganizationID, Catalog: source.Catalog, Objects: source.Objects}
	if err := repository.InitializeAccessCopyOrganization(ctx, accessSource); err != nil {
		return result, fmt.Errorf("initialize tenant organization: %w", err)
	}
	result.Projects, err = repository.ImportCoreProjects(ctx, postgres.CoreProjectImportSource{CopyPath: source.CopyPath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant projects: %w", err)
	}
	result.AccessRows, err = repository.ImportAccessCopy(ctx, accessSource)
	if err != nil {
		return result, fmt.Errorf("import tenant access: %w", err)
	}
	policies, err := repository.ImportCorePolicies(ctx, postgres.CorePolicyImportSource{CopyPath: source.CopyPath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant policies: %w", err)
	}
	result.PolicyRevisions = policies.PolicyRevisions
	result.CurrentPolicies = policies.CurrentPolicies
	result.ReviewPolicies = policies.ReviewPolicies
	result.MonthlyPolicies = policies.MonthlyPolicies
	var after tracker.ProjectID
	for {
		var projects []corestore.Project
		if err := repository.ReadTenant(ctx, source.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			projects, err = reader.Projects(ctx, after, 100)
			return err
		}); err != nil {
			return result, err
		}
		if len(projects) == 0 {
			break
		}
		for _, project := range projects {
			count, err := ImportCoreWorkItems(ctx, repository, CoreWorkItemImportSource{CopyPath: source.CopyPath, OrganizationID: source.OrganizationID, ProjectID: project.ID, Objects: source.Objects})
			if err != nil {
				return result, fmt.Errorf("import tenant work items: %w", err)
			}
			result.WorkItems += count
			after = project.ID
		}
	}
	result.Dependencies, err = ImportCoreIssueDependencies(ctx, repository, CoreDependencyImportSource{CopyPath: source.CopyPath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant dependencies: %w", err)
	}
	return result, nil
}
