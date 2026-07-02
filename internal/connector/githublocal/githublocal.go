package githublocal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
)

const (
	MetadataDivergence       = "github_local_divergence"
	MetadataDivergenceDetail = "github_local_divergence_detail"

	DivergenceClosedUpstreamLocalActive = "closed_upstream_local_active"
)

const disabledStatusLabelPrefix = "__detent_local_status_disabled__:"

type Config struct {
	GitHub         githubconnector.Config
	Local          local.Config
	Repository     string
	InitialState   string
	ActiveStates   []string
	TerminalStates []string
	Now            func() time.Time
}

type Connector struct {
	github        *githubconnector.Connector
	local         *local.Connector
	repository    string
	initialState  string
	terminalState map[string]struct{}
	now           func() time.Time
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.Authenticator = (*Connector)(nil)
var _ connector.AuthHealthReporter = (*Connector)(nil)
var _ connector.CandidateIssuesByStatesFetcher = (*Connector)(nil)
var _ connector.Closer = (*Connector)(nil)
var _ connector.RateLimitReporter = (*Connector)(nil)
var _ connector.GraphQLRateLimitUsageReporter = (*Connector)(nil)
var _ connector.InstanceIdentifier = (*Connector)(nil)
var _ connector.IssueChildrenResolver = (*Connector)(nil)
var _ connector.IssueCloser = (*Connector)(nil)
var _ connector.IssueCommentReader = (*Connector)(nil)
var _ connector.IssueFieldClearer = (*Connector)(nil)
var _ connector.IssueFieldSetter = (*Connector)(nil)
var _ connector.IssueParentResolver = (*Connector)(nil)
var _ connector.IssueReferenceResolver = (*Connector)(nil)
var _ connector.IssueStateProber = (*Connector)(nil)
var _ connector.IssuesByStatesLimiter = (*Connector)(nil)
var _ connector.ProjectRemover = (*Connector)(nil)
var _ connector.PullRequestCommenter = (*Connector)(nil)
var _ connector.PullRequestHydrator = (*Connector)(nil)
var _ connector.PullRequestMerger = (*Connector)(nil)
var _ connector.RESTRateLimitUsageReporter = (*Connector)(nil)
var _ connector.StatusDriftReader = (*Connector)(nil)

func New(cfg Config) (*Connector, error) {
	repository := strings.TrimSpace(cfg.Repository)
	if repository == "" {
		repository = strings.TrimSpace(cfg.GitHub.Repository)
	}
	if repository == "" {
		return nil, errors.New("github local repository is required")
	}

	localCfg := cfg.Local
	if len(localCfg.ActiveStates) == 0 {
		localCfg.ActiveStates = cloneStrings(cfg.ActiveStates)
	}
	if len(localCfg.TerminalStates) == 0 {
		localCfg.TerminalStates = cloneStrings(cfg.TerminalStates)
	}
	if localCfg.Now == nil {
		localCfg.Now = cfg.Now
	}
	localConn, err := local.New(localCfg)
	if err != nil {
		return nil, err
	}

	githubCfg := cfg.GitHub
	githubCfg.Repository = repository
	githubCfg.GitHubStatusSource = githubconnector.GitHubStatusSourceLabel
	if strings.TrimSpace(githubCfg.StatusLabelPrefix) == "" {
		githubCfg.StatusLabelPrefix = disabledStatusLabelPrefix
	}
	githubCfg.ActiveStates = cloneStrings(cfg.ActiveStates)
	githubCfg.TerminalStates = cloneStrings(cfg.TerminalStates)
	if githubCfg.Now == nil {
		githubCfg.Now = cfg.Now
	}
	githubConn, err := githubconnector.NewConnector(githubCfg)
	if err != nil {
		return nil, errors.Join(err, localConn.Close())
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Connector{
		github:        githubConn,
		local:         localConn,
		repository:    repository,
		initialState:  defaultInitialState(cfg.InitialState, cfg.ActiveStates),
		terminalState: normalizedStateSet(cfg.TerminalStates),
		now:           now,
	}, nil
}

func (c *Connector) Name() string {
	return connector.BackendGitHubLocal.String()
}

func (c *Connector) Close() error {
	if c == nil {
		return nil
	}
	return errors.Join(closeIfPresent(c.github), closeIfPresent(c.local))
}

func (c *Connector) Authenticate(ctx context.Context) error {
	return c.github.Authenticate(ctx)
}

func (c *Connector) InstanceLogin() string {
	return c.github.InstanceLogin()
}

func (c *Connector) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	return c.github.GraphQLRateLimit()
}

func (c *Connector) AuthHealth() (connector.AuthHealth, bool) {
	return c.github.AuthHealth()
}

func (c *Connector) ResetGraphQLRateLimitUsage() {
	c.github.ResetGraphQLRateLimitUsage()
}

func (c *Connector) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	return c.github.FlushGraphQLRateLimitUsage()
}

func (c *Connector) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	return c.github.FlushRESTRateLimitUsage()
}

func (c *Connector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	issues, err := c.local.FetchCandidateIssues(ctx)
	if err != nil {
		return nil, err
	}
	return c.hydrateLocalIssues(ctx, issues)
}

func (c *Connector) FetchCandidateIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	issues, err := c.local.FetchCandidateIssuesByStates(ctx, states)
	if err != nil {
		return nil, err
	}
	return c.hydrateLocalIssues(ctx, issues)
}

func (c *Connector) FetchIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	issues, err := c.local.FetchIssuesByStates(ctx, states)
	if err != nil {
		return nil, err
	}
	return c.hydrateLocalIssues(ctx, issues)
}

func (c *Connector) FetchIssuesByStatesLimit(ctx context.Context, states []string, limit int) ([]connector.Issue, error) {
	issues, err := c.local.FetchIssuesByStatesLimit(ctx, states, limit)
	if err != nil {
		return nil, err
	}
	return c.hydrateLocalIssues(ctx, issues)
}

func (c *Connector) FetchIssueStateProbe(ctx context.Context, states []string, limit int) ([]connector.Issue, error) {
	return c.FetchIssuesByStatesLimit(ctx, states, limit)
}

func (c *Connector) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) ([]connector.Issue, error) {
	issues, err := c.local.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	return c.hydrateLocalIssues(ctx, issues)
}

func (c *Connector) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) ([]connector.Issue, error) {
	identifiers = uniqueNonBlank(identifiers)
	if len(identifiers) == 0 {
		return []connector.Issue{}, nil
	}

	localIssues, err := c.local.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	localByIdentifier := issuesByIdentifier(localIssues)

	upstream, err := c.github.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	repoInfo, err := c.github.FetchRepositoryInfo(ctx, c.repository)
	if err != nil {
		return nil, err
	}
	out := make([]connector.Issue, 0, len(upstream))
	for _, issue := range upstream {
		issue = c.withGitHubIdentity(issue, repoInfo)
		if localIssue, ok := localByIdentifier[identifierKey(issue.Identifier)]; ok {
			merged := c.mergeLocalWithGitHub(localIssue, issue)
			if err := c.local.UpsertIssues(ctx, []connector.Issue{merged}); err != nil {
				return nil, err
			}
			out = append(out, merged)
			continue
		}
		out = append(out, issue)
	}
	return sortIssuesByIdentifiers(out, identifiers), nil
}

func (c *Connector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	githubComments, err := c.github.FetchIssueComments(ctx, issue)
	if err != nil {
		return nil, err
	}
	localComments, err := c.local.FetchIssueComments(ctx, issue)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return githubComments, nil
		}
		return nil, err
	}
	return append(githubComments, localComments...), nil
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	return c.local.CreateComment(ctx, issueID, body)
}

func (c *Connector) UpdateIssueState(ctx context.Context, issueID string, stateName string) error {
	return c.local.UpdateIssueState(ctx, issueID, stateName)
}

func (c *Connector) SetAssignee(ctx context.Context, issueID string, login string) error {
	return c.local.SetAssignee(ctx, issueID, login)
}

func (c *Connector) SetField(ctx context.Context, issueID string, fieldName string, value string) error {
	return c.local.SetField(ctx, issueID, fieldName, value)
}

func (c *Connector) SetIssueField(ctx context.Context, issueID string, fieldID int, value string) error {
	return c.local.SetIssueField(ctx, issueID, fieldID, value)
}

func (c *Connector) ClearIssueField(ctx context.Context, issueID string, fieldID int) error {
	return c.local.ClearIssueField(ctx, issueID, fieldID)
}

func (c *Connector) CloseIssue(ctx context.Context, issueID string) error {
	return c.local.CloseIssue(ctx, issueID)
}

func (c *Connector) RemoveIssueFromProject(ctx context.Context, issueID string) error {
	return c.local.RemoveIssueFromProject(ctx, issueID)
}

func (c *Connector) CreatePullRequestComment(ctx context.Context, repository string, number int, body string) error {
	return c.github.CreatePullRequestComment(ctx, repository, number, body)
}

func (c *Connector) MergePullRequest(ctx context.Context, repository string, number int, headSHA string) error {
	return c.github.MergePullRequest(ctx, repository, number, headSHA)
}

func (c *Connector) HydratePullRequest(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	return c.github.HydratePullRequest(ctx, issue)
}

func (c *Connector) FetchIssueParents(ctx context.Context, issueID string) ([]connector.Issue, error) {
	githubIssueID, err := c.githubIssueID(ctx, issueID)
	if err != nil {
		return nil, err
	}
	parents, err := c.github.FetchIssueParents(ctx, githubIssueID)
	if err != nil {
		return nil, err
	}
	return c.overlayLocalStatuses(ctx, parents)
}

func (c *Connector) FetchIssueChildren(ctx context.Context, issueID string) ([]connector.BlockedRef, error) {
	githubIssueID, err := c.githubIssueID(ctx, issueID)
	if err != nil {
		return nil, err
	}
	children, err := c.github.FetchIssueChildren(ctx, githubIssueID)
	if err != nil {
		return nil, err
	}
	identifiers := make([]string, 0, len(children))
	for _, child := range children {
		identifiers = append(identifiers, child.Identifier)
	}
	localIssues, err := c.local.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	localByIdentifier := issuesByIdentifier(localIssues)
	for index := range children {
		if issue, ok := localByIdentifier[identifierKey(children[index].Identifier)]; ok {
			children[index].ID = issue.ID
			children[index].State = issue.State
		}
	}
	return children, nil
}

func (c *Connector) FetchStatusDrift(ctx context.Context) (connector.StatusDrift, error) {
	issues, err := c.local.FetchIssueStateProbe(ctx, nil, 0)
	if err != nil {
		return connector.StatusDrift{}, err
	}
	drift := connector.StatusDrift{}
	for _, issue := range issues {
		if strings.TrimSpace(issue.Metadata[MetadataDivergence]) != "" {
			drift.OpenTerminal = append(drift.OpenTerminal, issue)
		}
	}
	return drift, nil
}

func (c *Connector) ImportIssues(ctx context.Context, numbers []int, state string) ([]connector.Issue, error) {
	numbers = uniquePositiveInts(numbers)
	if len(numbers) == 0 {
		return []connector.Issue{}, nil
	}
	state = strings.TrimSpace(state)
	if state == "" {
		state = c.initialState
	}

	repoInfo, err := c.github.FetchRepositoryInfo(ctx, c.repository)
	if err != nil {
		return nil, err
	}
	identifiers := make([]string, 0, len(numbers))
	for _, number := range numbers {
		identifiers = append(identifiers, c.repository+"#"+strconv.Itoa(number))
	}
	upstream, err := c.github.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	upstreamByNumber := map[int]connector.Issue{}
	for _, issue := range upstream {
		if number, ok := issueNumber(issue.Identifier); ok {
			upstreamByNumber[number] = issue
		}
	}

	imported := make([]connector.Issue, 0, len(numbers))
	for _, number := range numbers {
		issue, ok := upstreamByNumber[number]
		if !ok {
			return nil, fmt.Errorf("github local import: issue %s#%d not found", c.repository, number)
		}
		issue = c.withGitHubIdentity(issue, repoInfo)
		issue.ID = localIssueID(repoInfo.ID, number)
		issue.State = state
		now := c.now().UTC()
		issue.StageUpdatedAt = &now
		issue = c.applyDivergence(issue)
		imported = append(imported, issue)
	}
	if err := c.local.UpsertIssues(ctx, imported); err != nil {
		return nil, err
	}
	return imported, nil
}

func (c *Connector) hydrateLocalIssues(ctx context.Context, issues []connector.Issue) ([]connector.Issue, error) {
	if len(issues) == 0 {
		return []connector.Issue{}, nil
	}
	identifiers := make([]string, 0, len(issues))
	for _, issue := range issues {
		identifiers = append(identifiers, issue.Identifier)
	}
	upstream, err := c.github.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	repoInfo, err := c.github.FetchRepositoryInfo(ctx, c.repository)
	if err != nil {
		return nil, err
	}
	upstreamByIdentifier := issuesByIdentifier(upstream)
	out := make([]connector.Issue, 0, len(issues))
	changed := make([]connector.Issue, 0, len(issues))
	for _, localIssue := range issues {
		upstreamIssue, ok := upstreamByIdentifier[identifierKey(localIssue.Identifier)]
		if !ok {
			orphaned := localIssue
			orphaned.Metadata = cloneMetadata(orphaned.Metadata)
			orphaned.Metadata[local.MetadataGitHubOrphaned] = "true"
			changed = append(changed, orphaned)
			out = append(out, orphaned)
			continue
		}
		upstreamIssue = c.withGitHubIdentity(upstreamIssue, repoInfo)
		merged := c.mergeLocalWithGitHub(localIssue, upstreamIssue)
		changed = append(changed, merged)
		out = append(out, merged)
	}
	if len(changed) > 0 {
		if err := c.local.UpsertIssues(ctx, changed); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *Connector) mergeLocalWithGitHub(localIssue connector.Issue, upstream connector.Issue) connector.Issue {
	merged := upstream
	merged.ID = localIssue.ID
	if strings.TrimSpace(localIssue.Identifier) != "" {
		merged.Identifier = localIssue.Identifier
	}
	merged.State = localIssue.State
	merged.Priority = localIssue.Priority
	merged.Fields = cloneMetadata(localIssue.Fields)
	merged.Comments = cloneComments(localIssue.Comments)
	merged.Deliverable = cloneDeliverable(localIssue.Deliverable)
	merged.AssignedToWorker = localIssue.AssignedToWorker
	merged.StageUpdatedAt = localIssue.StageUpdatedAt
	merged.Metadata = mergeMetadata(upstream.Metadata, localIssue.Metadata)
	if strings.TrimSpace(localIssue.ModelOverride) != "" {
		merged.ModelOverride = localIssue.ModelOverride
	}
	return c.applyDivergence(merged)
}

func (c *Connector) withGitHubIdentity(issue connector.Issue, repo githubconnector.RepositoryInfo) connector.Issue {
	metadata := cloneMetadata(issue.Metadata)
	metadata[local.MetadataGitHubNodeID] = strings.TrimSpace(issue.ID)
	metadata[local.MetadataGitHubRepositoryID] = strconv.FormatInt(repo.ID, 10)
	if number, ok := issueNumber(issue.Identifier); ok {
		metadata[local.MetadataGitHubIssueNumber] = strconv.Itoa(number)
	}
	if issue.Closed {
		metadata[local.MetadataGitHubUpstreamState] = "closed"
	} else {
		metadata[local.MetadataGitHubUpstreamState] = "open"
	}
	delete(metadata, local.MetadataGitHubOrphaned)
	issue.Metadata = metadata
	return issue
}

func (c *Connector) applyDivergence(issue connector.Issue) connector.Issue {
	metadata := cloneMetadata(issue.Metadata)
	if issue.Closed && !c.isTerminalState(issue.State) {
		metadata[MetadataDivergence] = DivergenceClosedUpstreamLocalActive
		metadata[MetadataDivergenceDetail] = "closed upstream while locally active"
	} else {
		delete(metadata, MetadataDivergence)
		delete(metadata, MetadataDivergenceDetail)
	}
	issue.Metadata = metadata
	return issue
}

func (c *Connector) overlayLocalStatuses(ctx context.Context, issues []connector.Issue) ([]connector.Issue, error) {
	if len(issues) == 0 {
		return []connector.Issue{}, nil
	}
	identifiers := make([]string, 0, len(issues))
	for _, issue := range issues {
		identifiers = append(identifiers, issue.Identifier)
	}
	localIssues, err := c.local.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	localByIdentifier := issuesByIdentifier(localIssues)
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		if localIssue, ok := localByIdentifier[identifierKey(issue.Identifier)]; ok {
			issue.ID = localIssue.ID
			issue.State = localIssue.State
			issue.Priority = localIssue.Priority
			issue.Fields = cloneMetadata(localIssue.Fields)
			issue.Metadata = mergeMetadata(localIssue.Metadata, issue.Metadata)
		}
		out = append(out, issue)
	}
	return out, nil
}

func (c *Connector) githubIssueID(ctx context.Context, issueID string) (string, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return "", sql.ErrNoRows
	}
	issues, err := c.local.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		return "", err
	}
	if len(issues) == 0 {
		return issueID, nil
	}
	nodeID := strings.TrimSpace(issues[0].Metadata[local.MetadataGitHubNodeID])
	if nodeID == "" {
		return issueID, nil
	}
	return nodeID, nil
}

func (c *Connector) isTerminalState(state string) bool {
	_, ok := c.terminalState[normalizedStateName(state)]
	return ok
}

func closeIfPresent(value connector.Closer) error {
	if value == nil {
		return nil
	}
	return value.Close()
}

func defaultInitialState(configured string, activeStates []string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	for _, state := range activeStates {
		if state = strings.TrimSpace(state); state != "" {
			return state
		}
	}
	return "Todo"
}

func localIssueID(repositoryID int64, number int) string {
	if repositoryID > 0 {
		return "github:" + strconv.FormatInt(repositoryID, 10) + ":" + strconv.Itoa(number)
	}
	return "github:unknown:" + strconv.Itoa(number)
}

func issueNumber(identifier string) (int, bool) {
	_, number, ok := strings.Cut(strings.TrimSpace(identifier), "#")
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(number))
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func issuesByIdentifier(issues []connector.Issue) map[string]connector.Issue {
	out := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		key := identifierKey(issue.Identifier)
		if key != "" {
			out[key] = issue
		}
	}
	return out
}

func identifierKey(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func uniqueNonBlank(values []string) []string {
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
	return out
}

func uniquePositiveInts(values []int) []int {
	seen := map[int]struct{}{}
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizedStateSet(values []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, value := range values {
		if value = normalizedStateName(value); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func normalizedStateName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}

func cloneMetadata(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return maps.Clone(values)
}

func mergeMetadata(primary map[string]string, secondary map[string]string) map[string]string {
	out := cloneMetadata(secondary)
	for key, value := range primary {
		out[key] = value
	}
	return out
}

func cloneComments(values []connector.IssueComment) []connector.IssueComment {
	return append([]connector.IssueComment(nil), values...)
}

func cloneDeliverable(value *connector.Deliverable) *connector.Deliverable {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Metadata = cloneMetadata(value.Metadata)
	return &cloned
}

func sortIssuesByIdentifiers(issues []connector.Issue, identifiers []string) []connector.Issue {
	position := make(map[string]int, len(identifiers))
	for index, identifier := range identifiers {
		position[identifierKey(identifier)] = index
	}
	for left := 0; left < len(issues); left++ {
		for right := left + 1; right < len(issues); right++ {
			if position[identifierKey(issues[right].Identifier)] < position[identifierKey(issues[left].Identifier)] {
				issues[left], issues[right] = issues[right], issues[left]
			}
		}
	}
	return issues
}
