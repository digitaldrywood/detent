package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
)

func (c *Connector) PrepareHumanQuestionMigration(ctx context.Context, request connector.HumanQuestionMigration) (connector.Issue, error) {
	repository := strings.ToLower(c.repository.Owner + "/" + c.repository.Name)
	source, err := dependencyline.CanonicalReference(request.Source, repository)
	if err != nil || !strings.HasPrefix(source, repository+"#") {
		return connector.Issue{}, errors.New("migration source must be in this repository")
	}
	dependent, err := dependencyline.CanonicalReference(request.Dependent, repository)
	if err != nil || !strings.HasPrefix(dependent, repository+"#") || source == dependent {
		return connector.Issue{}, errors.New("migration dependent must be a different issue in this repository")
	}
	original, found, err := c.fetchIssueByIdentifier(ctx, source)
	if err != nil {
		return connector.Issue{}, err
	}
	if !found {
		return connector.Issue{}, errors.New("migration source not found")
	}
	originalBody, _, _ := strings.Cut(original.Description, "\n\n<!-- detent-question-retired -->\n")
	sum := sha256.Sum256([]byte(originalBody))
	task, marked, err := connector.ParseHumanTask(original.Description)
	if err != nil || !marked || task.CompletionEvidence != "" || hex.EncodeToString(sum[:]) != request.SourceBodySHA256 || strings.TrimSpace(request.Question) == "" {
		return connector.Issue{}, errors.New("migration requires an explicitly selected unchanged unresolved human question and readable replacement question")
	}
	issue, found, err := c.fetchIssueByIdentifier(ctx, dependent)
	if err != nil {
		return connector.Issue{}, err
	}
	if !found || connector.NonExecutableReason(issue) != "" {
		return connector.Issue{}, errors.New("migration dependent must be software work")
	}
	refs, err := dependencyline.References(issue.Description, repository)
	if err != nil {
		return connector.Issue{}, err
	}
	ref, _ := dependencyIssueRef(dependent)
	native, err := c.restNativeBlockedByRefs(ctx, ref)
	if err != nil {
		return connector.Issue{}, err
	}
	comments, err := c.FetchIssueComments(ctx, issue)
	if err != nil {
		return connector.Issue{}, err
	}
	migrated := slices.ContainsFunc(comments, func(comment connector.IssueComment) bool {
		return strings.Contains(comment.Body, migrationAuditMarker(source))
	})
	if !slices.Contains(refs, source) && !slices.ContainsFunc(native, func(ref connector.BlockedRef) bool { return strings.EqualFold(ref.Identifier, source) }) && !migrated && !request.PreviouslyGeneratedQuestion {
		return connector.Issue{}, errors.New("selected question is not a dependency of the original issue")
	}
	return issue, nil
}

func migrationAuditMarker(source string) string {
	return "<!-- detent-question-migration:" + strings.ToLower(source) + " -->"
}

func (c *Connector) RetireHumanQuestion(ctx context.Context, request connector.HumanQuestionMigration, questionCommentID string) error {
	c.prerequisiteMu.Lock()
	defer c.prerequisiteMu.Unlock()
	if questionCommentID == "" {
		return errors.New("persist the replacement question comment before removing its dependency")
	}
	issue, err := c.PrepareHumanQuestionMigration(ctx, request)
	if err != nil {
		return err
	}
	repository := c.repository.Owner + "/" + c.repository.Name
	source, err := dependencyline.CanonicalReference(request.Source, repository)
	if err != nil {
		return err
	}
	original, found, err := c.fetchIssueByIdentifier(ctx, source)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("migration source disappeared")
	}
	comments, err := c.FetchIssueComments(ctx, issue)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(comments, func(comment connector.IssueComment) bool {
		return comment.ID == questionCommentID && strings.Contains(comment.Body, migrationAuditMarker(source))
	}) {
		return errors.New("replacement question comment is not present on the original issue")
	}
	for _, comment := range comments {
		updated, err := removeQuestionWorkpadBlocker(comment.Body, repository, source)
		if err != nil {
			return err
		}
		if updated != comment.Body {
			var result struct {
				UpdateIssueComment struct {
					IssueComment struct {
						ID string `json:"id"`
					} `json:"issueComment"`
				} `json:"updateIssueComment"`
			}
			if err := c.client.GraphQL(ctx, `mutation($id:ID!,$body:String!){updateIssueComment(input:{id:$id,body:$body}){issueComment{id}}}`, map[string]any{"id": comment.ID, "body": updated}, &result); err != nil {
				return err
			}
		}
	}
	body, err := removeQuestionDependency(issue.Description, repository, source)
	if err != nil {
		return err
	}
	if body != issue.Description {
		if err := c.UpdateIssueBody(ctx, issue.ID, body); err != nil {
			return err
		}
	}
	ref, _ := dependencyIssueRef(issue.Identifier)
	native, err := c.restNativeBlockedByRefs(ctx, ref)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(native, func(ref connector.BlockedRef) bool { return strings.EqualFold(ref.Identifier, source) }) {
		if err := c.RemoveIssueBlockedByDependency(ctx, issue.Identifier, source); err != nil {
			return err
		}
	}
	all, err := fetchRESTList[restIssue](ctx, c.client, restRepositoryIssueCreatePath(c.repository)+"?state=all&per_page=100")
	if err != nil {
		return err
	}
	for _, raw := range all {
		if raw.PullRequest != nil || raw.Body == nil {
			continue
		}
		refs, err := dependencyline.References(*raw.Body, repository)
		if err != nil {
			return err
		}
		if slices.Contains(refs, source) {
			return nil
		}
	}
	sourceRef, _ := dependencyIssueRef(source)
	remaining, err := fetchRESTList[restIssue](ctx, c.client, restIssuePath(sourceRef)+"/dependencies/blocking?per_page=100")
	if err != nil {
		return err
	}
	if len(remaining) > 0 || original.Closed {
		return nil
	}
	explanation := "Superseded by the question on " + issue.URL + ". The human decision remains unresolved; answer in the original issue thread. No approval, completion evidence, or external-action permission is inferred. Original context and history are retained."
	var response restIssue
	if err := c.client.REST(ctx, http.MethodPatch, restIssuePath(sourceRef), map[string]any{"body": original.Description + "\n\n<!-- detent-question-retired -->\n" + explanation, "state": "closed", "state_reason": "not_planned"}, &response); err != nil {
		return fmt.Errorf("retire generated question: %w", err)
	}
	if response.NodeID == "" || response.State != "closed" {
		return errors.New("question retirement was not confirmed")
	}
	return nil
}

func removeQuestionDependency(body, repository, source string) (string, error) {
	var out []string
	fence := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if fence == "" {
				fence = trimmed[:3]
			} else if trimmed == fence {
				fence = ""
			}
			out = append(out, line)
			continue
		}
		if fence == "" {
			if _, matched := dependencyline.Match(line); matched {
				refs, err := dependencyline.References(line, repository)
				if err != nil {
					return "", err
				}
				if slices.Contains(refs, source) {
					refs = slices.DeleteFunc(refs, func(ref string) bool { return ref == source })
					if len(refs) > 0 {
						out = append(out, "Depends on: "+strings.Join(refs, ", "))
					}
					continue
				}
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), nil
}

func removeQuestionWorkpadBlocker(body, repository, source string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(body), "## Codex Workpad") {
		return body, nil
	}
	before, rest, found := strings.Cut(body, "```detent-status\n")
	if !found {
		return body, nil
	}
	content, after, found := strings.Cut(rest, "```")
	if !found {
		return "", errors.New("unterminated Workpad status")
	}
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(content), &fields); err != nil {
		return "", err
	}
	if fields == nil {
		return body, nil
	}
	blockers, ok := fields["blockers"].([]any)
	if !ok {
		return body, nil
	}
	remaining := make([]any, 0, len(blockers))
	for _, entry := range blockers {
		ref := ""
		switch value := entry.(type) {
		case string:
			ref = value
		case map[string]any:
			ref = questionMigrationString(value["ref"])
			if ref == "" {
				ref = questionMigrationString(value["identifier"])
			}
		}
		canonical, err := dependencyline.CanonicalReference(ref, repository)
		if err != nil || canonical != source {
			remaining = append(remaining, entry)
		}
	}
	if len(remaining) == len(blockers) {
		return body, nil
	}
	fields["blockers"] = remaining
	humanAction := questionMigrationString(fields["human_action"])
	reason := questionMigrationString(fields["reason_code"])
	if len(remaining) == 0 && strings.TrimSpace(humanAction) == "" && reason == "" && fields["status"] == "blocked" {
		fields["status"] = "in_progress"
	}
	encoded, err := yaml.Marshal(fields)
	if err != nil {
		return "", err
	}
	return before + "```detent-status\n" + string(encoded) + "```" + after, nil
}

func questionMigrationString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}
