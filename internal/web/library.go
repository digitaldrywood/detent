package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
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
	ProjectID       string
	ProjectName     string
	Repository      string
	LocalSQLitePath string
	LocalProjectID  string
	DeliverableKind string
	StatusField     string
}

type localArtifactRow struct {
	ProjectID                   string
	ID                          string
	Identifier                  string
	Title                       string
	State                       string
	URL                         string
	FieldsJSON                  string
	DeliverableKind             string
	DeliverablePath             string
	DeliverableReviewURL        string
	DeliverableValidationStatus string
	DeliverableExternalID       string
	DeliverableMetadataJSON     string
	CreatedAt                   string
	UpdatedAt                   string
	StageUpdatedAt              string
	GitHubIssueNumber           int64
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
		ConnectorName:    s.connector.Name(),
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
			ProjectID:       id,
			ProjectName:     name,
			Repository:      strings.TrimSpace(workflow.Tracker.Repository),
			LocalSQLitePath: localSQLiteArtifactPath(workflow),
			LocalProjectID:  libraryLocalProjectID(workflow.Tracker.LocalSQLite.ProjectID),
			DeliverableKind: strings.TrimSpace(workflow.Deliverable.Kind),
			StatusField:     gate.Effective(workflow.Gate).Artifact.StatusField,
		}
		sources = append(sources, source)
	}
	return sources
}

func (s *Server) libraryRows(ctx context.Context, filters libraryFilters, sources []libraryProjectSource) ([]templates.LibraryRow, []string, error) {
	rows := []templates.LibraryRow{}
	warnings := []string{}
	for _, source := range sources {
		if filters.ProjectID != "" && source.ProjectID != filters.ProjectID {
			continue
		}
		if source.LocalSQLitePath == "" {
			continue
		}
		localRows, warning, err := readLocalArtifactRows(ctx, source)
		if err != nil {
			return nil, nil, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
			s.logger.Warn("artifact library local sqlite read skipped", slog.String("project_id", source.ProjectID), slog.String("path", source.LocalSQLitePath), slog.String("reason", warning))
		}
		rows = append(rows, localRows...)
	}

	validatorRows, err := s.pullRequestLibraryRows(ctx, filters, sources)
	if err != nil {
		return nil, nil, err
	}
	rows = append(rows, validatorRows...)
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
	return rows, warnings, nil
}

func localSQLiteArtifactPath(cfg workflowconfig.Config) string {
	switch cfg.Tracker.Kind {
	case workflowconfig.TrackerLocalSQLite, workflowconfig.TrackerGitHubLocal:
		return strings.TrimSpace(cfg.Tracker.LocalSQLite.Path)
	default:
		return ""
	}
}

func readLocalArtifactRows(ctx context.Context, source libraryProjectSource) ([]templates.LibraryRow, string, error) {
	db, ok, warning, err := openLibrarySQLiteReadOnly(ctx, source.LocalSQLitePath)
	if err != nil || !ok {
		return nil, warning, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
select project_id, id, identifier, title, state, url, fields_json,
deliverable_kind, deliverable_path, deliverable_review_url, deliverable_validation_status,
deliverable_external_id, deliverable_metadata_json, created_at, updated_at, stage_updated_at,
github_issue_number
from detent_work_items
where project_id = ?
order by updated_at desc, id asc`, source.LocalProjectID)
	if err != nil {
		if libraryMissingWorkItemsTable(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("read artifact library rows for %s: %w", source.ProjectID, err)
	}
	defer rows.Close()

	out := []templates.LibraryRow{}
	for rows.Next() {
		var row localArtifactRow
		if err := rows.Scan(
			&row.ProjectID,
			&row.ID,
			&row.Identifier,
			&row.Title,
			&row.State,
			&row.URL,
			&row.FieldsJSON,
			&row.DeliverableKind,
			&row.DeliverablePath,
			&row.DeliverableReviewURL,
			&row.DeliverableValidationStatus,
			&row.DeliverableExternalID,
			&row.DeliverableMetadataJSON,
			&row.CreatedAt,
			&row.UpdatedAt,
			&row.StageUpdatedAt,
			&row.GitHubIssueNumber,
		); err != nil {
			return nil, "", err
		}
		libraryRow, ok := localArtifactLibraryRow(source, row)
		if ok {
			out = append(out, libraryRow)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return out, "", nil
}

func localArtifactLibraryRow(source libraryProjectSource, row localArtifactRow) (templates.LibraryRow, bool) {
	fields := libraryStringMap(row.FieldsJSON)
	metadata := libraryStringMap(row.DeliverableMetadataJSON)
	kind := libraryFirstNonBlank(row.DeliverableKind, source.DeliverableKind)
	status := libraryFirstNonBlank(
		row.DeliverableValidationStatus,
		fields[source.StatusField],
		fields[gate.DefaultArtifactStatusField],
		fields["render_status"],
	)
	hasDeliverable := kind != "" ||
		strings.TrimSpace(row.DeliverablePath) != "" ||
		strings.TrimSpace(row.DeliverableReviewURL) != "" ||
		status != "" ||
		strings.TrimSpace(row.DeliverableExternalID) != "" ||
		len(metadata) > 0
	if !hasDeliverable && source.DeliverableKind != workflowconfig.DeliverableArtifact {
		return templates.LibraryRow{}, false
	}
	if kind == "" {
		kind = workflowconfig.DeliverableArtifact
	}
	createdAt := libraryParseTime(row.CreatedAt)
	updatedAt := libraryFirstTime(libraryParseTime(row.UpdatedAt), libraryParseTime(row.StageUpdatedAt), createdAt)
	sourceURL := strings.TrimSpace(row.URL)
	if sourceURL == "" && row.GitHubIssueNumber > 0 {
		sourceURL = githubIssueURL(source.Repository, int(row.GitHubIssueNumber))
	}
	return templates.LibraryRow{
		ID:               "artifact:" + source.ProjectID + ":" + strings.TrimSpace(row.ID),
		SourceKind:       "artifact",
		ProjectID:        source.ProjectID,
		ProjectName:      source.ProjectName,
		Kind:             kind,
		ArtifactPath:     strings.TrimSpace(row.DeliverablePath),
		ValidationStatus: status,
		ReviewURL:        strings.TrimSpace(row.DeliverableReviewURL),
		SourceURL:        sourceURL,
		SourceLabel:      librarySourceLabel(row.Identifier, row.ID),
		Title:            strings.TrimSpace(row.Title),
		State:            strings.TrimSpace(row.State),
		ExternalID:       strings.TrimSpace(row.DeliverableExternalID),
		Metadata:         libraryMetadataSummary(metadata),
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}, true
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

func openLibrarySQLiteReadOnly(ctx context.Context, path string) (*sql.DB, bool, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false, "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, "database does not exist", nil
		}
		return nil, false, "", err
	}
	if info.IsDir() {
		return nil, false, "database path is a directory", nil
	}
	db, err := sql.Open("sqlite", librarySQLiteReadOnlyDSN(path))
	if err != nil {
		return nil, false, "", fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		closeErr := db.Close()
		return nil, false, "", errors.Join(fmt.Errorf("enable query_only: %w", err), closeErr)
	}
	if err := db.PingContext(ctx); err != nil {
		closeErr := db.Close()
		return nil, false, "", errors.Join(err, closeErr)
	}
	return db, true, "", nil
}

func librarySQLiteReadOnlyDSN(path string) string {
	values := url.Values{}
	values.Set("mode", "ro")
	values.Set("cache", "shared")
	return "file:" + libraryEscapeSQLiteURIPath(librarySQLiteURIPath(path)) + "?" + values.Encode()
}

func librarySQLiteURIPath(path string) string {
	cleaned := filepath.Clean(path)
	uriPath := filepath.ToSlash(cleaned)
	if libraryWindowsDrivePath(uriPath) {
		uriPath = strings.ReplaceAll(cleaned, `\`, "/")
		if !strings.HasPrefix(uriPath, "/") {
			uriPath = "/" + uriPath
		}
	}
	return uriPath
}

func libraryWindowsDrivePath(path string) bool {
	return len(path) >= 2 && path[1] == ':' &&
		(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z')
}

func libraryEscapeSQLiteURIPath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func libraryMissingWorkItemsTable(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no such table: detent_work_items")
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

func libraryStringMap(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

func libraryMetadataSummary(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(metadata[key]) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > 3 {
		keys = keys[:3]
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+metadata[key])
	}
	return strings.Join(parts, " · ")
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

func libraryLocalProjectID(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "default"
	}
	return projectID
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

func libraryFirstTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func libraryParseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
