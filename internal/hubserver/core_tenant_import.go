package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TenantCoreImportSource struct {
	BundlePath        string
	MappingBundlePath string
	CopyPath          string
	OrganizationID    tracker.OrganizationID
	Catalog           storagecatalog.Catalog
	Objects           attachment.DetailStore
	Logger            *slog.Logger
}

type TenantCoreImportResult struct {
	CoreImportComplete bool  `json:"core_import_complete"`
	CreditAccounts     int64 `json:"credit_accounts"`
	CreditPacks        int64 `json:"credit_packs"`
	CreditTransactions int64 `json:"credit_transactions"`
	HubIdentityRows    int64 `json:"hub_identity_rows"`
	Projects           int64 `json:"projects"`
	AccessRows         int64 `json:"access_rows"`
	WorkItems          int64 `json:"work_items"`
	Dependencies       int64 `json:"dependencies"`
	PolicyRevisions    int64 `json:"policy_revisions"`
	CurrentPolicies    int64 `json:"current_policies"`
	ReviewPolicies     int64 `json:"review_policies"`
	MonthlyPolicies    int64 `json:"monthly_policies"`
	CostSourceRows     int64 `json:"cost_source_rows"`
	CostCurrentSources int64 `json:"cost_current_sources"`
	CostHistoryRows    int64 `json:"cost_history_rows"`
	ExecutionRows      int64 `json:"execution_rows"`
	UsageRows          int64 `json:"usage_rows"`
	MonthlyAdmissions  int64 `json:"monthly_admissions"`
	MonthlyLeases      int64 `json:"monthly_leases"`
	Changes            int64 `json:"changes"`
	GitHubOutboundRows int64 `json:"github_outbound_rows"`
	ConversationRows   int64 `json:"conversation_rows"`
	ArchiveSections    int64 `json:"archive_sections"`
	ArchiveMessages    int64 `json:"archive_messages"`
	ArchiveVersions    int64 `json:"archive_versions"`
	ConversationPrices int64 `json:"conversation_prices"`
	ConversationUsage  int64 `json:"conversation_usage"`
}

func ImportTenantCoreCopy(ctx context.Context, config postgres.Config, source TenantCoreImportSource) (result TenantCoreImportResult, resultErr error) {
	if source.CopyPath != "" {
		return result, neutralimport.ErrInvalid
	}
	if source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return result, errors.New("tenant import requires a neutral bundle, organization and archive storage")
	}
	if err := source.Catalog.Validate(); err != nil {
		return result, err
	}
	var historical bool
	if err := neutralimport.Read(ctx, source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
		issueSchema, exists := bundle.Schema()["issues"]
		_, native := issueSchema["native_id"]
		historical = exists && !native
		return nil
	}); err != nil {
		return result, err
	}
	if historical {
		return importHistoricalTenantCoreCopy(ctx, config, source)
	}
	identity, err := postgres.VerifyNeutralCoreCutoverSource(ctx, source.BundlePath, source.OrganizationID)
	if err != nil {
		return result, err
	}
	database, err := openPostgresDatabase(ctx, config)
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, database.Close())
		if resultErr != nil {
			result.CoreImportComplete = false
		}
	}()
	repository := database.postgres
	accessSource := postgres.AccessCopyImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Catalog: source.Catalog, Objects: source.Objects}
	if err := repository.InitializeAccessCopyOrganization(ctx, accessSource); err != nil {
		return result, fmt.Errorf("initialize tenant organization: %w", err)
	}
	result.HubIdentityRows, err = repository.ImportCoreHubIdentity(ctx, postgres.CoreHubIdentityImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant Hub identity: %w", err)
	}
	result.Projects, err = repository.ImportCoreProjects(ctx, postgres.CoreProjectImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant projects: %w", err)
	}
	result.AccessRows, err = repository.ImportAccessCopy(ctx, accessSource)
	if err != nil {
		return result, fmt.Errorf("import tenant access: %w", err)
	}
	policies, err := repository.ImportCorePolicies(ctx, postgres.CorePolicyImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant policies: %w", err)
	}
	result.PolicyRevisions = policies.PolicyRevisions
	result.CurrentPolicies = policies.CurrentPolicies
	result.ReviewPolicies = policies.ReviewPolicies
	result.MonthlyPolicies = policies.MonthlyPolicies
	var activity map[string]coreHistoricalActivity
	var activityDigest string
	if err := neutralimport.Read(ctx, source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
		var err error
		activity, err = readCoreHistoricalActivity(ctx, bundle, source.OrganizationID)
		activityDigest = bundle.Digest()
		return err
	}); err != nil {
		return result, fmt.Errorf("read tenant historical activity: %w", err)
	}
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
			count, err := ImportCoreWorkItems(ctx, repository, CoreWorkItemImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, ProjectID: project.ID, Objects: source.Objects, Logger: source.Logger, activity: activity, activityDigest: activityDigest})
			if err != nil {
				return result, fmt.Errorf("import tenant work items: %w", err)
			}
			result.WorkItems += count
			after = project.ID
		}
	}
	result.GitHubOutboundRows, err = ImportCoreGitHubOutbox(ctx, repository, source)
	if err != nil {
		return result, fmt.Errorf("import tenant GitHub outbound source: %w", err)
	}
	result.Dependencies, err = ImportCoreIssueDependencies(ctx, repository, CoreDependencyImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant dependencies: %w", err)
	}
	result.ExecutionRows, err = ImportCoreExecution(ctx, repository, CoreExecutionImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, SourceObjects: source.Objects, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant execution: %w", err)
	}
	usage, err := ImportCoreReportUsage(ctx, repository, ReportUsageImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, ExpectedSHA256: identity.SourceSHA256})
	if err != nil {
		return result, fmt.Errorf("import tenant usage facts: %w", err)
	}
	result.UsageRows = usage.UsageRows

	result.Changes, err = ImportCoreChanges(ctx, repository, CoreChangeImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, SourceObjects: source.Objects, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant changes: %w", err)
	}
	monthly, err := repository.ImportCoreMonthlyBudgetFacts(ctx, postgres.CorePolicyImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Objects: source.Objects})
	if err != nil {
		return result, fmt.Errorf("import tenant monthly budget facts: %w", err)
	}
	result.MonthlyAdmissions = monthly.Admissions
	result.MonthlyLeases = monthly.Leases
	costs, err := repository.ImportCostCopy(ctx, postgres.CostImportSource{
		BundlePath: source.BundlePath,
		Catalog:    source.Catalog,
		Source:     storagecatalog.Source{Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)},
		Objects:    source.Objects,
	})
	if err != nil {
		return result, fmt.Errorf("import tenant costs: %w", err)
	}
	if !costs.Done {
		return result, errors.New("tenant cost import did not finish")
	}
	result.CostSourceRows = costs.SourceRows
	result.CostCurrentSources = costs.CurrentSources
	result.CostHistoryRows = costs.HistoryRows
	conversations, err := repository.ImportConversationHistory(ctx, postgres.ConversationHistoryImportSource{
		BundlePath: source.BundlePath,
		Catalog:    source.Catalog,
		Source:     storagecatalog.Source{Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)},
		Objects:    source.Objects,
	})
	if err != nil {
		return result, fmt.Errorf("import tenant conversation history: %w", err)
	}
	result.ConversationRows = conversations.Rows
	archives, err := repository.ImportNativeArchiveHistory(ctx, postgres.NativeArchiveImportSource{
		ConversationHistoryImportSource: postgres.ConversationHistoryImportSource{BundlePath: source.BundlePath, Catalog: source.Catalog, Source: storagecatalog.Source{Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)}, Objects: source.Objects},
		ValidateSection:                 validateNativeArchiveSection,
	})
	if err != nil {
		return result, fmt.Errorf("recover tenant historical archives: %w", err)
	}
	result.ArchiveSections, result.ArchiveMessages, result.ArchiveVersions = archives.Sections, archives.Messages, archives.Versions
	billing, err := repository.ImportConversationBilling(ctx, postgres.ConversationBillingImportSource{
		BundlePath: source.BundlePath,
		Catalog:    source.Catalog,
		Source:     storagecatalog.Source{Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)},
		Objects:    source.Objects,
	})
	if err != nil {
		return result, fmt.Errorf("import tenant conversation usage: %w", err)
	}
	result.ConversationPrices = billing.Prices
	result.ConversationUsage = billing.Usage
	credits, err := repository.ImportCreditHistory(ctx, postgres.CreditHistoryImportSource{
		BundlePath: source.BundlePath,
		Catalog:    source.Catalog,
		Source:     storagecatalog.Source{Store: "hub", CatalogVersion: source.Catalog.Version, OrganizationID: string(source.OrganizationID)},
		Objects:    source.Objects,
	})
	if err != nil {
		return result, fmt.Errorf("import tenant credit history: %w", err)
	}
	result.CreditAccounts = credits.Accounts
	result.CreditPacks = credits.Packs
	result.CreditTransactions = credits.Transactions
	if _, err := repository.ImportCoreOnboardingProgress(ctx, postgres.CoreProjectImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, Objects: source.Objects}); err != nil {
		return result, fmt.Errorf("import tenant onboarding history: %w", err)
	}
	result.CoreImportComplete = true
	return result, nil
}

func importHistoricalTenantCoreCopy(ctx context.Context, config postgres.Config, source TenantCoreImportSource) (result TenantCoreImportResult, resultErr error) {
	database, err := openPostgresDatabase(ctx, config)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, database.Close()) }()
	err = neutralimport.Read(ctx, source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
		if err := neutralimport.ValidateHistoricalWorkItemTables(bundle); err != nil {
			return err
		}
		if _, exists := bundle.Schema()["issue_dependencies"]; exists {
			rows, err := bundle.Page(ctx, "issue_dependencies", nil, 1)
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				return errors.New("historical dependencies require a verified current import source")
			}
		}
		mappings, project, err := resolveHistoricalImportMappings(ctx, database.postgres, bundle, source)
		if err != nil {
			return err
		}
		result.WorkItems, err = ImportCoreWorkItems(ctx, database.postgres, CoreWorkItemImportSource{BundlePath: source.BundlePath, OrganizationID: source.OrganizationID, ProjectID: project, Objects: source.Objects, Logger: source.Logger, IdentityMappings: mappings})
		return err
	})
	return result, err
}

func resolveHistoricalImportMappings(ctx context.Context, repository *postgres.Repository, bundle *neutralimport.Bundle, source TenantCoreImportSource) ([]CoreWorkItemIdentityMapping, tracker.ProjectID, error) {
	sourceNumbers := map[int64]int64{}
	var sourceAfter []string
	for {
		rows, err := bundle.Page(ctx, "issues", sourceAfter, 100)
		if err != nil {
			return nil, "", err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			var id, number int64
			if err := row.Decode(map[string]any{"id": &id, "github_number": &number}); err != nil {
				return nil, "", err
			}
			sourceNumbers[id] = number
			sourceAfter = row.Key
		}
	}
	candidates := map[int64]CoreWorkItemIdentityMapping{}
	if source.MappingBundlePath != "" {
		if err := neutralimport.Read(ctx, source.MappingBundlePath, source.OrganizationID, func(mapping *neutralimport.Bundle) error {
			if err := neutralimport.ValidateCoreTables(mapping, "projects", "issues", "repositories"); err != nil {
				return err
			}
			var after []string
			for {
				rows, err := mapping.Page(ctx, "issues", after, 100)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					return nil
				}
				for _, row := range rows {
					var id, repositoryID, number int64
					var organization tracker.OrganizationID
					var item tracker.NativeWorkItemID
					var project tracker.ProjectID
					var node sql.NullString
					if err := row.Decode(map[string]any{"id": &id, "number": &number, "repository_id": &repositoryID, "organization_id": &organization, "project_id": &project, "native_id": &item, "github_node_id": &node}); err != nil {
						return err
					}
					after = row.Key
					sourceNumber, selected := sourceNumbers[id]
					if !selected {
						continue
					}
					if number != sourceNumber {
						return corestore.ErrTenantMismatch
					}
					if organization != source.OrganizationID || item == "" || project == "" || !node.Valid || node.String == "" {
						return corestore.ErrTenantMismatch
					}
					sourceRepository, err := mapping.Lookup(ctx, "repositories", []string{strconv.FormatInt(repositoryID, 10)})
					if err != nil {
						return err
					}
					var repositoryNode string
					if err := sourceRepository.Decode(map[string]any{"github_node_id": &repositoryNode}); err != nil {
						return err
					}
					projectRow, err := mapping.Lookup(ctx, "projects", []string{string(project)})
					if err != nil {
						return err
					}
					var projectOrganization tracker.OrganizationID
					var profile string
					var projectRepositoryID sql.NullInt64
					if err := projectRow.Decode(map[string]any{"organization_id": &projectOrganization, "profile": &profile, "repository_id": &projectRepositoryID}); err != nil {
						return err
					}
					if projectOrganization != organization || profile != "github_compatible" || !projectRepositoryID.Valid || projectRepositoryID.Int64 != repositoryID {
						return corestore.ErrTenantMismatch
					}
					if _, exists := candidates[id]; exists {
						return corestore.ErrRevisionConflict
					}
					candidates[id] = CoreWorkItemIdentityMapping{SourceSHA256: bundle.Identity().SourceSHA256, OrganizationID: organization, SourceIssueID: id, SourceRepositoryID: repositoryID, SourceRepositoryNodeID: repositoryNode, SourceGitHubNodeID: node.String, TargetProjectID: project, TargetWorkItemID: item}
				}
			}
		}); err != nil {
			return nil, "", fmt.Errorf("verify historical identity mapping source: %w", err)
		}
	}

	if err := repository.ReadTenant(ctx, source.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
		pin, err := reader.Receipt(ctx, "production-import", "core-project-source", "source")
		if err == nil && pin.RequestHash != bundle.Identity().SourceSHA256 {
			return errors.New("historical recovery cannot replace the existing primary import source")
		}
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	}); err != nil {
		return nil, "", err
	}
	projects := map[string][]corestore.Project{}
	var afterProject tracker.ProjectID
	for {
		var rows []corestore.Project
		var bindings map[tracker.ProjectID]string
		if err := repository.ReadTenant(ctx, source.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			rows, err = reader.Projects(ctx, afterProject, 100)
			if err != nil {
				return err
			}
			bindings = map[tracker.ProjectID]string{}
			for _, project := range rows {
				if project.Profile != "github_compatible" {
					continue
				}
				record, err := reader.ProjectRecord(ctx, project.ID)
				if err != nil {
					return err
				}
				bindings[project.ID] = record.Settings.Repository
			}
			return nil
		}); err != nil {
			return nil, "", err
		}
		if len(rows) == 0 {
			break
		}
		for _, project := range rows {
			afterProject = project.ID
			if binding, found := bindings[project.ID]; found {
				projects[binding] = append(projects[binding], project)
			}
		}
	}
	var result []CoreWorkItemIdentityMapping
	var selected tracker.ProjectID
	var after []string
	for {
		rows, err := bundle.Page(ctx, "issues", after, 100)
		if err != nil {
			return nil, "", err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			var id, repositoryID int64
			var node string
			if err := row.Decode(map[string]any{"id": &id, "repository_id": &repositoryID, "github_node_id": &node}); err != nil {
				return nil, "", err
			}
			after = row.Key
			repositoryRow, err := bundle.Lookup(ctx, "repositories", []string{strconv.FormatInt(repositoryID, 10)})
			if err != nil {
				return nil, "", err
			}
			var owner, name, repositoryNode string
			if err := repositoryRow.Decode(map[string]any{"github_owner": &owner, "github_name": &name, "github_node_id": &repositoryNode}); err != nil {
				return nil, "", err
			}
			targets := projects[owner+"/"+name]
			if len(targets) != 1 {
				return nil, "", errors.New("historical recovery requires one existing authoritative project repository binding")
			}
			project := targets[0].ID
			candidate, supplied := candidates[id]
			var current corestore.WorkItem
			var found bool
			var receipt eventstore.Receipt
			if err := repository.ReadTenant(ctx, source.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
				var err error
				current, err = reader.WorkItemByProvenance(ctx, project, "github", node)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				found = err == nil
				if !found {
					if !supplied {
						return errors.New("historical recovery requires an explicitly verified post-native mapping bundle")
					}
					if _, err := reader.WorkItem(ctx, project, candidate.TargetWorkItemID); !errors.Is(err, corestore.ErrNotFound) {
						return errors.New("historical recovery cannot overwrite an occupied target identity")
					}
					if _, err := reader.WorkItemByNumber(ctx, project, sourceNumbers[id]); !errors.Is(err, corestore.ErrNotFound) {
						return errors.New("historical recovery cannot overwrite an occupied target number")
					}
					return nil
				}
				if current.ID != id {
					return corestore.ErrTenantMismatch
				}
				receipt, err = reader.Receipt(ctx, "production-import", "core-work-item-source", strconv.FormatInt(id, 10))
				if err != nil {
					return errors.New("historical recovery cannot overwrite an existing work item without its matching import receipt")
				}
				_, err = reader.Receipt(ctx, "production-import", "core-work-item", strconv.FormatInt(id, 10))
				return err
			}); err != nil {
				return nil, "", err
			}
			if found {
				existing := CoreWorkItemIdentityMapping{SourceSHA256: bundle.Identity().SourceSHA256, OrganizationID: source.OrganizationID, SourceIssueID: id, SourceRepositoryID: repositoryID, SourceRepositoryNodeID: repositoryNode, SourceGitHubNodeID: node, TargetProjectID: project, TargetWorkItemID: current.NativeID}
				if supplied && candidate != existing {
					return nil, "", corestore.ErrRevisionConflict
				}
				raw, err := eventstore.NewContent(source.Objects).Receipt(ctx, receipt)
				if err != nil {
					return nil, "", err
				}
				var saved struct{ Mapping CoreWorkItemIdentityMapping }
				if err := json.Unmarshal(raw, &saved); err != nil {
					return nil, "", err
				}
				if saved.Mapping != existing {
					return nil, "", corestore.ErrRevisionConflict
				}
				candidate = existing
			}
			expected := CoreWorkItemIdentityMapping{SourceSHA256: bundle.Identity().SourceSHA256, OrganizationID: source.OrganizationID, SourceIssueID: id, SourceRepositoryID: repositoryID, SourceRepositoryNodeID: repositoryNode, SourceGitHubNodeID: node, TargetProjectID: project, TargetWorkItemID: candidate.TargetWorkItemID}
			if candidate != expected || candidate.TargetWorkItemID == "" {
				return nil, "", corestore.ErrTenantMismatch
			}
			if selected != "" && selected != project {
				return nil, "", errors.New("historical recovery requires a source scoped to one verified project")
			}
			selected = project
			result = append(result, candidate)
		}
	}
	if len(result) == 0 {
		return nil, "", errors.New("historical recovery source has no mapped work items")
	}
	return result, selected, nil
}
