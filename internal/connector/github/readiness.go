package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ReadinessStatus string

const (
	ReadinessOK   ReadinessStatus = "OK"
	ReadinessWarn ReadinessStatus = "WARN"
	ReadinessFail ReadinessStatus = "FAIL"
)

type ReadinessCheck struct {
	Name   string
	Status ReadinessStatus
	Detail string
	Hint   string
}

type ReadinessConfig struct {
	AuthPath                      string
	WriteProbeIssue               string
	Repositories                  []string
	StatusStates                  []string
	ReadStates                    []string
	RequireIssueCommentsRead      bool
	RequireDependencyMetadataRead bool
	RequireIssueChildrenRead      bool
	RequireIssueParentsRead       bool
	RequirePullRequestRead        bool
	RequirePullRequestReviews     bool
	RequirePullRequestChecks      bool
	RequireProjectStatusWrite     bool
	RequireIssueComments          bool
	RequireAssigneeWrite          bool
	RequireIssueClose             bool
	ProjectFieldWrites            []ReadinessProjectFieldWrite
}

type ReadinessProjectFieldWrite struct {
	Name string
}

type readinessProbeIssue struct {
	ID  string
	Ref issueRef
}

func CheckReadiness(ctx context.Context, cfg Config, readiness ReadinessConfig) ([]ReadinessCheck, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = readinessConnectorConfig(cfg)
	connector, err := NewConnector(cfg)
	if err != nil {
		return nil, err
	}

	checker := githubReadinessChecker{
		connector: connector,
		cfg:       cfg,
	}
	checks := checker.Check(ctx, readiness)
	if err := connector.Close(); err != nil {
		checks = append(checks, ReadinessCheck{
			Name:   "GitHub connector close",
			Status: ReadinessWarn,
			Detail: err.Error(),
			Hint:   "Rerun detent doctor and check local network resources.",
		})
	}
	return checks, nil
}

type githubReadinessChecker struct {
	connector *Connector
	cfg       Config
}

func (c githubReadinessChecker) Check(ctx context.Context, cfg ReadinessConfig) []ReadinessCheck {
	checks := []ReadinessCheck{c.authPathCheck(cfg)}
	if hasGitHubAppCredentials(c.cfg, c.cfg.LookupEnv) {
		checks = append(checks, c.appInstallationCheck(ctx, cfg))
	}
	checks = append(checks,
		c.projectAccessCheck(ctx),
		c.statusOptionsCheck(ctx, cfg.StatusStates),
		c.projectItemsReadCheck(ctx, cfg.ReadStates),
	)
	checks = append(checks, c.repositoryChecks(ctx, cfg.Repositories, cfg)...)

	probe, hasProbe, probeCheck := c.resolveWriteProbe(ctx, cfg)
	if probeCheck != nil {
		checks = append(checks, *probeCheck)
	}
	checks = append(checks, c.probeReadChecks(ctx, cfg, probe, hasProbe)...)
	checks = append(checks, c.writeChecks(ctx, cfg, probe, hasProbe)...)
	return checks
}

func readinessConnectorConfig(cfg Config) Config {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = NewPooledHTTPClient(cfg.HTTPTransport)
	}
	cfg.HTTPClient = httpClient
	if cfg.TokenSource == nil {
		cfg.TokenSource = NewTokenResolver(TokenResolverConfig{
			Endpoint:                cfg.Endpoint,
			APIKey:                  cfg.APIKey,
			GitHubAppID:             cfg.GitHubAppID,
			GitHubAppPrivateKey:     cfg.GitHubAppPrivateKey,
			GitHubAppPrivateKeyPath: cfg.GitHubAppPrivateKeyPath,
			GitHubAppInstallationID: cfg.GitHubAppInstallationID,
			HTTPClient:              httpClient,
			Now:                     cfg.Now,
			LookupEnv:               cfg.LookupEnv,
			GHToken:                 cfg.GHToken,
		})
	}
	return cfg
}

func (c githubReadinessChecker) authPathCheck(cfg ReadinessConfig) ReadinessCheck {
	authPath := strings.TrimSpace(cfg.AuthPath)
	if authPath == "" {
		authPath = "GitHub token"
	}
	return ReadinessCheck{
		Name:   "GitHub auth path",
		Status: ReadinessOK,
		Detail: authPath,
	}
}

func (c githubReadinessChecker) appInstallationCheck(ctx context.Context, cfg ReadinessConfig) ReadinessCheck {
	source, err := NewInstallationTokenSource(InstallationTokenConfig{
		Endpoint:       c.cfg.Endpoint,
		AppID:          c.cfg.GitHubAppID,
		InstallationID: c.cfg.GitHubAppInstallationID,
		PrivateKey:     c.cfg.GitHubAppPrivateKey,
		PrivateKeyPath: c.cfg.GitHubAppPrivateKeyPath,
		HTTPClient:     c.cfg.HTTPClient,
		Now:            c.cfg.Now,
		LookupEnv:      c.cfg.LookupEnv,
	})
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub App installation",
			Status: ReadinessFail,
			Detail: "installation token source could not be created: " + err.Error(),
			Hint:   "Fix tracker.github_app_id, tracker.github_app_installation_id, and private key configuration.",
		}
	}
	details, err := source.TokenDetails(ctx)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub App installation",
			Status: ReadinessFail,
			Detail: "installation permissions could not be read: " + err.Error(),
			Hint:   "Ensure the GitHub App credentials can create an installation token.",
		}
	}
	missing := missingInstallationPermissions(details, cfg)
	missing = append(missing, missingInstallationRepositories(details, cfg.Repositories)...)
	if len(missing) > 0 {
		return ReadinessCheck{
			Name:   "GitHub App installation",
			Status: ReadinessFail,
			Detail: "installation lacks " + strings.Join(missing, ", "),
			Hint:   "Grant the GitHub App installation the listed repository and permission access, then rerun detent doctor.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub App installation",
		Status: ReadinessOK,
		Detail: installationPermissionDetail(details),
	}
}

func (c githubReadinessChecker) projectAccessCheck(ctx context.Context) ReadinessCheck {
	if err := c.connector.Authenticate(ctx); err != nil {
		return ReadinessCheck{
			Name:   "GitHub project access",
			Status: ReadinessFail,
			Detail: fmt.Sprintf("cannot authenticate against ProjectV2 %s: %v", c.connector.projectID, err),
			Hint:   "Grant the token or GitHub App access to the configured ProjectV2 board.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub project access",
		Status: ReadinessOK,
		Detail: "viewer can read ProjectV2 " + c.connector.projectID,
	}
}

func (c githubReadinessChecker) statusOptionsCheck(ctx context.Context, states []string) ReadinessCheck {
	states = uniqueNonBlank(states)
	if len(states) == 0 {
		return ReadinessCheck{
			Name:   "GitHub project Status mappings",
			Status: ReadinessOK,
			Detail: "skipped because no tracker states are configured",
		}
	}
	if err := c.connector.VerifyStatusOptions(ctx, states); err != nil {
		return ReadinessCheck{
			Name:   "GitHub project Status mappings",
			Status: ReadinessFail,
			Detail: fmt.Sprintf("cannot resolve required Status options on project %s: %v", c.connector.projectID, err),
			Hint:   "Add the missing ProjectV2 Status options or fix tracker.state_map.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub project Status mappings",
		Status: ReadinessOK,
		Detail: fmt.Sprintf("resolved %d configured Status option(s) on project %s", len(states), c.connector.projectID),
	}
}

func (c githubReadinessChecker) projectItemsReadCheck(ctx context.Context, states []string) ReadinessCheck {
	states = uniqueNonBlank(states)
	if len(states) == 0 {
		return ReadinessCheck{
			Name:   "GitHub project item read",
			Status: ReadinessOK,
			Detail: "skipped because no active or observed tracker states are configured",
		}
	}
	count, err := c.readProjectItemsSample(ctx, states)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub project item read",
			Status: ReadinessFail,
			Detail: fmt.Sprintf("cannot read ProjectV2 items for configured states on project %s: %v", c.connector.projectID, err),
			Hint:   "Grant ProjectV2 read access and repository issue/PR read access for the configured project.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub project item read",
		Status: ReadinessOK,
		Detail: fmt.Sprintf("read ProjectV2 items for %d configured state(s); sampled %d issue(s)", len(states), count),
	}
}

func (c githubReadinessChecker) readProjectItemsSample(ctx context.Context, states []string) (int, error) {
	var projectQuery *string
	if query := c.connector.projectStatusQuery(states); query != "" {
		projectQuery = &query
	}
	var response struct {
		Node *struct {
			Items projectItemsConnection `json:"items"`
		} `json:"node"`
	}
	if err := c.connector.client.GraphQLWithType(ctx, graphQLQueryObservedStatus, observedStatusProjectItemsQuery, map[string]any{
		"projectId": c.connector.projectID,
		"first":     1,
		"after":     nil,
		"query":     projectQuery,
	}, &response); err != nil {
		return 0, fmt.Errorf("fetch github project items: %w", err)
	}
	if response.Node == nil {
		return 0, ErrProjectNotFound
	}
	count := 0
	for _, item := range response.Node.Items.Nodes {
		if item.Content != nil && item.Content.TypeName == "Issue" {
			count++
		}
	}
	return count, nil
}

func (c githubReadinessChecker) repositoryChecks(ctx context.Context, repositories []string, cfg ReadinessConfig) []ReadinessCheck {
	repositories = normalizedRepositories(repositories)
	if len(repositories) == 0 {
		return []ReadinessCheck{{
			Name:   "GitHub repository access",
			Status: ReadinessWarn,
			Detail: "no source or target repository could be inferred for repository-level checks",
			Hint:   "Ensure the project source checkout has a GitHub origin remote.",
		}}
	}
	checks := make([]ReadinessCheck, 0, len(repositories)*8)
	for _, repo := range repositories {
		checks = append(checks, c.repositoryReadCheck(ctx, repo))
		checks = append(checks, c.repositoryIssueReadCheck(ctx, repo))
		if cfg.RequireIssueCommentsRead {
			checks = append(checks, c.repositoryIssueCommentsReadCheck(ctx, repo))
		}
		if cfg.RequireDependencyMetadataRead {
			checks = append(checks, c.repositoryDependencyMetadataCheck(ctx, repo))
		}
		if cfg.RequirePullRequestRead {
			checks = append(checks, c.repositoryPullRequestReadCheck(ctx, repo))
		}
		if cfg.RequirePullRequestReviews {
			checks = append(checks, c.repositoryPullRequestReviewsCheck(ctx, repo))
		}
		if cfg.RequirePullRequestChecks {
			checks = append(checks, c.repositoryPullRequestChecksCheck(ctx, repo))
		}
	}
	return checks
}

func (c githubReadinessChecker) repositoryReadCheck(ctx context.Context, repo string) ReadinessCheck {
	var out struct {
		FullName string `json:"full_name"`
	}
	if err := c.connector.client.REST(ctx, http.MethodGet, restRepositoryPath(repo), nil, &out); err != nil {
		return ReadinessCheck{
			Name:   "GitHub repository " + repo + " access",
			Status: ReadinessFail,
			Detail: "cannot read repository metadata: " + err.Error(),
			Hint:   "Grant the token or GitHub App access to " + repo + ".",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub repository " + repo + " access",
		Status: ReadinessOK,
		Detail: "repository metadata is readable",
	}
}

func (c githubReadinessChecker) repositoryIssueReadCheck(ctx context.Context, repo string) ReadinessCheck {
	var out []restIssue
	if err := c.connector.client.REST(ctx, http.MethodGet, restRepositoryIssuesPath(repo), nil, &out); err != nil {
		return ReadinessCheck{
			Name:   "GitHub issue metadata " + repo,
			Status: ReadinessFail,
			Detail: "cannot read repository issues: " + err.Error(),
			Hint:   "Grant Issues read access for " + repo + ".",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub issue metadata " + repo,
		Status: ReadinessOK,
		Detail: "issue metadata endpoint is readable",
	}
}

func (c githubReadinessChecker) repositoryIssueCommentsReadCheck(ctx context.Context, repo string) ReadinessCheck {
	return c.restReadCheck(
		ctx,
		"GitHub issue comments read "+repo,
		restRepositoryIssueCommentsPath(repo),
		"repository issue comments endpoint is readable",
		"Grant Issues read access for "+repo+".",
	)
}

func (c githubReadinessChecker) repositoryDependencyMetadataCheck(ctx context.Context, repo string) ReadinessCheck {
	return c.restReadCheck(
		ctx,
		"GitHub dependency metadata "+repo,
		restRepositoryIssueSearchPath(repo),
		"issue search and dependency metadata endpoint is readable",
		"Grant repository issue search access for "+repo+".",
	)
}

func (c githubReadinessChecker) repositoryPullRequestReadCheck(ctx context.Context, repo string) ReadinessCheck {
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return ReadinessCheck{
			Name:   "GitHub pull request metadata " + repo,
			Status: ReadinessFail,
			Detail: "repository name is invalid",
			Hint:   "Use owner/name repository identifiers.",
		}
	}
	var out []restPullRequest
	if err := c.connector.client.REST(ctx, http.MethodGet, restPullRequestsPath(pullRequestRepo{Owner: owner, Name: name}, 1), nil, &out); err != nil {
		return ReadinessCheck{
			Name:   "GitHub pull request metadata " + repo,
			Status: ReadinessFail,
			Detail: "cannot read repository pull requests: " + err.Error(),
			Hint:   "Grant Pull requests read access for " + repo + ".",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub pull request metadata " + repo,
		Status: ReadinessOK,
		Detail: "pull request metadata endpoint is readable",
	}
}

func (c githubReadinessChecker) repositoryPullRequestReviewsCheck(ctx context.Context, repo string) ReadinessCheck {
	pr, ok, check := c.repositoryPullRequestSample(ctx, repo, "GitHub pull request reviews "+repo)
	if check != nil {
		return *check
	}
	if !ok {
		return ReadinessCheck{
			Name:   "GitHub pull request reviews " + repo,
			Status: ReadinessWarn,
			Detail: "no pull request was available to prove review metadata access",
			Hint:   "Create or keep one scratch pull request if strict PR review readiness proof is required.",
		}
	}
	return c.restReadCheck(
		ctx,
		"GitHub pull request reviews "+repo,
		restPullRequestReviewsPath(pr.Repo, pr.Number),
		"pull request reviews endpoint is readable",
		"Grant Pull requests read access for "+repo+".",
	)
}

func (c githubReadinessChecker) repositoryPullRequestChecksCheck(ctx context.Context, repo string) ReadinessCheck {
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return ReadinessCheck{
			Name:   "GitHub CI state " + repo,
			Status: ReadinessFail,
			Detail: "repository name is invalid",
			Hint:   "Use owner/name repository identifiers.",
		}
	}
	defaultBranch, err := c.repositoryDefaultBranch(ctx, repo)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub CI state " + repo,
			Status: ReadinessFail,
			Detail: "cannot resolve repository default branch: " + err.Error(),
			Hint:   "Grant repository metadata read access for " + repo + ".",
		}
	}
	if defaultBranch == "" {
		return ReadinessCheck{
			Name:   "GitHub CI state " + repo,
			Status: ReadinessWarn,
			Detail: "repository default branch is blank; check-run and status endpoints were not probed",
			Hint:   "Ensure " + repo + " has a default branch or validate CI state access with a scratch pull request.",
		}
	}
	repoRef := pullRequestRepo{Owner: owner, Name: name}
	checkRuns := c.restReadCheck(
		ctx,
		"GitHub check runs "+repo,
		restCommitCheckRunsPath(repoRef, defaultBranch),
		"check runs endpoint is readable",
		"Grant Checks read access for "+repo+".",
	)
	if checkRuns.Status != ReadinessOK {
		return checkRuns
	}
	statuses := c.restReadCheck(
		ctx,
		"GitHub commit statuses "+repo,
		restCommitStatusesPath(repoRef, defaultBranch),
		"commit statuses endpoint is readable",
		"Grant commit status read access for "+repo+".",
	)
	if statuses.Status != ReadinessOK {
		return statuses
	}
	return ReadinessCheck{
		Name:   "GitHub CI state " + repo,
		Status: ReadinessOK,
		Detail: "check runs and commit statuses endpoints are readable",
	}
}

type readinessPullRequestSample struct {
	Repo   pullRequestRepo
	Number int
}

func (c githubReadinessChecker) repositoryPullRequestSample(ctx context.Context, repo string, name string) (readinessPullRequestSample, bool, *ReadinessCheck) {
	owner, repoName, ok := splitRepositoryName(repo)
	if !ok {
		check := ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "repository name is invalid",
			Hint:   "Use owner/name repository identifiers.",
		}
		return readinessPullRequestSample{}, false, &check
	}
	repoRef := pullRequestRepo{Owner: owner, Name: repoName}
	var out []restPullRequest
	if err := c.connector.client.REST(ctx, http.MethodGet, restPullRequestsPath(repoRef, 1), nil, &out); err != nil {
		check := ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "cannot read repository pull requests: " + err.Error(),
			Hint:   "Grant Pull requests read access for " + repo + ".",
		}
		return readinessPullRequestSample{}, false, &check
	}
	for _, pr := range out {
		if pr.Number > 0 {
			return readinessPullRequestSample{Repo: repoRef, Number: pr.Number}, true, nil
		}
	}
	return readinessPullRequestSample{}, false, nil
}

func (c githubReadinessChecker) repositoryDefaultBranch(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.connector.client.REST(ctx, http.MethodGet, restRepositoryPath(repo), nil, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.DefaultBranch), nil
}

func (c githubReadinessChecker) restReadCheck(ctx context.Context, name string, path string, okDetail string, hint string) ReadinessCheck {
	result, err := c.connector.client.restProbe(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "read probe failed: " + err.Error(),
			Hint:   "Check GitHub credentials and network access, then rerun detent doctor.",
		}
	}
	detailSuffix := acceptedPermissionsDetail(result.Headers)
	if result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessOK,
			Detail: okDetail + detailSuffix,
		}
	}
	if result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden || result.StatusCode == http.StatusNotFound {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: fmt.Sprintf("token cannot read endpoint: HTTP %d%s %s", result.StatusCode, detailSuffix, strings.TrimSpace(result.Body)),
			Hint:   hint,
		}
	}
	return ReadinessCheck{
		Name:   name,
		Status: ReadinessFail,
		Detail: fmt.Sprintf("read probe returned HTTP %d%s %s", result.StatusCode, detailSuffix, strings.TrimSpace(result.Body)),
		Hint:   "Check GitHub endpoint availability and repository access.",
	}
}

func (c githubReadinessChecker) resolveWriteProbe(ctx context.Context, cfg ReadinessConfig) (readinessProbeIssue, bool, *ReadinessCheck) {
	if !cfg.requiresProbeIssue() {
		return readinessProbeIssue{}, false, nil
	}
	probeID := strings.TrimSpace(cfg.WriteProbeIssue)
	if probeID == "" {
		return readinessProbeIssue{}, false, nil
	}
	probe, err := c.resolveProbeIssue(ctx, probeID)
	if err != nil {
		check := ReadinessCheck{
			Name:   "GitHub write probe target",
			Status: ReadinessFail,
			Detail: probeID + ": " + err.Error(),
			Hint:   "Set tracker.write_probe_issue to a scratch issue that belongs to the configured ProjectV2 board.",
		}
		return readinessProbeIssue{}, false, &check
	}
	check := ReadinessCheck{
		Name:   "GitHub write probe target",
		Status: ReadinessOK,
		Detail: fmt.Sprintf("%s resolves to %s#%d and node %s", probeID, probe.Ref.Owner+"/"+probe.Ref.Name, probe.Ref.Number, probe.ID),
	}
	return probe, true, &check
}

func (cfg ReadinessConfig) requiresProbeIssue() bool {
	return cfg.requiresWriteProbe() ||
		cfg.RequireIssueChildrenRead ||
		cfg.RequireIssueParentsRead
}

func (cfg ReadinessConfig) requiresWriteProbe() bool {
	return cfg.RequireProjectStatusWrite ||
		cfg.RequireIssueComments ||
		cfg.RequireAssigneeWrite ||
		cfg.RequireIssueClose ||
		len(cfg.ProjectFieldWrites) > 0
}

func (c githubReadinessChecker) resolveProbeIssue(ctx context.Context, value string) (readinessProbeIssue, error) {
	if ref, ok := issueRefFromIdentifier(value); ok {
		issue, err := c.connector.fetchRESTIssue(ctx, ref)
		if err != nil {
			return readinessProbeIssue{}, err
		}
		if strings.TrimSpace(issue.ID) == "" {
			return readinessProbeIssue{}, ErrNotFound
		}
		c.connector.cacheIssueRef(issue)
		return readinessProbeIssue{ID: strings.TrimSpace(issue.ID), Ref: ref}, nil
	}
	ref, ok, err := c.connector.issueRefForID(ctx, value, graphQLQueryIssueLookup)
	if err != nil {
		return readinessProbeIssue{}, err
	}
	if !ok {
		return readinessProbeIssue{}, ErrNotFound
	}
	issue, err := c.connector.fetchRESTIssue(ctx, ref)
	if err != nil {
		return readinessProbeIssue{}, err
	}
	if strings.TrimSpace(issue.ID) == "" {
		return readinessProbeIssue{}, ErrNotFound
	}
	return readinessProbeIssue{ID: strings.TrimSpace(issue.ID), Ref: ref}, nil
}

func (c githubReadinessChecker) probeReadChecks(ctx context.Context, cfg ReadinessConfig, probe readinessProbeIssue, hasProbe bool) []ReadinessCheck {
	checks := []ReadinessCheck{}
	if cfg.RequireIssueChildrenRead {
		checks = append(checks, c.issueChildrenReadCheck(ctx, probe, hasProbe))
	}
	if cfg.RequireIssueParentsRead {
		checks = append(checks, c.issueParentsReadCheck(ctx, probe, hasProbe))
	}
	return checks
}

func (c githubReadinessChecker) issueChildrenReadCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool) ReadinessCheck {
	if !hasProbe {
		return unprovenProbeIssueCheck("GitHub issue children metadata")
	}
	children, err := c.connector.FetchIssueChildren(ctx, probe.ID)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub issue children metadata",
			Status: ReadinessFail,
			Detail: "cannot read issue sub-issue/tracked-issue metadata: " + err.Error(),
			Hint:   "Grant GraphQL issue relationship read access for the probe issue repository and project.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub issue children metadata",
		Status: ReadinessOK,
		Detail: fmt.Sprintf("queried sub-issue and tracked-issue metadata for probe issue; found %d child link(s)", len(children)),
	}
}

func (c githubReadinessChecker) issueParentsReadCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool) ReadinessCheck {
	if !hasProbe {
		return unprovenProbeIssueCheck("GitHub issue parent metadata")
	}
	parents, err := c.connector.FetchIssueParents(ctx, probe.ID)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub issue parent metadata",
			Status: ReadinessFail,
			Detail: "cannot read issue parent/tracked-in metadata: " + err.Error(),
			Hint:   "Grant GraphQL issue relationship and repository search access for the probe issue repository and project.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub issue parent metadata",
		Status: ReadinessOK,
		Detail: fmt.Sprintf("queried parent and tracked-in metadata for probe issue; found %d parent link(s)", len(parents)),
	}
}

func (c githubReadinessChecker) writeChecks(ctx context.Context, cfg ReadinessConfig, probe readinessProbeIssue, hasProbe bool) []ReadinessCheck {
	checks := []ReadinessCheck{}
	if cfg.RequireProjectStatusWrite {
		checks = append(checks, c.projectStatusWriteCheck(ctx, probe, hasProbe))
	}
	if cfg.RequireIssueComments {
		checks = append(checks, c.issueCommentWriteCheck(ctx, probe, hasProbe))
	}
	if cfg.RequireAssigneeWrite {
		checks = append(checks, c.assigneeWriteCheck(ctx, probe, hasProbe))
	}
	for _, field := range cfg.ProjectFieldWrites {
		checks = append(checks, c.projectFieldWriteCheck(ctx, probe, hasProbe, field))
	}
	if cfg.RequireIssueClose {
		checks = append(checks, c.issueCloseWriteCheck(ctx, probe, hasProbe))
	}
	return checks
}

func (c githubReadinessChecker) projectStatusWriteCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool) ReadinessCheck {
	if !hasProbe {
		return unprovenWriteCheck("GitHub project status update")
	}
	item, err := c.connector.resolveProjectItem(ctx, probe.ID)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub project status update",
			Status: ReadinessFail,
			Detail: fmt.Sprintf("token cannot resolve ProjectV2 item for probe issue on project %s: %v", c.connector.projectID, err),
			Hint:   "Use a scratch issue that is present on the configured ProjectV2 board.",
		}
	}
	statusName := strings.TrimSpace(item.StatusName)
	if statusName == "" {
		return ReadinessCheck{
			Name:   "GitHub project status update",
			Status: ReadinessWarn,
			Detail: "write probe issue has no current Status value; updateProjectV2ItemFieldValue was not executed",
			Hint:   "Set the scratch issue Status field, then rerun detent doctor.",
		}
	}
	fieldID, optionID, err := c.connector.resolveStatusOption(ctx, statusName)
	if err != nil {
		return ReadinessCheck{
			Name:   "GitHub project status update",
			Status: ReadinessFail,
			Detail: fmt.Sprintf("token cannot resolve current Status option %q for project %s: %v", statusName, c.connector.projectID, err),
			Hint:   "Fix ProjectV2 Status options or tracker.state_map.",
		}
	}
	if err := c.connector.updateStatusFieldValue(ctx, item.ID, fieldID, optionID); err != nil {
		return ReadinessCheck{
			Name:   "GitHub project status update",
			Status: ReadinessFail,
			Detail: fmt.Sprintf("token cannot update ProjectV2 item field value for project %s: %v", c.connector.projectID, err),
			Hint:   "Grant Projects write access to the token or GitHub App installation.",
		}
	}
	return ReadinessCheck{
		Name:   "GitHub project status update",
		Status: ReadinessOK,
		Detail: "reapplied existing Status value on the write probe item",
	}
}

func (c githubReadinessChecker) issueCommentWriteCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool) ReadinessCheck {
	if !hasProbe {
		return unprovenWriteCheck("GitHub issue comments")
	}
	return c.invalidWriteProbe(ctx, "GitHub issue comments", http.MethodPost, restIssueCommentsPath(probe.Ref), map[string]any{"body": ""}, "create issue comments")
}

func (c githubReadinessChecker) assigneeWriteCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool) ReadinessCheck {
	if !hasProbe {
		return unprovenWriteCheck("GitHub assignee update")
	}
	return c.invalidWriteProbe(ctx, "GitHub assignee update", http.MethodPost, restIssueAssigneesPath(probe.Ref), map[string]any{
		"assignees": []string{"detent-doctor-permission-probe-not-a-real-user-" + time.Now().UTC().Format("20060102150405")},
	}, "update issue assignees")
}

func (c githubReadinessChecker) issueCloseWriteCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool) ReadinessCheck {
	if !hasProbe {
		return unprovenWriteCheck("GitHub issue close")
	}
	return c.invalidWriteProbe(ctx, "GitHub issue close", http.MethodPatch, restIssuePath(probe.Ref), map[string]any{
		"state": "detent-doctor-invalid-state",
	}, "close issues")
}

func (c githubReadinessChecker) invalidWriteProbe(ctx context.Context, name string, method string, path string, body any, capability string) ReadinessCheck {
	result, err := c.connector.client.restProbe(ctx, method, path, body)
	if err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: capability + " permission probe failed: " + err.Error(),
			Hint:   "Check GitHub credentials and network access, then rerun detent doctor.",
		}
	}
	detailSuffix := acceptedPermissionsDetail(result.Headers)
	switch result.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessOK,
			Detail: fmt.Sprintf("endpoint accepted auth and rejected the intentionally invalid probe with HTTP %d%s", result.StatusCode, detailSuffix),
		}
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: fmt.Sprintf("token cannot %s: HTTP %d%s %s", capability, result.StatusCode, detailSuffix, strings.TrimSpace(result.Body)),
			Hint:   "Grant the token or GitHub App installation the required repository write permission.",
		}
	default:
		if result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices {
			return ReadinessCheck{
				Name:   name,
				Status: ReadinessWarn,
				Detail: fmt.Sprintf("permission probe unexpectedly returned HTTP %d%s", result.StatusCode, detailSuffix),
				Hint:   "Inspect the scratch issue and rerun detent doctor with a different write probe target if needed.",
			}
		}
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: fmt.Sprintf("permission probe returned HTTP %d%s %s", result.StatusCode, detailSuffix, strings.TrimSpace(result.Body)),
			Hint:   "Grant the required GitHub permission or check GitHub endpoint availability.",
		}
	}
}

func (c githubReadinessChecker) projectFieldWriteCheck(ctx context.Context, probe readinessProbeIssue, hasProbe bool, field ReadinessProjectFieldWrite) ReadinessCheck {
	fieldName := strings.TrimSpace(field.Name)
	name := "GitHub project field update"
	if fieldName != "" {
		name = "GitHub project field " + fieldName + " update"
	}
	if !hasProbe {
		return unprovenWriteCheck(name)
	}
	if fieldName == "" {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessWarn,
			Detail: "skipped because field name is blank",
		}
	}
	item, err := c.connector.resolveProjectItem(ctx, probe.ID)
	if err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: fmt.Sprintf("token cannot resolve ProjectV2 item for probe issue on project %s: %v", c.connector.projectID, err),
			Hint:   "Use a scratch issue that is present on the configured ProjectV2 board.",
		}
	}
	_, _, _, fields, ok, err := c.connector.fetchProjectFieldsPage(ctx, probe.ID, nil)
	if err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "cannot read current project field values for probe issue: " + err.Error(),
			Hint:   "Grant ProjectV2 read access for the configured project.",
		}
	}
	if !ok {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "probe issue is not on the configured ProjectV2 board",
			Hint:   "Use a scratch issue that is present on the configured ProjectV2 board.",
		}
	}
	projectField, err := c.connector.fetchProjectField(ctx, fieldName)
	if err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "cannot resolve project field: " + err.Error(),
			Hint:   "Create the configured ProjectV2 field or update the workflow identity/claims configuration.",
		}
	}
	current, hasCurrent := fields[fieldName]
	if projectTextField(projectField) {
		if err := c.connector.updateProjectV2TextFieldValue(ctx, item.ID, projectField.ID, current, ErrProjectFieldUpdateFailed); err != nil {
			return ReadinessCheck{
				Name:   name,
				Status: ReadinessFail,
				Detail: "token cannot update ProjectV2 text field value: " + err.Error(),
				Hint:   "Grant Projects write access to the token or GitHub App installation.",
			}
		}
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessOK,
			Detail: "reapplied existing text field value on the write probe item",
		}
	}
	if !hasCurrent || strings.TrimSpace(current) == "" {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessWarn,
			Detail: "write probe issue has no current value for this single-select field; field update was not executed",
			Hint:   "Set the field on the scratch issue, then rerun detent doctor.",
		}
	}
	decoded, err := decodeProjectSingleSelectField(fieldName, &projectField)
	if err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "project field is not a supported text or single-select field: " + err.Error(),
			Hint:   "Use a ProjectV2 text or single-select field for Detent ownership/lease fields.",
		}
	}
	optionID := singleSelectOptionID(decoded.Options, current)
	if optionID == "" {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: fmt.Sprintf("current probe field value %q is not an option on %s", current, fieldName),
			Hint:   "Fix the ProjectV2 field options or choose a probe issue with a valid current value.",
		}
	}
	if err := c.connector.updateProjectV2SingleSelectFieldValue(ctx, item.ID, decoded.ID, optionID, ErrProjectFieldUpdateFailed); err != nil {
		return ReadinessCheck{
			Name:   name,
			Status: ReadinessFail,
			Detail: "token cannot update ProjectV2 single-select field value: " + err.Error(),
			Hint:   "Grant Projects write access to the token or GitHub App installation.",
		}
	}
	return ReadinessCheck{
		Name:   name,
		Status: ReadinessOK,
		Detail: "reapplied existing single-select field value on the write probe item",
	}
}

func unprovenProbeIssueCheck(name string) ReadinessCheck {
	return ReadinessCheck{
		Name:   name,
		Status: ReadinessWarn,
		Detail: "no tracker.write_probe_issue configured; issue-specific read capability not proven",
		Hint:   "Configure tracker.write_probe_issue with a scratch issue on the configured project to prove this capability.",
	}
}

func unprovenWriteCheck(name string) ReadinessCheck {
	return ReadinessCheck{
		Name:   name,
		Status: ReadinessWarn,
		Detail: "no tracker.write_probe_issue configured; write capability not proven",
		Hint:   "Configure tracker.write_probe_issue with a scratch issue on the configured project to prove this write capability.",
	}
}

func missingInstallationPermissions(details InstallationTokenDetails, cfg ReadinessConfig) []string {
	var missing []string
	projectLevel := "read"
	if cfg.RequireProjectStatusWrite || len(cfg.ProjectFieldWrites) > 0 {
		projectLevel = "write"
	}
	if !hasAnyPermission(details.Permissions, []string{"organization_projects", "repository_projects", "projects"}, projectLevel) {
		missing = append(missing, "Projects: "+projectLevel)
	}
	issueLevel := "read"
	if cfg.RequireIssueComments || cfg.RequireAssigneeWrite || cfg.RequireIssueClose {
		issueLevel = "write"
	}
	if !permissionAllows(details.Permissions["issues"], issueLevel) {
		missing = append(missing, "Issues: "+issueLevel)
	}
	if cfg.RequirePullRequestRead && !permissionAllows(details.Permissions["pull_requests"], "read") {
		missing = append(missing, "Pull requests: read")
	}
	if cfg.RequirePullRequestChecks && !permissionAllows(details.Permissions["checks"], "read") {
		missing = append(missing, "Checks: read")
	}
	return missing
}

func missingInstallationRepositories(details InstallationTokenDetails, repositories []string) []string {
	if !strings.EqualFold(strings.TrimSpace(details.RepositorySelection), "selected") {
		return nil
	}
	repositories = normalizedRepositories(repositories)
	if len(repositories) == 0 {
		return []string{"selected repository access for configured repositories"}
	}
	available := make(map[string]struct{}, len(details.Repositories))
	for _, repo := range details.Repositories {
		if normalized := normalizeRepository(repo.FullName); normalized != "" {
			available[normalized] = struct{}{}
		}
	}
	var missing []string
	for _, repo := range repositories {
		if _, ok := available[normalizeRepository(repo)]; !ok {
			missing = append(missing, "repository access to "+repo)
		}
	}
	return missing
}

func installationPermissionDetail(details InstallationTokenDetails) string {
	parts := make([]string, 0, len(details.Permissions))
	for key, value := range details.Permissions {
		parts = append(parts, key+"="+value)
	}
	sort.Strings(parts)
	selection := strings.TrimSpace(details.RepositorySelection)
	if selection == "" {
		selection = "unknown"
	}
	if len(parts) == 0 {
		return "installation token created; repository_selection=" + selection
	}
	return "installation token created; repository_selection=" + selection + "; permissions " + strings.Join(parts, ", ")
}

func hasAnyPermission(permissions map[string]string, keys []string, required string) bool {
	for _, key := range keys {
		if permissionAllows(permissions[key], required) {
			return true
		}
	}
	return false
}

func permissionAllows(actual string, required string) bool {
	actualRank := permissionRank(actual)
	requiredRank := permissionRank(required)
	return actualRank >= requiredRank && requiredRank > 0
}

func permissionRank(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	default:
		return 0
	}
}

func hasGitHubAppCredentials(cfg Config, lookupEnv func(string) string) bool {
	if lookupEnv == nil {
		lookupEnv = func(string) string { return "" }
	}
	return resolveSecretValue(cfg.GitHubAppID, lookupEnv) != "" &&
		resolveSecretValue(cfg.GitHubAppInstallationID, lookupEnv) != "" &&
		(resolveSecretValue(cfg.GitHubAppPrivateKey, lookupEnv) != "" ||
			resolveSecretValue(cfg.GitHubAppPrivateKeyPath, lookupEnv) != "")
}

func acceptedPermissionsDetail(headers http.Header) string {
	value := strings.TrimSpace(headers.Get("X-Accepted-GitHub-Permissions"))
	if value == "" {
		return ""
	}
	return "; accepted permissions: " + value
}

func normalizedRepositories(repositories []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(repositories))
	for _, repo := range repositories {
		repo = normalizeRepository(repo)
		if repo == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(repo)]; ok {
			continue
		}
		seen[strings.ToLower(repo)] = struct{}{}
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

func normalizeRepository(repo string) string {
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return ""
	}
	return owner + "/" + name
}

func restRepositoryPath(repo string) string {
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return "/repos/"
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func restRepositoryIssuesPath(repo string) string {
	values := url.Values{}
	values.Set("per_page", "1")
	values.Set("state", "all")
	return restRepositoryPath(repo) + "/issues?" + values.Encode()
}

func restRepositoryIssueCommentsPath(repo string) string {
	values := url.Values{}
	values.Set("per_page", "1")
	return restRepositoryPath(repo) + "/issues/comments?" + values.Encode()
}

func restRepositoryIssueSearchPath(repo string) string {
	values := url.Values{}
	values.Set("q", "repo:"+repo+" is:issue")
	values.Set("per_page", "1")
	return "/search/issues?" + values.Encode()
}
