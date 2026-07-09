package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	dependencySourceMerged     = "merged"
	dependencySourceNativeOnly = "native_only"

	nativeDependencyStatusAvailable   = "available"
	nativeDependencyStatusUnavailable = "unavailable"
	nativeDependencyStatusDegraded    = "degraded"
)

func normalizeDependencySource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case dependencySourceNativeOnly:
		return dependencySourceNativeOnly
	default:
		return dependencySourceMerged
	}
}

func (c *Connector) DependencyCapabilities() []connector.DependencyCapability {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	source := c.dependencySource
	caps := make(map[string]nativeDependencyCapability, len(c.dependencyCaps))
	for repo, cap := range c.dependencyCaps {
		caps[repo] = cap
	}
	c.mu.RUnlock()

	repositories := make([]string, 0, len(caps))
	for repo := range caps {
		repositories = append(repositories, repo)
	}
	sort.Strings(repositories)

	out := make([]connector.DependencyCapability, 0, len(repositories))
	for _, repo := range repositories {
		capability := caps[repo]
		out = append(out, connector.DependencyCapability{
			Repository:      repo,
			NativeBlockedBy: capability.Status,
			Source:          source,
			Detail:          capability.Detail,
		})
	}
	return out
}

func (c *Connector) AddIssueBlockedByDependency(ctx context.Context, issueIdentifier string, blockerIdentifier string) error {
	return c.writeIssueBlockedByDependency(ctx, http.MethodPost, issueIdentifier, blockerIdentifier)
}

func (c *Connector) RemoveIssueBlockedByDependency(ctx context.Context, issueIdentifier string, blockerIdentifier string) error {
	return c.writeIssueBlockedByDependency(ctx, http.MethodDelete, issueIdentifier, blockerIdentifier)
}

func (c *Connector) writeIssueBlockedByDependency(ctx context.Context, method string, issueIdentifier string, blockerIdentifier string) error {
	if c == nil {
		return ErrIssueDependencyUpdateFailed
	}
	ref, ok := dependencyIssueRef(issueIdentifier)
	if !ok {
		return ErrIssueDependencyUpdateFailed
	}
	blockerID, ok, err := c.nativeDependencyIssueID(ctx, blockerIdentifier)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIssueDependencyUpdateFailed
	}

	repo := ref.Owner + "/" + ref.Name
	var response restIssue
	var requestBody any
	path := restIssueBlockedByDependenciesPath(ref)
	switch method {
	case http.MethodPost:
		requestBody = map[string]any{"issue_id": blockerID}
	case http.MethodDelete:
		path = restIssueBlockedByDependencyPath(ref, blockerID)
	default:
		return ErrIssueDependencyUpdateFailed
	}
	if err := c.client.REST(ctx, method, path, requestBody, &response); err != nil {
		c.handleNativeDependencyWriteError(ctx, repo, err)
		return fmt.Errorf("update github issue dependency: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return ErrIssueDependencyUpdateFailed
	}
	c.recordNativeDependencyCapability(repo, nativeDependencyStatusAvailable, "")
	return nil
}

func (c *Connector) nativeDependencyIssueID(ctx context.Context, identifier string) (int, bool, error) {
	ref, ok := dependencyIssueRef(identifier)
	if !ok {
		return 0, false, nil
	}
	issue, err := c.fetchRESTIssueRaw(ctx, ref)
	if err != nil {
		return 0, false, err
	}
	if issue.ID <= 0 {
		return 0, false, nil
	}
	return issue.ID, true, nil
}

func dependencyIssueRef(identifier string) (issueRef, bool) {
	if ref, ok := issueRefFromIdentifier(identifier); ok {
		return ref, true
	}
	return issueRefFromURL(identifier)
}

func (c *Connector) hydrateBlockedByRefs(ctx context.Context, issues []connector.Issue) {
	for index := range issues {
		c.hydrateIssueBlockedByRefs(ctx, &issues[index])
	}
}

func (c *Connector) hydrateIssueBlockedByRefs(ctx context.Context, issue *connector.Issue) {
	if c == nil || issue == nil {
		return
	}
	ref, ok := issueRefFromIdentifier(issue.Identifier)
	if !ok {
		ref, ok = issueRefFromURL(issue.URL)
	}
	if !ok {
		markBlockedRefsSource(issue.BlockedBy, connector.BlockedRefSourceProse)
		return
	}

	nativeRefs, nativeAvailable := c.fetchNativeBlockedByRefs(ctx, ref)
	if !nativeAvailable {
		markBlockedRefsSource(issue.BlockedBy, connector.BlockedRefSourceProse)
		issue.BlockedBy = dependencyBlockedRefsWithoutSelf(issue.BlockedBy, issue.Identifier)
		return
	}

	if c.dependencySource == dependencySourceNativeOnly {
		issue.BlockedBy = dependencyBlockedRefsWithoutSelf(nativeRefs, issue.Identifier)
		return
	}

	issue.BlockedBy = mergeGitHubDependencyBlockedRefs(nativeRefs, issue.BlockedBy)
	issue.BlockedBy = dependencyBlockedRefsWithoutSelf(issue.BlockedBy, issue.Identifier)
}

func (c *Connector) fetchNativeBlockedByRefs(ctx context.Context, ref issueRef) ([]connector.BlockedRef, bool) {
	repo := ref.Owner + "/" + ref.Name
	if cap, ok := c.nativeDependencyCapability(repo); ok {
		if cap.Status != nativeDependencyStatusAvailable {
			return nil, false
		}
	}

	refs, err := c.restNativeBlockedByRefs(ctx, ref)
	if err != nil {
		c.handleNativeDependencyFetchError(ctx, repo, err)
		return nil, false
	}
	c.recordNativeDependencyCapability(repo, nativeDependencyStatusAvailable, "")
	return refs, true
}

func (c *Connector) nativeDependencyCapability(repo string) (nativeDependencyCapability, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	capability, ok := c.dependencyCaps[repo]
	return capability, ok
}

func (c *Connector) recordNativeDependencyCapability(repo string, status string, detail string) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = nativeDependencyStatusDegraded
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dependencyCaps == nil {
		c.dependencyCaps = map[string]nativeDependencyCapability{}
	}
	c.dependencyCaps[repo] = nativeDependencyCapability{
		Status: status,
		Detail: strings.TrimSpace(detail),
	}
}

func (c *Connector) restNativeBlockedByRefs(ctx context.Context, ref issueRef) ([]connector.BlockedRef, error) {
	response, err := fetchRESTList[restIssueDependency](ctx, c.client, restIssueBlockedByDependenciesListPath(ref))
	if err != nil {
		return nil, err
	}
	return c.nativeBlockedRefsFromREST(ref, response), nil
}

func (c *Connector) nativeBlockedRefsFromREST(ref issueRef, dependencies []restIssueDependency) []connector.BlockedRef {
	repo := ref.Owner + "/" + ref.Name
	refs := make([]connector.BlockedRef, 0, len(dependencies))
	seen := map[string]struct{}{}
	for _, dependency := range dependencies {
		identifier := buildIdentifier(nativeDependencyRepository(dependency, repo), dependency.Number)
		key := normalizedIssueIdentifier(identifier)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, connector.BlockedRef{
			ID:         strings.TrimSpace(dependency.NodeID),
			Identifier: identifier,
			State:      c.githubIssueStateToDetentState(dependency.State),
			Source:     connector.BlockedRefSourceNative,
		})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func nativeDependencyRepository(dependency restIssueDependency, fallback string) string {
	for _, value := range []string{dependency.RepositoryURL, dependency.HTMLURL, dependency.URL} {
		if repo := repositoryFromGitHubURL(value); repo != "" {
			return repo
		}
	}
	return fallback
}

func repositoryFromGitHubURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index+2 < len(parts); index++ {
		if parts[index] != "repos" {
			continue
		}
		return parts[index+1] + "/" + parts[index+2]
	}
	if len(parts) >= 4 && (parts[2] == "issues" || parts[2] == "pull") {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

func (c *Connector) handleNativeDependencyFetchError(ctx context.Context, repo string, err error) {
	if nativeDependencyRetryableError(err) {
		c.logNativeDependencyFetchError(ctx, repo, err)
		return
	}
	if nativeDependencyUnavailableError(err) {
		c.recordNativeDependencyCapability(repo, nativeDependencyStatusUnavailable, nativeDependencyCapabilityDetail(err))
		return
	}
	c.recordNativeDependencyCapability(repo, nativeDependencyStatusDegraded, err.Error())
	c.logNativeDependencyFetchError(ctx, repo, err)
}

func (c *Connector) handleNativeDependencyWriteError(ctx context.Context, repo string, err error) {
	if nativeDependencyRetryableError(err) {
		c.logNativeDependencyFetchError(ctx, repo, err)
		return
	}
	if nativeDependencyUnavailableError(err) {
		c.recordNativeDependencyCapability(repo, nativeDependencyStatusUnavailable, nativeDependencyCapabilityDetail(err))
		return
	}
	c.recordNativeDependencyCapability(repo, nativeDependencyStatusDegraded, err.Error())
	c.logNativeDependencyFetchError(ctx, repo, err)
}

func nativeDependencyRetryableError(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrRESTBudgetReserved)
}

func nativeDependencyUnavailableError(err error) bool {
	if nativeDependencyRetryableError(err) {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr == nil {
		return false
	}
	return statusErr.StatusCode == http.StatusForbidden || statusErr.StatusCode == http.StatusNotFound
}

func nativeDependencyCapabilityDetail(err error) string {
	var statusErr *StatusError
	if errors.As(err, &statusErr) && statusErr != nil {
		return fmt.Sprintf("status %d", statusErr.StatusCode)
	}
	return err.Error()
}

func (c *Connector) logNativeDependencyFetchError(ctx context.Context, repo string, err error) {
	if c == nil || c.logger == nil || err == nil {
		return
	}
	c.logger.DebugContext(ctx, "github native dependency hydration skipped", "repository", repo, "error", err)
}

func mergeGitHubDependencyBlockedRefs(nativeRefs []connector.BlockedRef, proseRefs []connector.BlockedRef) []connector.BlockedRef {
	if len(nativeRefs) == 0 && len(proseRefs) == 0 {
		return nil
	}
	merged := make([]connector.BlockedRef, 0, len(nativeRefs)+len(proseRefs))
	seen := map[string]struct{}{}
	appendRefs := func(refs []connector.BlockedRef, fallbackSource string) {
		for _, ref := range refs {
			key := normalizedIssueIdentifier(ref.Identifier)
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(ref.ID))
			}
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			if strings.TrimSpace(ref.Source) == "" {
				ref.Source = fallbackSource
			}
			seen[key] = struct{}{}
			merged = append(merged, ref)
		}
	}
	appendRefs(nativeRefs, connector.BlockedRefSourceNative)
	appendRefs(proseRefs, connector.BlockedRefSourceProse)
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func dependencyBlockedRefsWithoutSelf(refs []connector.BlockedRef, identifier string) []connector.BlockedRef {
	self := normalizedIssueIdentifier(identifier)
	if self == "" || len(refs) == 0 {
		return refs
	}
	filtered := refs[:0]
	for _, ref := range refs {
		if normalizedIssueIdentifier(ref.Identifier) == self {
			continue
		}
		filtered = append(filtered, ref)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func markBlockedRefsSource(refs []connector.BlockedRef, source string) {
	for index := range refs {
		if strings.TrimSpace(refs[index].Source) == "" {
			refs[index].Source = source
		}
	}
}
