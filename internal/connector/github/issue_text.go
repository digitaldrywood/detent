package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (c *Connector) resolveBlockedByProjectState(ctx context.Context, issues []connector.Issue) error {
	byIdentifier := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		identifier := normalizedIssueIdentifier(issue.Identifier)
		if identifier != "" {
			byIdentifier[identifier] = issue
		}
	}

	missing := []string{}
	seenMissing := map[string]struct{}{}
	for issueIndex := range issues {
		for blockerIndex := range issues[issueIndex].BlockedBy {
			identifier := normalizedIssueIdentifier(issues[issueIndex].BlockedBy[blockerIndex].Identifier)
			blocker, ok := byIdentifier[identifier]
			if !ok {
				if identifier == "" || strings.TrimSpace(issues[issueIndex].BlockedBy[blockerIndex].State) != "" {
					continue
				}
				if _, seen := seenMissing[identifier]; seen {
					continue
				}
				seenMissing[identifier] = struct{}{}
				missing = append(missing, issues[issueIndex].BlockedBy[blockerIndex].Identifier)
				continue
			}
			c.applyBlockedByIssueState(&issues[issueIndex].BlockedBy[blockerIndex], blocker)
		}
	}

	resolved := make(map[string]connector.Issue, len(missing))
	for _, identifier := range missing {
		blocker, ok, err := c.fetchIssueByIdentifier(ctx, identifier)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("resolve blocked-by issue %s: %w", identifier, err)
			}
			c.logBlockedByHydrationError(ctx, identifier, err)
			continue
		}
		if !ok {
			continue
		}
		key := normalizedIssueIdentifier(identifier)
		if key != "" {
			resolved[key] = blocker
		}
	}

	for issueIndex := range issues {
		for blockerIndex := range issues[issueIndex].BlockedBy {
			identifier := normalizedIssueIdentifier(issues[issueIndex].BlockedBy[blockerIndex].Identifier)
			blocker, ok := resolved[identifier]
			if !ok {
				continue
			}
			c.applyBlockedByIssueState(&issues[issueIndex].BlockedBy[blockerIndex], blocker)
		}
	}
	return nil
}

func (c *Connector) applyBlockedByIssueState(ref *connector.BlockedRef, blocker connector.Issue) {
	if id := strings.TrimSpace(blocker.ID); id != "" {
		ref.ID = id
	}
	if identifier := strings.TrimSpace(blocker.Identifier); identifier != "" {
		ref.Identifier = identifier
	}
	state := strings.TrimSpace(blocker.State)
	if blocker.Closed && !stateInList(state, c.terminalStates) {
		state = c.closedIssueState()
	}
	ref.State = state
}

func (c *Connector) logBlockedByHydrationError(ctx context.Context, identifier string, err error) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.DebugContext(ctx, "github blocked-by hydration skipped", "identifier", identifier, "error", err)
}

func labelNames(labels nodeConnection[label]) []string {
	names := make([]string, 0, len(labels.Nodes))
	for _, label := range labels.Nodes {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func parseGitHubTime(value *string) *time.Time {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*value))
	if err != nil {
		return nil
	}
	return &parsed
}

func cloneGitHubTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func parseModelOverride(body string) string {
	matches := modelOverridePattern.FindStringSubmatch(body)
	if len(matches) != 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func parseBlockerReason(issue githubIssueNode) string {
	return workpad.Reason(parseWorkpadSignal(issue))
}

func parseWorkpadSignal(issue githubIssueNode) *workpad.Signal {
	repo := strings.TrimSpace(issue.Repository.NameWithOwner)
	for index := len(issue.Comments.Nodes) - 1; index >= 0; index-- {
		comment := issue.Comments.Nodes[index]
		body := comment.Body
		if !workpadCommentBody(body) {
			continue
		}
		if signal, ok := workpad.SignalFromComment(body, comment.URL, repo); ok {
			signal.RecordedAt = parseWorkpadRecordedAt(comment.CreatedAt, comment.UpdatedAt)
			return signal
		}
		if reason := markdownSectionText(body, "Human Action Needed"); reason != "" {
			return &workpad.Signal{
				Source:      workpad.SourceProseSection,
				CommentURL:  strings.TrimSpace(comment.URL),
				HumanAction: reason,
				RecordedAt:  parseWorkpadRecordedAt(comment.CreatedAt, comment.UpdatedAt),
			}
		}
		return nil
	}
	if reason := markdownSectionText(issue.Body, "Human Action Needed"); reason != "" {
		return &workpad.Signal{
			Source:      workpad.SourceProseSection,
			HumanAction: reason,
			RecordedAt:  parseWorkpadRecordedAt(issue.CreatedAt, issue.UpdatedAt),
		}
	}
	return nil
}

func parseWorkpadRecordedAt(createdAt *string, updatedAt *string) *time.Time {
	if parsed := parseGitHubTime(updatedAt); parsed != nil {
		return parsed
	}
	return parseGitHubTime(createdAt)
}

func workpadCommentBody(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		heading, ok := markdownHeadingTitle(line)
		if ok && normalizeSectionTitle(heading) == "codex workpad" {
			return true
		}
	}
	return false
}

func parseBlockedByFromIssueText(issue githubIssueNode, repo string) []connector.BlockedRef {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		repo = strings.TrimSpace(issue.Repository.NameWithOwner)
	}
	self := normalizedIssueIdentifier(buildIdentifier(repo, issue.Number))
	blockers := []connector.BlockedRef{}
	seen := map[string]struct{}{}
	appendBlockers := func(refs []connector.BlockedRef) {
		for _, ref := range refs {
			key := normalizedIssueIdentifier(ref.Identifier)
			if key == "" {
				continue
			}
			if self != "" && key == self {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			blockers = append(blockers, ref)
		}
	}
	appendBlockers(parseBlockedBy(issue.Body, repo))
	for _, comment := range issue.Comments.Nodes {
		appendBlockers(parseBlockedBy(comment.Body, repo))
	}
	return blockers
}

func markdownSectionText(body string, title string) string {
	want := normalizeSectionTitle(title)
	inSection := false
	lines := []string{}
	for line := range strings.SplitSeq(body, "\n") {
		heading, ok := markdownHeadingTitle(line)
		if ok {
			if inSection {
				break
			}
			inSection = normalizeSectionTitle(heading) == want
			continue
		}
		if inSection {
			lines = append(lines, line)
		}
	}
	return normalizeSectionLines(lines)
}

func markdownHeadingTitle(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '#' {
		return "", false
	}
	index := 0
	for index < len(line) && line[index] == '#' {
		index++
	}
	if index > 6 || index == len(line) {
		return "", false
	}
	if line[index] != ' ' && line[index] != '\t' {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(line[index:]), "# \t"), true
}

func normalizeSectionTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

func normalizeSectionLines(lines []string) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = normalizeSectionLine(line)
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}

func normalizeSectionLine(line string) string {
	line = strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if after, ok := strings.CutPrefix(line, marker); ok {
			line = strings.TrimSpace(after)
			break
		}
	}
	line = numberedListPattern.ReplaceAllString(line, "")
	return strings.Join(strings.Fields(line), " ")
}

func parseBlockedBy(body string, repo string) []connector.BlockedRef {
	repo = strings.TrimSpace(repo)
	seen := map[string]struct{}{}
	blockers := []connector.BlockedRef{}

	for _, line := range strings.FieldsFunc(body, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		text, ok := dependencyline.Match(line)
		if !ok {
			continue
		}
		for _, identifier := range issueReferencesInText(text, repo) {
			key := normalizedIssueIdentifier(identifier)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			blockers = append(blockers, connector.BlockedRef{
				Identifier: identifier,
				Source:     connector.BlockedRefSourceProse,
			})
		}
	}
	return blockers
}

func bodyReferencesIssue(body string, repo string, identifier string) bool {
	want := normalizedIssueIdentifier(identifier)
	if want == "" {
		return false
	}
	for _, candidate := range issueReferencesInText(body, repo) {
		if normalizedIssueIdentifier(candidate) == want {
			return true
		}
	}
	return false
}

func issueReferencesInText(text string, repo string) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	add := func(refRepo string, number string) {
		identifier := blockerIdentifier(refRepo, number, repo)
		if identifier == "" {
			return
		}
		key := normalizedIssueIdentifier(identifier)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, identifier)
	}
	for _, matches := range issueURLPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			add(matches[1], matches[2])
		}
	}
	for _, matches := range issueRefPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			add(matches[1], matches[2])
		}
	}
	return refs
}

func githubEpicIssue(issue connector.Issue) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(label), "epic") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(issue.Title)), "epic:")
}

func issueRepo(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 {
		return ""
	}
	return strings.TrimSpace(identifier[:index])
}

func blockerIdentifier(refRepo string, number string, repo string) string {
	if strings.TrimSpace(number) == "" {
		return ""
	}
	refRepo = strings.TrimSpace(refRepo)
	if refRepo == "" {
		if repo == "" {
			return "#" + number
		}
		refRepo = repo
	}
	return refRepo + "#" + number
}

func uniqueNonBlank(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
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
