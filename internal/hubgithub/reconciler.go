package hubgithub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type Reconciler struct {
	client restClient
}

func NewReconciler(client *connectorgithub.Client) *Reconciler {
	return &Reconciler{client: client}
}

func (r *Reconciler) Reconcile(ctx context.Context, request hubserver.ReconcileRequest) (hubserver.ReconcileSnapshot, error) {
	if r == nil || r.client == nil {
		return hubserver.ReconcileSnapshot{}, errors.New("github reconciler is not configured")
	}
	repository, err := r.fetchRepository(ctx, request.Repository)
	if err != nil {
		return hubserver.ReconcileSnapshot{}, err
	}
	issuesPath := repositoryRESTPath(repository.Owner.Login, repository.Name) + "/issues"
	issueQuery := url.Values{
		"direction": {"asc"},
		"per_page":  {"100"},
		"sort":      {"updated"},
		"state":     {"all"},
	}
	if request.Mode == hubserver.ReconcileIncremental && request.Since != nil {
		issueQuery.Set("since", request.Since.UTC().Format(time.RFC3339Nano))
	}
	issuesPath += "?" + issueQuery.Encode()
	remoteIssues, err := fetchRESTList[reconcileIssue](ctx, r.client, issuesPath)
	if err != nil {
		return hubserver.ReconcileSnapshot{}, fmt.Errorf("list github repository issues: %w", err)
	}
	pullsPath := repositoryRESTPath(repository.Owner.Login, repository.Name) + "/pulls?direction=asc&per_page=100&sort=updated&state=all"
	remotePulls, err := fetchRESTList[reconcilePullRequest](ctx, r.client, pullsPath)
	if err != nil {
		return hubserver.ReconcileSnapshot{}, fmt.Errorf("list github repository pull requests: %w", err)
	}
	snapshot := hubserver.ReconcileSnapshot{
		Repository: hubserver.RepositorySource{
			NodeID: repository.NodeID, DatabaseID: positiveID(repository.ID),
			Owner: repository.Owner.Login, Name: repository.Name, UpdatedAt: repository.UpdatedAt,
		},
		Issues:       make([]hubserver.IssueSource, 0, len(remoteIssues)),
		PullRequests: make([]hubserver.PullRequestSource, 0, len(remotePulls)),
	}
	if err := validateRepositorySource(snapshot.Repository); err != nil {
		return hubserver.ReconcileSnapshot{}, err
	}
	for _, issue := range remoteIssues {
		if issue.PullRequest != nil {
			continue
		}
		source := issue.source()
		if err := validateIssueSource(source); err != nil {
			return hubserver.ReconcileSnapshot{}, err
		}
		snapshot.Issues = append(snapshot.Issues, source)
	}
	for _, pullRequest := range remotePulls {
		source := pullRequest.source()
		if err := validatePullRequestSource(source); err != nil {
			return hubserver.ReconcileSnapshot{}, err
		}
		snapshot.PullRequests = append(snapshot.PullRequests, source)
	}
	return snapshot, nil
}

func (r *Reconciler) fetchRepository(ctx context.Context, target hubserver.RepositoryTarget) (reconcileRepository, error) {
	path := repositoryRESTPath(target.Owner, target.Name)
	if target.DatabaseID != nil && *target.DatabaseID > 0 {
		path = "/repositories/" + strconv.FormatInt(*target.DatabaseID, 10)
	}
	var repository reconcileRepository
	if err := r.client.REST(ctx, http.MethodGet, path, nil, &repository); err != nil {
		return reconcileRepository{}, fmt.Errorf("refresh github repository: %w", err)
	}
	return repository, nil
}

func repositoryRESTPath(owner string, name string) string {
	return "/repos/" + url.PathEscape(strings.TrimSpace(owner)) + "/" + url.PathEscape(strings.TrimSpace(name))
}

type reconcileRepository struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	Owner     restActor `json:"owner"`
	UpdatedAt time.Time `json:"updated_at"`
}

type restActor struct {
	Login string `json:"login"`
}

type reconcileIssue struct {
	ID          int64       `json:"id"`
	NodeID      string      `json:"node_id"`
	Number      int         `json:"number"`
	Title       string      `json:"title"`
	Body        *string     `json:"body"`
	HTMLURL     string      `json:"html_url"`
	State       string      `json:"state"`
	Labels      []restLabel `json:"labels"`
	Assignees   []restActor `json:"assignees"`
	PullRequest *struct{}   `json:"pull_request"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

func (i reconcileIssue) source() hubserver.IssueSource {
	body := ""
	if i.Body != nil {
		body = *i.Body
	}
	labels := make([]string, 0, len(i.Labels))
	for _, label := range i.Labels {
		labels = append(labels, label.Name)
	}
	assignees := make([]string, 0, len(i.Assignees))
	for _, assignee := range i.Assignees {
		assignees = append(assignees, assignee.Login)
	}
	return hubserver.IssueSource{
		NodeID: i.NodeID, DatabaseID: positiveID(i.ID), Number: i.Number,
		Title: i.Title, Body: body, URL: i.HTMLURL, State: i.State,
		Labels: labels, Assignees: assignees, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
}

type reconcilePullRequest struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	HTMLURL   string    `json:"html_url"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Head      restRef   `json:"head"`
	Base      restRef   `json:"base"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type restRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

func (p reconcilePullRequest) source() hubserver.PullRequestSource {
	return hubserver.PullRequestSource{
		NodeID: p.NodeID, DatabaseID: positiveID(p.ID), Number: p.Number,
		Title: p.Title, URL: p.HTMLURL, State: p.State, Draft: p.Draft,
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func validateRepositorySource(source hubserver.RepositorySource) error {
	if strings.TrimSpace(source.NodeID) == "" || strings.TrimSpace(source.Owner) == "" || strings.TrimSpace(source.Name) == "" || source.UpdatedAt.IsZero() {
		return errors.New("github repository reconciliation returned an incomplete repository")
	}
	return nil
}

func validateIssueSource(source hubserver.IssueSource) error {
	if strings.TrimSpace(source.NodeID) == "" || source.Number <= 0 || strings.TrimSpace(source.Title) == "" || strings.TrimSpace(source.URL) == "" || strings.TrimSpace(source.State) == "" || source.CreatedAt.IsZero() || source.UpdatedAt.IsZero() {
		return fmt.Errorf("github repository reconciliation returned incomplete issue %d", source.Number)
	}
	return nil
}

func validatePullRequestSource(source hubserver.PullRequestSource) error {
	if strings.TrimSpace(source.NodeID) == "" || source.Number <= 0 || strings.TrimSpace(source.Title) == "" || strings.TrimSpace(source.URL) == "" || strings.TrimSpace(source.State) == "" || strings.TrimSpace(source.HeadRef) == "" || strings.TrimSpace(source.HeadSHA) == "" || strings.TrimSpace(source.BaseRef) == "" || strings.TrimSpace(source.BaseSHA) == "" || source.CreatedAt.IsZero() || source.UpdatedAt.IsZero() {
		return fmt.Errorf("github repository reconciliation returned incomplete pull request %d", source.Number)
	}
	return nil
}

func positiveID(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

var _ hubserver.ReconcileBackend = (*Reconciler)(nil)
