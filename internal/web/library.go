package web

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type libraryFilters struct {
	ProjectID string
	Kind      string
	Status    string
	From      time.Time
	To        time.Time
	FromValue string
	ToValue   string
}

type libraryProjectSource struct {
	ProjectID   string
	ProjectName string
	Repository  string
}

func (s *Server) library(c echo.Context) error {
	filters, response, status := libraryFiltersFromRequest(c)
	if response != nil {
		return c.JSON(status, response)
	}
	data, err := s.libraryData(c.Request().Context(), filters)
	if err != nil {
		s.logger.Error("artifact library page failed", slog.Any("error", err))
		return c.JSON(http.StatusInternalServerError, errorResponse("artifact_library_failed", "Artifact library failed"))
	}
	applyLibraryPreferences(c.Request(), &data)
	return render(c, templates.LibraryPage(data))
}

func libraryFiltersFromRequest(c echo.Context) (libraryFilters, *apiErrorResponse, int) {
	from, response, status := usageDate("from", c.QueryParam("from"))
	if response != nil {
		return libraryFilters{}, response, status
	}
	to, response, status := usageDate("to", c.QueryParam("to"))
	if response != nil {
		return libraryFilters{}, response, status
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		response := errorResponse("invalid_date_range", "from must be on or before to")
		return libraryFilters{}, &response, http.StatusBadRequest
	}
	return libraryFilters{
		ProjectID: strings.TrimSpace(c.QueryParam("project")),
		Kind:      strings.TrimSpace(c.QueryParam("kind")),
		Status:    strings.TrimSpace(c.QueryParam("status")),
		From:      from,
		To:        to,
		FromValue: strings.TrimSpace(c.QueryParam("from")),
		ToValue:   strings.TrimSpace(c.QueryParam("to")),
	}, nil, 0
}

func (s *Server) libraryData(ctx context.Context, filters libraryFilters) (templates.LibraryData, error) {
	instanceName := s.instanceName()
	snapshot := s.latestSnapshot(ctx)
	sidebarProjects := s.projectSmallMultiples(ctx, snapshot)
	shellProjectID, shellProjectName, _ := s.sidebarProjectContext(filters.ProjectID, sidebarProjects, snapshot)

	sources := s.libraryProjectSources(sidebarProjects)
	rows, warnings, err := s.libraryRows(ctx, filters, sources)
	if err != nil {
		return templates.LibraryData{}, err
	}
	allRows := append([]templates.LibraryRow(nil), rows...)
	filtered := filterLibraryRows(rows, filters)

	return templates.LibraryData{
		Title:            instancePageTitle(instanceName, "Detent Library"),
		ApplicationName:  applicationName(instanceName),
		InstanceName:     instanceName,
		Version:          s.version,
		Build:            s.build,
		ConnectorName:    s.connectorName(),
		Snapshot:         snapshot,
		Assets:           s.assets.templatePaths(),
		SidebarProjects:  sidebarProjects,
		ActiveNav:        "library",
		ProjectID:        shellProjectID,
		ProjectName:      shellProjectName,
		Filters:          templates.LibraryFilters(filters),
		Summary:          librarySummary(filtered),
		Rows:             filtered,
		ProjectOptions:   libraryProjectOptions(sidebarProjects, allRows, filters.ProjectID),
		KindOptions:      libraryValueOptions(allRows, filters.Kind, func(row templates.LibraryRow) string { return row.Kind }),
		StatusOptions:    libraryValueOptions(allRows, filters.Status, func(row templates.LibraryRow) string { return row.ValidationStatus }),
		Warnings:         warnings,
		UnfilteredCount:  len(allRows),
		FilteredCount:    len(filtered),
		HasActiveFilters: libraryHasActiveFilters(filters),
	}, nil
}

func (s *Server) libraryProjectSources(sidebarProjects []templates.ProjectSmallMultiple) []libraryProjectSource {
	if s.registry == nil {
		return nil
	}
	projectNames := map[string]string{}
	for _, project := range sidebarProjects {
		id := strings.TrimSpace(project.ID)
		if id != "" {
			projectNames[id] = strings.TrimSpace(project.Name)
		}
	}
	trackedProjects := s.registry.List()
	sources := make([]libraryProjectSource, 0, len(trackedProjects))
	for _, trackedProject := range trackedProjects {
		if trackedProject == nil {
			continue
		}
		id := strings.TrimSpace(string(trackedProject.ID()))
		if id == "" {
			continue
		}
		workflow := trackedProject.Workflow().Config
		name := strings.TrimSpace(projectNames[id])
		if name == "" {
			name = id
		}
		source := libraryProjectSource{
			ProjectID:   id,
			ProjectName: name,
			Repository:  strings.TrimSpace(workflow.Tracker.Repository),
		}
		sources = append(sources, source)
	}
	return sources
}

func (s *Server) libraryRows(ctx context.Context, filters libraryFilters, sources []libraryProjectSource) ([]templates.LibraryRow, []string, error) {
	rows, err := s.pullRequestLibraryRows(ctx, filters, sources)
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left := rows[i].UpdatedAt
		right := rows[j].UpdatedAt
		if left.IsZero() && right.IsZero() {
			return rows[i].ID < rows[j].ID
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		if left.Equal(right) {
			return rows[i].ID < rows[j].ID
		}
		return left.After(right)
	})
	return rows, nil, nil
}

func (s *Server) pullRequestLibraryRows(ctx context.Context, filters libraryFilters, sources []libraryProjectSource) ([]templates.LibraryRow, error) {
	if s.store == nil {
		return nil, nil
	}
	query := store.ValidatorVerdictQuery{
		ProjectID: filters.ProjectID,
		From:      filters.From,
		To:        libraryExclusiveTo(filters.To),
	}
	verdicts, err := s.store.ListValidatorVerdicts(ctx, query)
	if err != nil {
		return nil, err
	}
	sourceByProject := map[string]libraryProjectSource{}
	for _, source := range sources {
		sourceByProject[source.ProjectID] = source
	}
	rows := make([]templates.LibraryRow, 0, len(verdicts))
	seen := map[string]struct{}{}
	for _, verdict := range verdicts {
		if verdict.PRNumber == nil || *verdict.PRNumber <= 0 {
			continue
		}
		key := strings.Join([]string{verdict.ProjectID, verdict.IssueID, strconv.FormatInt(*verdict.PRNumber, 10)}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		source := sourceByProject[strings.TrimSpace(verdict.ProjectID)]
		prURL := libraryPullRequestURL(verdict.IssueURL, source.Repository, int(*verdict.PRNumber))
		rows = append(rows, templates.LibraryRow{
			ID:               "pr:" + verdict.ProjectID + ":" + verdict.IssueID + ":" + strconv.FormatInt(*verdict.PRNumber, 10),
			SourceKind:       "pull_request",
			ProjectID:        verdict.ProjectID,
			ProjectName:      libraryFirstNonBlank(source.ProjectName, verdict.ProjectID),
			Kind:             workflowconfig.DeliverablePullRequest,
			ArtifactPath:     "PR #" + strconv.FormatInt(*verdict.PRNumber, 10),
			ValidationStatus: strings.TrimSpace(verdict.Verdict),
			ReviewURL:        prURL,
			SourceURL:        strings.TrimSpace(verdict.IssueURL),
			SourceLabel:      librarySourceLabel(verdict.Identifier, verdict.IssueID),
			PullRequestURL:   prURL,
			PullRequestLabel: "PR #" + strconv.FormatInt(*verdict.PRNumber, 10),
			Title:            strings.TrimSpace(verdict.Summary),
			ExternalID:       shortSHA(verdict.HeadSHA),
			Metadata:         validatorMetadata(verdict),
			CreatedAt:        verdict.RecordedAt,
			UpdatedAt:        verdict.UpdatedAt,
		})
	}
	return rows, nil
}

func filterLibraryRows(rows []templates.LibraryRow, filters libraryFilters) []templates.LibraryRow {
	out := make([]templates.LibraryRow, 0, len(rows))
	from := filters.From
	to := libraryExclusiveTo(filters.To)
	for _, row := range rows {
		if filters.ProjectID != "" && row.ProjectID != filters.ProjectID {
			continue
		}
		if filters.Kind != "" && !strings.EqualFold(strings.TrimSpace(row.Kind), filters.Kind) {
			continue
		}
		if filters.Status != "" && !strings.EqualFold(strings.TrimSpace(row.ValidationStatus), filters.Status) {
			continue
		}
		if !from.IsZero() && (row.UpdatedAt.IsZero() || row.UpdatedAt.Before(from)) {
			continue
		}
		if !to.IsZero() && (row.UpdatedAt.IsZero() || !row.UpdatedAt.Before(to)) {
			continue
		}
		out = append(out, row)
	}
	return out
}

func libraryExclusiveTo(to time.Time) time.Time {
	if to.IsZero() {
		return time.Time{}
	}
	return to.AddDate(0, 0, 1)
}

func librarySummary(rows []templates.LibraryRow) templates.LibrarySummary {
	projects := map[string]struct{}{}
	summary := templates.LibrarySummary{Total: len(rows)}
	for _, row := range rows {
		if row.ProjectID != "" {
			projects[row.ProjectID] = struct{}{}
		}
		switch row.SourceKind {
		case "pull_request":
			summary.PullRequests++
		default:
			summary.Artifacts++
		}
		if libraryPassingStatus(row.ValidationStatus) {
			summary.Validated++
		}
	}
	summary.Projects = len(projects)
	return summary
}

func libraryProjectOptions(projects []templates.ProjectSmallMultiple, rows []templates.LibraryRow, selected string) []templates.LibraryFilterOption {
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.ProjectID]++
	}
	seen := map[string]struct{}{}
	options := make([]templates.LibraryFilterOption, 0, len(projects)+1)
	options = append(options, templates.LibraryFilterOption{Label: "All projects", Count: len(rows), Selected: strings.TrimSpace(selected) == ""})
	for _, project := range projects {
		id := strings.TrimSpace(project.ID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
		label := strings.TrimSpace(project.Name)
		if label == "" {
			label = id
		}
		options = append(options, templates.LibraryFilterOption{
			Value:    id,
			Label:    label,
			Count:    counts[id],
			Selected: selected == id,
		})
	}
	extraProjectIDs := make([]string, 0, len(counts))
	for projectID := range counts {
		if _, ok := seen[projectID]; ok || strings.TrimSpace(projectID) == "" {
			continue
		}
		extraProjectIDs = append(extraProjectIDs, projectID)
	}
	sort.Strings(extraProjectIDs)
	for _, projectID := range extraProjectIDs {
		options = append(options, templates.LibraryFilterOption{
			Value:    projectID,
			Label:    projectID,
			Count:    counts[projectID],
			Selected: selected == projectID,
		})
	}
	return options
}

func libraryValueOptions(rows []templates.LibraryRow, selected string, value func(templates.LibraryRow) string) []templates.LibraryFilterOption {
	counts := map[string]int{}
	labels := map[string]string{}
	for _, row := range rows {
		raw := strings.TrimSpace(value(row))
		if raw == "" {
			continue
		}
		key := strings.ToLower(raw)
		counts[key]++
		if labels[key] == "" {
			labels[key] = raw
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	options := make([]templates.LibraryFilterOption, 0, len(keys)+1)
	options = append(options, templates.LibraryFilterOption{Label: "All", Count: len(rows), Selected: strings.TrimSpace(selected) == ""})
	for _, key := range keys {
		options = append(options, templates.LibraryFilterOption{
			Value:    labels[key],
			Label:    labels[key],
			Count:    counts[key],
			Selected: strings.EqualFold(selected, labels[key]),
		})
	}
	return options
}

func libraryHasActiveFilters(filters libraryFilters) bool {
	return filters.ProjectID != "" ||
		filters.Kind != "" ||
		filters.Status != "" ||
		filters.FromValue != "" ||
		filters.ToValue != ""
}

func validatorMetadata(verdict store.ValidatorVerdict) string {
	parts := []string{}
	if verdict.Score > 0 {
		parts = append(parts, "score "+strconv.Itoa(int(verdict.Score*100))+"%")
	}
	if len(verdict.Findings) > 0 {
		parts = append(parts, strconv.Itoa(len(verdict.Findings))+" findings")
	}
	if verdict.Submitted {
		parts = append(parts, "submitted")
	}
	return strings.Join(parts, " · ")
}

func libraryPassingStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "approved", "complete", "completed", "pass", "passed", "valid":
		return true
	default:
		return false
	}
}

func librarySourceLabel(identifier string, id string) string {
	if label := strings.TrimSpace(identifier); label != "" {
		return label
	}
	if label := strings.TrimSpace(id); label != "" {
		return label
	}
	return "unassigned"
}

func githubIssueURL(repository string, number int) string {
	repository = strings.TrimSpace(repository)
	if repository == "" || number <= 0 {
		return ""
	}
	return "https://github.com/" + repository + "/issues/" + strconv.Itoa(number)
}

func libraryPullRequestURL(issueURL string, repository string, number int) string {
	if number <= 0 {
		return ""
	}
	if repository != "" {
		return "https://github.com/" + strings.TrimSpace(repository) + "/pull/" + strconv.Itoa(number)
	}
	issueURL = strings.TrimSpace(issueURL)
	if issueURL == "" {
		return ""
	}
	if before, _, ok := strings.Cut(issueURL, "/issues/"); ok {
		return before + "/pull/" + strconv.Itoa(number)
	}
	return ""
}

func shortSHA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func libraryFirstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
