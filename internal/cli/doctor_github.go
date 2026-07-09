package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

var (
	requiredProjectV2GitHubScopes  = []string{"repo", "read:org", "read:project", "project"}
	requiredIssueFieldGitHubScopes = []string{"repo", "read:org"}
	requiredLabelGitHubScopes      = []string{"repo"}
)

type doctorGitHubReadinessFunc func(context.Context, ghconnector.Config, ghconnector.ReadinessConfig) ([]ghconnector.ReadinessCheck, error)

func checkDoctorGitHubReadiness(
	ctx context.Context,
	id string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
	githubToken RuntimeSecret,
	sourceRoot string,
	allowWriteProbes bool,
) []doctorCheck {
	readiness := doctorGitHubReadinessConfig(ctx, project, cfg, deps, githubToken, sourceRoot)
	readiness, writeProbeCheck := doctorGitHubReadinessWithWriteProbeAuthorization(id, readiness, allowWriteProbes)
	checks, err := deps.githubReadiness(ctx, doctorGitHubConnectorConfig(cfg), readiness)
	if err != nil {
		return []doctorCheck{{
			Name:   "Project " + id + " GitHub readiness",
			Status: doctorFail,
			Detail: "create GitHub readiness checker: " + err.Error(),
			Hint:   "Fix GitHub tracker configuration and credentials.",
		}}
	}
	out := make([]doctorCheck, 0, len(checks))
	for _, check := range checks {
		out = append(out, doctorCheck{
			Name:   "Project " + id + " " + check.Name,
			Status: doctorStatusFromGitHubReadiness(check.Status),
			Detail: check.Detail,
			Hint:   check.Hint,
		})
	}
	if writeProbeCheck != nil {
		out = append(out, *writeProbeCheck)
	}
	return out
}

func doctorGitHubReadinessWithWriteProbeAuthorization(id string, cfg ghconnector.ReadinessConfig, allowWriteProbes bool) (ghconnector.ReadinessConfig, *doctorCheck) {
	if allowWriteProbes || !doctorGitHubReadinessRequiresWrites(cfg) {
		return cfg, nil
	}

	detail := "skipped; rerun detent doctor with --allow-write-probes after mutation authorization"
	if probe := strings.TrimSpace(cfg.WriteProbeIssue); probe != "" {
		detail = "skipped for tracker.write_probe_issue " + probe + "; rerun detent doctor with --allow-write-probes after mutation authorization"
	}
	check := doctorCheck{
		Name:   "Project " + id + " GitHub write probes",
		Status: doctorWarn,
		Detail: detail,
		Hint:   `Run detent onboarding validate-answers --phase mutation and get operator confirmation before enabling write probes.`,
	}
	cfg.RequireProjectStatusWrite = false
	cfg.RequireIssueFieldStatusWrite = false
	cfg.RequireLabelStatusWrite = false
	cfg.RequireIssueComments = false
	cfg.RequireAssigneeWrite = false
	cfg.RequireIssueClose = false
	cfg.ProjectFieldWrites = nil
	return cfg, &check
}

func doctorGitHubReadinessRequiresWrites(cfg ghconnector.ReadinessConfig) bool {
	return cfg.RequireProjectStatusWrite ||
		cfg.RequireIssueFieldStatusWrite ||
		cfg.RequireLabelStatusWrite ||
		cfg.RequireIssueComments ||
		cfg.RequireAssigneeWrite ||
		cfg.RequireIssueClose ||
		len(cfg.ProjectFieldWrites) > 0
}

func doctorGitHubConnectorConfig(cfg workflowconfig.Config) ghconnector.Config {
	return ghconnector.Config{
		Endpoint: cfg.Tracker.Endpoint,
		APIKey:   cfg.Tracker.APIKey,
		HTTPTransport: ghconnector.HTTPTransportConfig{
			MaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
			MaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
			IdleConnTimeout:     time.Duration(cfg.Tracker.HTTPIdleConnTimeoutMS) * time.Millisecond,
		},
		RESTMinRemainingReserve: cfg.Tracker.GitHubRESTMinReserve,
		RESTFanoutMaxRequests:   cfg.Tracker.GitHubRESTFanoutMaxRequests,
		RESTDebugLogging:        cfg.Tracker.GitHubRESTDebugLogging,
		GitHubAppID:             cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:     cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath: cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID: cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:      cfg.Tracker.GitHubStatusSource,
		DependencySource:        cfg.Dependencies.Source,
		ProjectSlug:             cfg.Tracker.ProjectSlug,
		Repository:              cfg.Tracker.Repository,
		StatusField:             cfg.Tracker.StatusField,
		StatusLabelPrefix:       cfg.Tracker.StatusLabelPrefix,
		ActiveStates:            cfg.Tracker.ActiveStates,
		ObservedStates:          cfg.Tracker.ObservedStates,
		TerminalStates:          cfg.Tracker.TerminalStates,
		StateMap:                doctorTrackerStateMap(cfg.Tracker.StateMap),
		Logger:                  doctorConnectorLogger(),
	}
}

func doctorConnectorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func doctorGitHubReadinessConfig(
	ctx context.Context,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
	githubToken RuntimeSecret,
	sourceRoot string,
) ghconnector.ReadinessConfig {
	if cfg.Tracker.Kind == workflowconfig.TrackerGitHubLocal {
		return ghconnector.ReadinessConfig{
			AuthPath:                      doctorGitHubAuthPath(cfg, githubToken, deps.lookupEnv),
			LocalStatusMode:               true,
			Repositories:                  doctorGitHubRepositories(ctx, project, cfg, deps, sourceRoot),
			RequireIssueCommentsRead:      true,
			RequireDependencyMetadataRead: true,
			RequireIssueChildrenRead:      doctorRequiresIssueChildrenRead(cfg),
			RequireIssueParentsRead:       doctorRequiresIssueParentsRead(cfg),
			RequirePullRequestRead:        true,
			RequirePullRequestReviews:     true,
			RequirePullRequestChecks:      true,
		}
	}
	return ghconnector.ReadinessConfig{
		AuthPath:                      doctorGitHubAuthPath(cfg, githubToken, deps.lookupEnv),
		WriteProbeIssue:               cfg.Tracker.WriteProbeIssue,
		Repositories:                  doctorGitHubRepositories(ctx, project, cfg, deps, sourceRoot),
		StatusStates:                  doctorRequiredGitHubStatusStates(cfg),
		ReadStates:                    doctorRequiredGitHubReadStates(cfg),
		RequireProjectRead:            doctorRequiresProjectRead(cfg),
		RequireIssueFieldRead:         doctorRequiresIssueFieldRead(cfg),
		RequireIssueCommentsRead:      doctorRequiresIssueCommentsRead(cfg),
		RequireDependencyMetadataRead: doctorRequiresDependencyMetadataRead(cfg),
		RequireIssueChildrenRead:      doctorRequiresIssueChildrenRead(cfg),
		RequireIssueParentsRead:       doctorRequiresIssueParentsRead(cfg),
		RequirePullRequestRead:        doctorRequiresPullRequestRead(cfg),
		RequirePullRequestReviews:     doctorRequiresPullRequestReviewsRead(cfg),
		RequirePullRequestChecks:      doctorRequiresPullRequestChecksRead(cfg),
		RequireProjectStatusWrite:     doctorRequiresProjectStatusWrite(cfg),
		RequireIssueFieldStatusWrite:  doctorRequiresIssueFieldStatusWrite(cfg),
		RequireLabelStatusWrite:       doctorRequiresLabelStatusWrite(cfg),
		RequireIssueComments:          doctorRequiresIssueCommentWrite(cfg),
		RequireAssigneeWrite:          doctorRequiresAssigneeWrite(cfg),
		RequireIssueClose:             doctorRequiresIssueClose(cfg),
		ProjectFieldWrites:            doctorRequiredProjectFieldWrites(cfg),
	}
}

func doctorStatusFromGitHubReadiness(status ghconnector.ReadinessStatus) doctorStatus {
	switch status {
	case ghconnector.ReadinessOK:
		return doctorOK
	case ghconnector.ReadinessWarn:
		return doctorWarn
	case ghconnector.ReadinessFail:
		return doctorFail
	default:
		return doctorWarn
	}
}

func doctorGitHubAuthPath(cfg workflowconfig.Config, token RuntimeSecret, lookupEnv func(string) string) string {
	if trackerHasGitHubAppCredentials(cfg.Tracker, lookupEnv) {
		return "GitHub App installation token"
	}
	if token.ResolvedVia == "gh" {
		return "gh-resolved token"
	}
	switch strings.TrimSpace(token.Source) {
	case "GITHUB_TOKEN":
		return "GITHUB_TOKEN PAT"
	case "github_token":
		return "global github_token PAT"
	case "":
		if strings.TrimSpace(cfg.Tracker.APIKey) != "" {
			return "workflow tracker.api_key"
		}
		return "GitHub token"
	default:
		return token.Source + " PAT"
	}
}

func doctorRequiredGitHubStatusStates(cfg workflowconfig.Config) []string {
	return uniqueDoctorStrings(append(append([]string{}, cfg.Tracker.ActiveStates...), append(cfg.Tracker.ObservedStates, cfg.Tracker.TerminalStates...)...))
}

func doctorRequiredGitHubReadStates(cfg workflowconfig.Config) []string {
	return uniqueDoctorStrings(append(append([]string{}, cfg.Tracker.ActiveStates...), cfg.Tracker.ObservedStates...))
}

func doctorRequiresProjectRead(cfg workflowconfig.Config) bool {
	return cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceProjectV2
}

func doctorRequiresIssueFieldRead(cfg workflowconfig.Config) bool {
	return cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceIssueField
}

func doctorRequiresLabelRead(cfg workflowconfig.Config) bool {
	return cfg.Tracker.GitHubStatusSource == workflowconfig.GitHubStatusSourceLabel
}

func doctorRequiresStatusWrite(cfg workflowconfig.Config) bool {
	return doctorKanbanIntegrationEnabled(cfg) ||
		len(cfg.Tracker.ActiveStates) > 0 ||
		cfg.Agent.AutoPromote.Enabled ||
		cfg.Tracker.DependencyAutoUnblock.Enabled
}

func doctorRequiresProjectStatusWrite(cfg workflowconfig.Config) bool {
	return doctorRequiresProjectRead(cfg) && doctorRequiresStatusWrite(cfg)
}

func doctorRequiresIssueFieldStatusWrite(cfg workflowconfig.Config) bool {
	return doctorRequiresIssueFieldRead(cfg) && doctorRequiresStatusWrite(cfg)
}

func doctorRequiresLabelStatusWrite(cfg workflowconfig.Config) bool {
	return doctorRequiresLabelRead(cfg) && doctorRequiresStatusWrite(cfg)
}

func doctorRequiresIssueCommentWrite(cfg workflowconfig.Config) bool {
	return doctorKanbanIntegrationEnabled(cfg) ||
		cfg.Agent.AutoPromote.Enabled ||
		cfg.Tracker.DependencyAutoUnblock.Enabled ||
		doctorRequiresIssueClose(cfg)
}

func doctorKanbanIntegrationEnabled(cfg workflowconfig.Config) bool {
	kanban := cfg.Server.Kanban
	kanban.Normalize()
	return kanban.Mode == workflowconfig.KanbanModeIntegration
}

func doctorRequiresIssueCommentsRead(cfg workflowconfig.Config) bool {
	return doctorStateInList("Blocked", cfg.Tracker.ObservedStates) ||
		doctorStateInList("Blocked", cfg.Tracker.ActiveStates) ||
		doctorStateInList("Blocked", cfg.Tracker.DependencyAutoUnblock.SourceStates)
}

func doctorRequiresDependencyMetadataRead(cfg workflowconfig.Config) bool {
	return len(cfg.Tracker.ActiveStates) > 0 ||
		cfg.Tracker.DependencyAutoUnblock.Enabled ||
		doctorRequiresIssueParentsRead(cfg)
}

func doctorRequiresIssueChildrenRead(cfg workflowconfig.Config) bool {
	return doctorRequiresIssueClose(cfg)
}

func doctorRequiresIssueParentsRead(cfg workflowconfig.Config) bool {
	return doctorRequiresIssueClose(cfg)
}

func doctorRequiresAssigneeWrite(cfg workflowconfig.Config) bool {
	if !cfg.Tracker.Claims.Enabled {
		return false
	}
	identity := cfg.Identity
	identity.Normalize()
	return identity.OwnershipMode != workflowconfig.IdentityOwnershipField
}

func doctorRequiresIssueClose(cfg workflowconfig.Config) bool {
	return len(cfg.Tracker.TerminalStates) > 0
}

func doctorRequiresPullRequestRead(cfg workflowconfig.Config) bool {
	return len(cfg.Tracker.ActiveStates) > 0 ||
		cfg.Agent.AutoPromote.Enabled ||
		doctorStateInList("Human Review", cfg.Tracker.ObservedStates) ||
		doctorStateInList("Merging", cfg.Tracker.ActiveStates)
}

func doctorRequiresPullRequestReviewsRead(cfg workflowconfig.Config) bool {
	return doctorRequiresPullRequestRead(cfg)
}

func doctorRequiresPullRequestChecksRead(cfg workflowconfig.Config) bool {
	return doctorRequiresPullRequestRead(cfg)
}

func doctorRequiredProjectFieldWrites(cfg workflowconfig.Config) []ghconnector.ReadinessProjectFieldWrite {
	if !doctorRequiresProjectRead(cfg) {
		return nil
	}
	if !cfg.Tracker.Claims.Enabled {
		return nil
	}
	fields := []ghconnector.ReadinessProjectFieldWrite{}
	if field := strings.TrimSpace(cfg.Tracker.Claims.LeaseField); field != "" {
		fields = append(fields, ghconnector.ReadinessProjectFieldWrite{Name: field})
	}
	identity := cfg.Identity
	identity.Normalize()
	if identity.OwnershipMode == workflowconfig.IdentityOwnershipField {
		if field := strings.TrimSpace(identity.OwnerField); field != "" {
			fields = append(fields, ghconnector.ReadinessProjectFieldWrite{Name: field})
		}
	}
	return fields
}

func doctorGitHubRepositories(
	ctx context.Context,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
	sourceRoot string,
) []string {
	repositories := []string{}
	if strings.TrimSpace(cfg.Tracker.Repository) != "" {
		repositories = append(repositories, cfg.Tracker.Repository)
	}
	if repo, ok := doctorGitHubRepositoryFromProbe(cfg.Tracker.WriteProbeIssue); ok {
		repositories = append(repositories, repo)
	}
	if repo, ok := doctorGitHubRepositoryFromProbe(project.Workdir); ok {
		repositories = append(repositories, repo)
	}
	if strings.TrimSpace(sourceRoot) != "" && deps.gitRemoteURL != nil {
		if remote, err := deps.gitRemoteURL(ctx, sourceRoot); err == nil {
			if repo, ok := doctorGitHubRepositoryFromRemoteURL(remote); ok {
				repositories = append(repositories, repo)
			}
		}
	}
	return uniqueDoctorStrings(repositories)
}

func doctorGitHubRepositoryFromProbe(value string) (string, bool) {
	repo, _, ok := strings.Cut(strings.TrimSpace(value), "#")
	if !ok {
		return "", false
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return "", false
	}
	return strings.TrimSpace(owner) + "/" + strings.TrimSpace(name), true
}

func doctorGitHubRepositoryFromRemoteURL(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}
	if after, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		return doctorCleanGitHubRepository(after)
	}
	if parsed, err := url.Parse(remote); err == nil && strings.EqualFold(parsed.Hostname(), "github.com") {
		return doctorCleanGitHubRepository(strings.TrimPrefix(parsed.Path, "/"))
	}
	return "", false
}

func doctorCleanGitHubRepository(path string) (string, bool) {
	path = strings.TrimSpace(strings.TrimSuffix(path, ".git"))
	parts := strings.Split(path, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[0]) + "/" + strings.TrimSpace(parts[1]), true
}

func uniqueDoctorStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func doctorTrackerStateMap(value workflowconfig.StringOrMap) map[string]string {
	if !value.IsMap {
		return nil
	}

	out := make(map[string]string, len(value.Map))
	for state, mapped := range value.Map {
		mappedState, ok := mapped.(string)
		if !ok {
			continue
		}
		state = strings.TrimSpace(state)
		mappedState = strings.TrimSpace(mappedState)
		if state != "" && mappedState != "" {
			out[state] = mappedState
		}
	}
	return out
}

func doctorWorkflowConfigWithRuntimeGitHubToken(cfg workflowconfig.Config, token string) workflowconfig.Config {
	token = strings.TrimSpace(token)
	if token != "" && doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
		cfg.Tracker.APIKey = token
	}
	return cfg
}

func doctorTrackerUsesGitHubReads(kind string) bool {
	return kind == workflowconfig.TrackerGitHub || kind == workflowconfig.TrackerGitHubLocal
}

func checkDoctorGitHub(ctx context.Context, cfg *globalconfig.Config, token RuntimeSecret, deps doctorDeps) doctorCheck {
	hasGitHubProject := doctorHasGitHubProject(ctx, cfg, deps)
	requiresRuntimeToken := doctorRequiresRuntimeGitHubToken(ctx, cfg, deps)
	if cfg != nil && !hasGitHubProject {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorWarn,
			Detail: "no GitHub tracker projects configured; token scope check skipped",
			Hint:   "Add a GitHub project before relying on GitHub token preflight checks.",
		}
	}
	if token.Value == "" && !requiresRuntimeToken && hasGitHubProject {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorOK,
			Detail: "GitHub App credentials configured; installation permissions checked per project",
		}
	}
	if token.Value == "" {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorFail,
			Detail: "GITHUB_TOKEN is not set, github_token is not configured, and no usable tracker.api_key was found",
			Hint:   githubAuthHint,
		}
	}

	source := doctorGitHubTokenSource(token)
	scopes, err := deps.githubScopes(ctx, token.Value)
	if err != nil {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s scope check failed: %v", source, err),
			Hint:   githubAuthHint,
		}
	}
	if len(scopes) == 0 {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorOK,
			Detail: source + " did not expose classic OAuth scopes; treating as fine-grained or resource-scoped token and relying on operation checks",
		}
	}
	requiredScopes := doctorRequiredGitHubScopes(ctx, cfg, deps)
	missing := missingGitHubScopes(scopes, requiredScopes)
	if len(missing) > 0 {
		return doctorCheck{
			Name:   "GitHub token",
			Status: doctorFail,
			Detail: fmt.Sprintf("%s missing scope(s): %s", source, strings.Join(missing, ", ")),
			Hint:   githubAuthHint,
		}
	}

	return doctorCheck{
		Name:   "GitHub token",
		Status: doctorOK,
		Detail: fmt.Sprintf("%s has classic PAT scopes: %s; operation checks still verify resource access", source, strings.Join(requiredScopes, ", ")),
	}
}

func doctorHasGitHubProject(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) bool {
	if cfg != nil {
		for _, project := range cfg.Projects {
			workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
			if err != nil || !doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
				continue
			}
			return true
		}
	}
	return false
}

func doctorRequiresRuntimeGitHubToken(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) bool {
	if cfg == nil {
		return true
	}
	for _, project := range cfg.Projects {
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil || !doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
			continue
		}
		if trackerHasGitHubAppCredentials(workflow.Config.Tracker, deps.lookupEnv) {
			continue
		}
		return true
	}
	return false
}

func doctorGitHubTokenSource(token RuntimeSecret) string {
	if token.ResolvedVia == "gh" {
		return "github_token resolved via gh"
	}
	if source := strings.TrimSpace(token.Source); source != "" {
		return source
	}
	return "github_token"
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		if index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func doctorRequiredGitHubScopes(ctx context.Context, cfg *globalconfig.Config, deps doctorDeps) []string {
	if cfg == nil {
		return append([]string{}, requiredProjectV2GitHubScopes...)
	}
	required := []string{}
	add := func(scopes []string) {
		for _, scope := range scopes {
			scope = strings.TrimSpace(scope)
			if scope == "" || doctorStringSliceContains(required, scope) {
				continue
			}
			required = append(required, scope)
		}
	}
	for _, project := range cfg.Projects {
		workflow, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil || !doctorTrackerUsesGitHubReads(workflow.Config.Tracker.Kind) {
			continue
		}
		if trackerHasGitHubAppCredentials(workflow.Config.Tracker, deps.lookupEnv) {
			continue
		}
		if workflow.Config.Tracker.Kind == workflowconfig.TrackerGitHubLocal {
			add(requiredLabelGitHubScopes)
			continue
		}
		switch workflow.Config.Tracker.GitHubStatusSource {
		case workflowconfig.GitHubStatusSourceProjectV2:
			add(requiredProjectV2GitHubScopes)
		case workflowconfig.GitHubStatusSourceIssueField:
			add(requiredIssueFieldGitHubScopes)
		case workflowconfig.GitHubStatusSourceLabel:
			add(requiredLabelGitHubScopes)
		default:
			add(requiredLabelGitHubScopes)
		}
	}
	if len(required) == 0 {
		return append([]string{}, requiredProjectV2GitHubScopes...)
	}
	return required
}

func doctorStringSliceContains(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func missingGitHubScopes(scopes []string, requiredScopes []string) []string {
	have := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope != "" {
			have[scope] = struct{}{}
		}
	}

	var missing []string
	for _, scope := range requiredScopes {
		if !hasEffectiveGitHubScope(have, scope) {
			missing = append(missing, scope)
		}
	}
	return missing
}

func hasEffectiveGitHubScope(scopes map[string]struct{}, scope string) bool {
	if _, ok := scopes[scope]; ok {
		return true
	}
	return scope == "read:project" && hasGitHubProjectScope(scopes)
}

func hasGitHubProjectScope(scopes map[string]struct{}) bool {
	_, ok := scopes["project"]
	return ok
}
