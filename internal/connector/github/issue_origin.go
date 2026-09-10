package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

func (c *Connector) createMachineIssue(ctx context.Context, draft connector.IssueDraft, fingerprint string) (connector.Issue, error) {
	repository := strings.ToLower(c.repository.Owner + "/" + c.repository.Name)
	config := (scheduleowner.Config{Enabled: true, Key: repository}).Normalized(repository, "")
	store := c.machineIssueStore
	if store == nil {
		var err error
		store, err = coordination.NewGitHubRefStore(coordination.GitHubRefConfig{
			Repository: repository, Branch: config.Branch, Client: c.client,
		})
		if err != nil {
			return connector.Issue{}, err
		}
	}
	coordinator, err := scheduleowner.NewIssueCoordinator(config, store, scheduleowner.Dependencies{})
	if err != nil {
		return connector.Issue{}, err
	}
	backend := &fingerprintIssueBackend{connector: c, fingerprint: fingerprint}
	_, created, err := coordinator.EnsureRecurring(ctx, "machine:"+fingerprint, intake.IssueDraft{
		Title: draft.Title, Body: draft.Body, Labels: draft.Labels,
	}, backend)
	if err != nil {
		return connector.Issue{}, err
	}
	backend.issue.PublicationReused = !created
	return backend.issue, nil
}

type fingerprintIssueBackend struct {
	connector   *Connector
	fingerprint string
	issue       connector.Issue
}

func (b *fingerprintIssueBackend) FindIntakeIssue(ctx context.Context, _ string) (intake.Issue, bool, error) {
	issue, found, err := b.connector.findOpenFingerprint(ctx, b.fingerprint)
	if found {
		b.issue = issue
	}
	return intake.Issue{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL, Body: issue.Description}, found, err
}

func (b *fingerprintIssueBackend) CreateIntakeIssue(ctx context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	issue, err := b.connector.createIssue(ctx, connector.IssueDraft{Title: draft.Title, Body: draft.Body, Labels: draft.Labels}, draft.Title, draft.Body)
	b.issue = issue
	return intake.Issue{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL, Body: issue.Description}, err
}

func (b *fingerprintIssueBackend) IntakeIssueClosed(ctx context.Context, id string) (bool, error) {
	issue, found, err := b.FindIntakeIssue(ctx, b.fingerprint)
	if err != nil {
		return false, err
	}
	if found && issue.ID == id {
		return false, nil
	}
	ref, ok, err := b.connector.issueRefForID(ctx, id, graphQLQueryIssueLookup)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrStatusUpdateFailed
	}
	node, err := b.connector.fetchRESTIssue(ctx, ref)
	if err != nil {
		return false, err
	}
	b.issue = b.connector.buildLabelIssue(node, b.connector.labelStatusFromLabels(node.Labels))
	return strings.EqualFold(node.State, "closed"), nil
}

func (b *fingerprintIssueBackend) CreateComment(ctx context.Context, id, body string) error {
	return b.connector.CreateComment(ctx, id, body)
}

func (c *Connector) findOpenFingerprint(ctx context.Context, fingerprint string) (connector.Issue, bool, error) {
	for page := 1; ; page++ {
		var issues []restIssue
		path := fmt.Sprintf("/repos/%s/%s/issues?state=open&per_page=100&page=%d", c.repository.Owner, c.repository.Name, page)
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &issues); err != nil {
			return connector.Issue{}, false, fmt.Errorf("find open issue fingerprint: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest != nil || issue.Body == nil || issue.State == "closed" {
				continue
			}
			origin, ok := issueorigin.Parse(*issue.Body)
			if !ok || origin.Fingerprint != fingerprint {
				continue
			}
			ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: issue.Number}
			node := githubIssueNodeFromREST(ref, issue)
			c.cacheIssueRef(node)
			return c.buildLabelIssue(node, c.labelStatusFromLabels(node.Labels)), true, nil
		}
		if len(issues) < 100 {
			return connector.Issue{}, false, nil
		}
	}
}
