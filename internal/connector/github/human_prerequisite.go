package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/intake"
)

func (c *Connector) EnsureHumanPrerequisite(ctx context.Context, dependentIdentifier string, request connector.HumanPrerequisiteRequest) (connector.HumanPrerequisiteResult, error) {
	c.prerequisiteMu.Lock()
	defer c.prerequisiteMu.Unlock()
	var result connector.HumanPrerequisiteResult
	repository := c.repository.Owner + "/" + c.repository.Name
	dependentRef, err := dependencyline.CanonicalReference(dependentIdentifier, repository)
	if err != nil || !strings.HasPrefix(dependentRef, strings.ToLower(repository)+"#") {
		return result, errors.New("human prerequisite must belong to the dependent repository")
	}
	request.Task.Key = strings.ToLower(strings.TrimSpace(request.Task.Key))
	if err := request.Task.Validate(); err != nil {
		return result, err
	}
	if strings.TrimSpace(request.Title) == "" || request.Task.CompletionEvidence != "" {
		return result, errors.New("prerequisite requires a title and cannot assert human completion")
	}
	existing := ""
	if request.ExistingIdentifier != "" {
		existing, err = dependencyline.CanonicalReference(request.ExistingIdentifier, repository)
		if err != nil || !strings.HasPrefix(existing, strings.ToLower(repository)+"#") || existing == dependentRef {
			return result, errors.New("existing prerequisite must be a different issue in the same repository")
		}
	}
	dependent, found, err := c.fetchIssueByIdentifier(ctx, dependentRef)
	if err != nil {
		return result, err
	}
	if !found || dependent.Closed || stateInList(dependent.State, c.terminalStates) || connector.NonExecutableReason(dependent) != "" {
		return result, errors.New("dependent must be unfinished executable work")
	}
	dependent, err = c.HydratePullRequest(ctx, dependent)
	if err != nil {
		return result, err
	}
	if dependent.PullRequest != nil && strings.EqualFold(dependent.PullRequest.State, "merged") {
		return result, errors.New("completed software cannot acquire a prerequisite")
	}
	if _, err := dependencyline.References(dependent.Description, repository); err != nil {
		return result, err
	}
	issues, err := fetchRESTList[restIssue](ctx, c.client, restRepositoryIssueCreatePath(c.repository)+"?state=all&sort=created&direction=asc&per_page=100")
	if err != nil {
		return result, fmt.Errorf("list prerequisite registry: %w", err)
	}
	var matched *connector.Issue
	for _, raw := range issues {
		if raw.PullRequest != nil {
			continue
		}
		ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: raw.Number}
		node := githubIssueNodeFromREST(ref, raw)
		issue := c.buildLabelIssue(node, c.labelStatusFromLabels(node.Labels))
		task, present, parseErr := connector.ParseHumanTask(issue.Description)
		selected := existing != "" && strings.EqualFold(issue.Identifier, existing)
		keyMatches := present && strings.EqualFold(strings.TrimSpace(task.Key), request.Task.Key)
		if !selected && !keyMatches {
			continue
		}
		if matched != nil && matched.ID != issue.ID {
			return result, errors.New("multiple matching prerequisites; consolidate them before adding dependencies")
		}
		if strings.EqualFold(issue.Identifier, dependentRef) || (present && parseErr != nil) {
			return result, errors.New("invalid or self-referential prerequisite")
		}
		if present && (!strings.EqualFold(strings.TrimSpace(task.Key), request.Task.Key) || task.Action != request.Task.Action || task.Owner != request.Task.Owner || task.CompletionCriteria != request.Task.CompletionCriteria || task.ApprovalConstraint != request.Task.ApprovalConstraint) {
			return result, errors.New("prerequisite contract differs; reuse its exact milestone and approval constraint")
		}
		c.cacheIssueRef(node)
		matched = &issue
	}
	if existing != "" && matched == nil {
		return result, errors.New("existing prerequisite was not found")
	}
	block, err := yaml.Marshal(request.Task)
	if err != nil {
		return result, err
	}
	body := "```detent-human\n" + string(block) + "```\n\n```detent-agent\nschema: 1\neffort: medium\n```\n"
	if c.protectPublicationText(ctx, repository, body) != body || c.protectPublicationText(ctx, repository, request.Title) != request.Title {
		return result, errors.New("prerequisite contains protected publication text; supply a safe milestone description")
	}
	if matched == nil {
		created, err := c.CreateIntakeIssue(ctx, intake.IssueDraft{Title: request.Title, Body: body, Labels: []string{"human-owned"}})
		if err != nil {
			return result, err
		}
		result.Created = true
		matched = &connector.Issue{ID: created.ID, Identifier: created.Identifier, URL: created.URL, Description: body, Title: request.Title}
	}
	result.Issue = *matched
	if err := c.validateHumanPrerequisiteEdge(ctx, dependentRef, matched.Identifier); err != nil {
		return result, err
	}
	_, marked, err := connector.ParseHumanTask(matched.Description)
	if err != nil {
		return result, err
	}
	if !marked {
		if matched.Closed {
			return result, errors.New("closed issue has no human completion contract")
		}
		matched.Description += "\n\n" + body
		if err := c.UpdateIssueBody(ctx, matched.ID, matched.Description); err != nil {
			return result, err
		}
	}
	if !matched.Closed {
		if err := c.SetIntakeIssueState(ctx, matched.ID, "Backlog"); err != nil {
			return result, err
		}
		matched.State = "Backlog"
	}
	dependent, found, err = c.fetchIssueByIdentifier(ctx, dependentRef)
	if err != nil {
		return result, err
	}
	if !found || dependent.Closed || stateInList(dependent.State, c.terminalStates) {
		return result, errors.New("dependent completed while prerequisite was being prepared")
	}
	body, err = dependencyline.Append(dependent.Description, repository, matched.Identifier)
	if err != nil {
		return result, err
	}
	ref, _ := dependencyIssueRef(dependentRef)
	native, nativeErr := c.restNativeBlockedByRefs(ctx, ref)
	if nativeErr != nil && (!nativeDependencyUnavailableError(nativeErr) || c.dependencySource == dependencySourceNativeOnly) {
		return result, nativeErr
	}
	if nativeErr == nil && !slices.ContainsFunc(native, func(ref connector.BlockedRef) bool { return strings.EqualFold(ref.Identifier, matched.Identifier) }) {
		if err := c.AddIssueBlockedByDependency(ctx, dependentRef, matched.Identifier); err != nil {
			return result, err
		}
	}
	if body != dependent.Description {
		if err := c.UpdateIssueBody(ctx, dependent.ID, body); err != nil {
			return result, err
		}
	}
	result.Issue = *matched
	return result, nil
}

func (c *Connector) validateHumanPrerequisiteEdge(ctx context.Context, dependent, prerequisite string) error {
	repository := strings.ToLower(c.repository.Owner + "/" + c.repository.Name)
	queue := []string{prerequisite}
	seen := map[string]bool{}
	for len(queue) > 0 {
		ref, err := dependencyline.CanonicalReference(queue[0], repository)
		queue = queue[1:]
		if err != nil {
			return err
		}
		if ref == dependent {
			return errors.New("prerequisite would create a dependency cycle")
		}
		if !strings.HasPrefix(ref, repository+"#") {
			return errors.New("prerequisite graph crosses a repository boundary; review it separately")
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if len(seen) > 100 {
			return errors.New("prerequisite graph exceeds the verification limit")
		}
		issueRef, _ := dependencyIssueRef(ref)
		raw, err := c.fetchRESTIssueRaw(ctx, issueRef)
		if err != nil {
			return err
		}
		if raw.Body != nil {
			refs, err := dependencyline.References(*raw.Body, repository)
			if err != nil {
				return err
			}
			queue = append(queue, refs...)
		}
		native, err := c.restNativeBlockedByRefs(ctx, issueRef)
		if err != nil && (!nativeDependencyUnavailableError(err) || c.dependencySource == dependencySourceNativeOnly) {
			return err
		}
		for _, blocker := range native {
			queue = append(queue, blocker.Identifier)
		}
	}
	return nil
}

var _ connector.HumanPrerequisiteWriter = (*Connector)(nil)
