package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
)

type exposureFinding struct {
	SourceProject         string `json:"source_project"`
	SourceRepository      string `json:"source_repository"`
	DestinationRepository string `json:"destination_repository"`
	IssueNumber           int    `json:"issue_number"`
	IssueURL              string `json:"issue_url,omitempty"`
	MatchedIdentifier     string `json:"matched_identifier"`
}

type exposureReport struct {
	Findings []exposureFinding `json:"findings"`
	Warnings []string          `json:"warnings,omitempty"`
}

type exposureDeps struct {
	loadWorkflow   func(globalconfig.Project) (workflowconfig.Workflow, error)
	repositoryInfo func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error)
	scan           func(context.Context, workflowconfig.Config, string, []string) ([]ghconnector.IssueExposure, error)
}

func newExposureCommand(configPath *string, opts options) *cobra.Command {
	return newExposureCommandWithDeps(configPath, opts, exposureDeps{})
}

func newExposureCommandWithDeps(configPath *string, opts options, deps exposureDeps) *cobra.Command {
	projectID := ""
	cmd := &cobra.Command{
		Use:          "exposure",
		Short:        "Scan public issue destinations for private project identifiers",
		Long:         "Run a read-only, one-shot scan of configured public issue-filing destinations. The report lists possible historical cross-project disclosures and never edits issues or comments.",
		Example:      "detent exposure\n  detent exposure --project private-api --format json",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			resolution, err := resolveConfigPathResolution(derefString(configPath), opts)
			if err != nil {
				return err
			}
			read := opts.read
			if read == nil {
				read = func(path string) (globalconfig.Config, error) {
					return globalconfig.Read(path)
				}
			}
			global, err := read(resolution.Path)
			if err != nil {
				return err
			}
			report, err := scanIssueExposure(cmd.Context(), global, projectID, deps)
			if err != nil {
				return err
			}
			return out.Write(func(writer io.Writer) error {
				return writeExposurePretty(writer, report)
			}, report)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "limit the scan to one configured project id")
	return cmd
}

func scanIssueExposure(ctx context.Context, global globalconfig.Config, projectID string, deps exposureDeps) (exposureReport, error) {
	deps = exposureDepsWithDefaults(deps)
	report := exposureReport{}
	matchedProject := strings.TrimSpace(projectID) == ""
	for _, project := range global.Projects {
		if projectID != "" && project.ID != projectID {
			continue
		}
		matchedProject = true
		workflow, err := deps.loadWorkflow(project)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("project %s workflow: %v", project.ID, err))
			continue
		}
		workflow.Config = doctorWorkflowConfigWithRuntimeGitHubToken(workflow.Config, global.GitHubToken)
		switch {
		case project.Identity.Configured():
			workflow.Config.Identity = project.Identity
			workflow.Config.Identity.Normalize()
		case global.Global.Identity.Configured():
			workflow.Config.Identity = global.Global.Identity
			workflow.Config.Identity.Normalize()
		}
		sourceRepository := strings.TrimSpace(workflow.Config.Tracker.Repository)
		destinations := doctorIssueFilingDestinations(workflow.Config)
		if sourceRepository == "" || len(destinations) == 0 {
			continue
		}
		sourceInfo, err := deps.repositoryInfo(ctx, workflow.Config, sourceRepository)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("project %s source visibility: %v", project.ID, err))
			continue
		}
		if !sourceInfo.Private && !strings.EqualFold(sourceInfo.Visibility, "private") && !strings.EqualFold(sourceInfo.Visibility, "internal") {
			continue
		}
		identifiers := exposureIdentifiers(project, workflow.Config)
		for _, destination := range destinations {
			destinationInfo, infoErr := deps.repositoryInfo(ctx, workflow.Config, destination.repository)
			if infoErr != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("project %s destination %s visibility: %v", project.ID, destination.repository, infoErr))
				continue
			}
			if destinationInfo.Private || !strings.EqualFold(destinationInfo.Visibility, "public") {
				continue
			}
			findings, scanErr := deps.scan(ctx, workflow.Config, destination.repository, identifiers)
			if scanErr != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("project %s destination %s scan: %v", project.ID, destination.repository, scanErr))
				continue
			}
			for _, finding := range findings {
				report.Findings = append(report.Findings, exposureFinding{
					SourceProject:         project.ID,
					SourceRepository:      sourceRepository,
					DestinationRepository: destination.repository,
					IssueNumber:           finding.Number,
					IssueURL:              finding.URL,
					MatchedIdentifier:     finding.MatchedIdentifier,
				})
			}
		}
	}
	if !matchedProject {
		return exposureReport{}, projectNotFoundError(projectID, global.Projects)
	}
	sort.Slice(report.Findings, func(i int, j int) bool {
		left := report.Findings[i]
		right := report.Findings[j]
		if left.DestinationRepository != right.DestinationRepository {
			return left.DestinationRepository < right.DestinationRepository
		}
		if left.IssueNumber != right.IssueNumber {
			return left.IssueNumber < right.IssueNumber
		}
		return left.MatchedIdentifier < right.MatchedIdentifier
	})
	sort.Strings(report.Warnings)
	return report, nil
}

func exposureDepsWithDefaults(deps exposureDeps) exposureDeps {
	if deps.loadWorkflow == nil {
		deps.loadWorkflow = projectpkg.LoadWorkflow
	}
	if deps.repositoryInfo == nil {
		deps.repositoryInfo = defaultDoctorGitHubRepositoryInfo
	}
	if deps.scan == nil {
		deps.scan = defaultIssueExposureScan
	}
	return deps
}

func defaultIssueExposureScan(ctx context.Context, cfg workflowconfig.Config, repository string, identifiers []string) (_ []ghconnector.IssueExposure, resultErr error) {
	conn, err := ghconnector.NewConnector(doctorGitHubConnectorConfig(cfg))
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	return conn.ScanIssueExposure(ctx, repository, identifiers)
}

func exposureIdentifiers(project globalconfig.Project, cfg workflowconfig.Config) []string {
	repository := strings.TrimSpace(cfg.Tracker.Repository)
	owner, name, _ := strings.Cut(repository, "/")
	branchRepository := strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(repository)
	return normalizedExposureValues([]string{
		repository,
		owner,
		name,
		strings.TrimSpace(project.Workdir),
		strings.TrimSpace(cfg.Workspace.Root),
		strings.TrimSpace(cfg.Workspace.SourceRoot),
		strings.TrimSpace(cfg.Workspace.OutputRoot),
		strings.TrimSpace(cfg.Identity.GitHubLogin),
		"detent/" + branchRepository + "_",
	})
}

func normalizedExposureValues(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" || value == "detent/_" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func writeExposurePretty(writer io.Writer, report exposureReport) error {
	if _, err := fmt.Fprintf(writer, "Possible public issue exposures: %d\n", len(report.Findings)); err != nil {
		return err
	}
	for _, finding := range report.Findings {
		reference := finding.IssueURL
		if reference == "" {
			reference = fmt.Sprintf("%s#%d", finding.DestinationRepository, finding.IssueNumber)
		}
		if _, err := fmt.Fprintf(writer, "- %s: source project %s matched %q\n", reference, finding.SourceProject, finding.MatchedIdentifier); err != nil {
			return err
		}
	}
	for _, warning := range report.Warnings {
		if _, err := fmt.Fprintln(writer, "Warning: "+warning); err != nil {
			return err
		}
	}
	return nil
}
