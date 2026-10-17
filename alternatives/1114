package github

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (c *Connector) issueRefsForIDs(ctx context.Context, ids []string, queryType string) (map[string]issueRef, error) {
	refs := make(map[string]issueRef, len(ids))
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if ref, ok := c.projectCache.GetIssueRef(id); ok {
			refs[id] = ref
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return refs, nil
	}

	fetched, err := c.fetchIssueRefsByID(ctx, missing, queryType)
	if err != nil {
		return nil, err
	}
	maps.Copy(refs, fetched)
	return refs, nil
}

func (c *Connector) issueRefForID(ctx context.Context, issueID string, queryType string) (issueRef, bool, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return issueRef{}, false, nil
	}
	if ref, ok := c.projectCache.GetIssueRef(issueID); ok {
		return ref, true, nil
	}
	refs, err := c.fetchIssueRefsByID(ctx, []string{issueID}, queryType)
	if err != nil {
		return issueRef{}, false, err
	}
	ref, ok := refs[issueID]
	return ref, ok, nil
}

func (c *Connector) fetchIssueRefsByID(ctx context.Context, ids []string, queryType string) (map[string]issueRef, error) {
	var response struct {
		Nodes []githubIssueNode `json:"nodes"`
	}
	if err := c.client.GraphQLWithType(ctx, queryType, issueIdentitiesByIDQuery, map[string]any{"issueIds": ids}, &response); err != nil {
		return nil, fmt.Errorf("fetch github issue identities by ids: %w", err)
	}

	refs := make(map[string]issueRef, len(response.Nodes))
	for _, node := range response.Nodes {
		if node.TypeName != "Issue" {
			continue
		}
		ref, ok := issueRefFromNode(node)
		if !ok {
			continue
		}
		refs[node.ID] = ref
		c.projectCache.SetIssueRef(node.ID, ref)
	}
	return refs, nil
}

func normalizedIssueIdentifier(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func connectorIssueKey(issue connector.Issue) string {
	if id := strings.TrimSpace(issue.ID); id != "" {
		return "id:" + id
	}
	if identifier := normalizedIssueIdentifier(issue.Identifier); identifier != "" {
		return "identifier:" + identifier
	}
	return ""
}

func buildIdentifier(repo string, number int) string {
	if number == 0 {
		return ""
	}
	if repo == "" {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func issueRefFromIdentifier(identifier string) (issueRef, bool) {
	repo, number, ok := splitIssueIdentifier(identifier)
	if !ok {
		return issueRef{}, false
	}
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return issueRef{}, false
	}
	return issueRef{Owner: owner, Name: name, Number: number}, true
}

func issueRefFromNode(issue githubIssueNode) (issueRef, bool) {
	owner, name, ok := splitRepositoryName(issue.Repository.NameWithOwner)
	if !ok || issue.Number <= 0 {
		return issueRef{}, false
	}
	return issueRef{Owner: owner, Name: name, Number: issue.Number}, true
}

func issueRefFromRESTSearchItem(issue restIssue, fallback issueRef) (issueRef, bool) {
	if ref, ok := issueRefFromURL(issue.HTMLURL); ok {
		return ref, true
	}
	if issue.Number <= 0 || fallback.Owner == "" || fallback.Name == "" {
		return issueRef{}, false
	}
	return issueRef{Owner: fallback.Owner, Name: fallback.Name, Number: issue.Number}, true
}

func issueRefFromURL(value string) (issueRef, bool) {
	matches := issueURLPattern.FindStringSubmatch(value)
	if len(matches) != 3 {
		return issueRef{}, false
	}
	owner, name, ok := splitRepositoryName(matches[1])
	if !ok {
		return issueRef{}, false
	}
	number, err := strconv.Atoi(matches[2])
	if err != nil || number <= 0 {
		return issueRef{}, false
	}
	return issueRef{Owner: owner, Name: name, Number: number}, true
}

func sameIssueRef(left issueRef, right issueRef) bool {
	return strings.EqualFold(left.Owner, right.Owner) &&
		strings.EqualFold(left.Name, right.Name) &&
		left.Number == right.Number
}

func splitRepositoryName(repo string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner := strings.TrimSpace(parts[0])
	name := strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

func (c *Connector) cacheIssueRef(issue githubIssueNode) {
	ref, ok := issueRefFromNode(issue)
	if !ok {
		return
	}
	c.projectCache.SetIssueRef(issue.ID, ref)
}

func pullRequestRepoFromIdentifier(identifier string) (pullRequestRepo, bool) {
	repo, _, ok := splitIssueIdentifier(identifier)
	if !ok {
		return pullRequestRepo{}, false
	}
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return pullRequestRepo{}, false
	}
	return pullRequestRepo{Owner: owner, Name: name}, true
}

func detentIssueBranchPrefix(identifier string) string {
	_, _, ok := splitIssueIdentifier(identifier)
	if !ok {
		return ""
	}

	key := branchKeyPattern.ReplaceAllString(strings.TrimSpace(identifier), "_")
	key = strings.TrimSpace(key)
	if key == "" || key == "." || key == ".." {
		return ""
	}
	return "detent/" + strings.ToLower(key)
}

func splitIssueIdentifier(identifier string) (string, int, bool) {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 || index == len(identifier)-1 {
		return "", 0, false
	}
	number, err := strconv.Atoi(identifier[index+1:])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	repo := strings.TrimSpace(identifier[:index])
	if repo == "" {
		return "", 0, false
	}
	return repo, number, true
}

func branchMatchesIssuePrefix(branchName string, prefix string) bool {
	branchName = strings.ToLower(strings.TrimSpace(branchName))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if branchName == "" || prefix == "" {
		return false
	}
	if branchName == prefix {
		return true
	}
	for _, suffix := range []string{"_", "-", "/"} {
		if strings.HasPrefix(branchName, prefix+suffix) {
			return true
		}
	}
	if legacyPrefix, ok := strings.CutPrefix(prefix, "detent/"); ok {
		for _, suffix := range []string{"_", "-", "/"} {
			if strings.HasPrefix(branchName, "detent/detent-"+legacyPrefix+suffix) {
				return true
			}
		}
		if branchMatchesCurrentDetentPrefix(branchName, legacyPrefix) {
			return true
		}
		if number := issueNumberFromBranchPrefix(legacyPrefix); number != "" {
			if branchName == "detent/"+number {
				return true
			}
			for _, suffix := range []string{"_", "-", "/"} {
				if strings.HasPrefix(branchName, "detent/"+number+suffix) {
					return true
				}
			}
		}
	}
	return false
}

func branchMatchesCurrentDetentPrefix(branchName string, issueKey string) bool {
	branchStem, ok := strings.CutPrefix(branchName, "detent/")
	if !ok {
		return false
	}
	digestSeparator := strings.LastIndex(branchStem, "-")
	if digestSeparator <= 0 || digestSeparator == len(branchStem)-1 {
		return false
	}
	return strings.HasSuffix(branchStem[:digestSeparator], "-"+issueKey)
}

func issueNumberFromBranchPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	index := strings.LastIndexAny(prefix, "_-")
	if index < 0 || index == len(prefix)-1 {
		return ""
	}
	number := prefix[index+1:]
	for _, r := range number {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return number
}
