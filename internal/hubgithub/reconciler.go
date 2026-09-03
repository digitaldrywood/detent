package hubgithub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/reviewseverity"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	if err := r.hydratePullRequestDetails(ctx, repositoryRESTPath(repository.Owner.Login, repository.Name), request.Mode, request.Hydrations, &snapshot); err != nil {
		return hubserver.ReconcileSnapshot{}, err
	}
	return snapshot, nil
}

type pullRequestDetailNeed struct {
	checks  bool
	reviews bool
}

func (r *Reconciler) hydratePullRequestDetails(ctx context.Context, repositoryPath string, mode hubserver.ReconcileMode, hydrations []hubserver.HydrationRequest, snapshot *hubserver.ReconcileSnapshot) error {
	needs := make(map[int]pullRequestDetailNeed)
	commitChecks := make(map[string]struct{})
	for _, hydration := range hydrations {
		number := hydration.GitHubNumber
		if number <= 0 && strings.TrimSpace(hydration.GitHubNodeID) != "" {
			for _, pullRequest := range snapshot.PullRequests {
				if pullRequest.NodeID == strings.TrimSpace(hydration.GitHubNodeID) {
					number = pullRequest.Number
					break
				}
			}
		}
		switch hydration.ObjectKind {
		case "pull_request_checks":
			if number > 0 {
				need := needs[number]
				need.checks = true
				needs[number] = need
			}
		case "pull_request_reviews":
			if number > 0 {
				need := needs[number]
				need.reviews = true
				needs[number] = need
			}
		case "commit_checks":
			if headSHA := strings.TrimSpace(hydration.HeadSHA); headSHA != "" {
				commitChecks[headSHA] = struct{}{}
			}
		}
	}
	if mode == hubserver.ReconcileFullRepair {
		for _, pullRequest := range snapshot.PullRequests {
			if !strings.EqualFold(strings.TrimSpace(pullRequest.State), "open") {
				continue
			}
			needs[pullRequest.Number] = pullRequestDetailNeed{checks: true, reviews: true}
		}
	}
	matchedCommitChecks := make(map[string]struct{})
	for _, pullRequest := range snapshot.PullRequests {
		if _, ok := commitChecks[strings.TrimSpace(pullRequest.HeadSHA)]; ok {
			need := needs[pullRequest.Number]
			need.checks = true
			needs[pullRequest.Number] = need
			matchedCommitChecks[strings.TrimSpace(pullRequest.HeadSHA)] = struct{}{}
		}
	}
	numbers := make([]int, 0, len(needs))
	for number := range needs {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	checksBySHA := make(map[string]tracker.CheckSummary)
	for _, number := range numbers {
		var pullRequest reconcilePullRequest
		if err := r.client.REST(ctx, http.MethodGet, repositoryPath+"/pulls/"+strconv.Itoa(number), nil, &pullRequest); err != nil {
			return fmt.Errorf("refresh github pull request %d: %w", number, err)
		}
		source := pullRequest.source()
		if err := validatePullRequestSource(source); err != nil {
			return err
		}
		replacePullRequestSource(&snapshot.PullRequests, source)
		mergeableState := strings.ToLower(strings.TrimSpace(pullRequest.MergeableState))
		details := hubserver.PullRequestDetailSource{
			Number: number, HeadSHA: source.HeadSHA, MergeableState: &mergeableState,
		}
		need := needs[number]
		if need.checks {
			checks, ok := checksBySHA[source.HeadSHA]
			if !ok {
				var err error
				checks, err = r.fetchCheckSummary(ctx, repositoryPath, source.HeadSHA)
				if err != nil {
					return err
				}
				checksBySHA[source.HeadSHA] = checks
			}
			details.Checks = &checks
		}
		if need.reviews {
			reviews, err := r.fetchReviewSummary(ctx, repositoryPath, number)
			if err != nil {
				return err
			}
			details.Reviews = &reviews
		}
		snapshot.PullRequestDetails = append(snapshot.PullRequestDetails, details)
	}
	unmatchedSHAs := make([]string, 0, len(commitChecks))
	for headSHA := range commitChecks {
		if _, matched := matchedCommitChecks[headSHA]; !matched {
			unmatchedSHAs = append(unmatchedSHAs, headSHA)
		}
	}
	sort.Strings(unmatchedSHAs)
	for _, headSHA := range unmatchedSHAs {
		checks, err := r.fetchCheckSummary(ctx, repositoryPath, headSHA)
		if err != nil {
			return err
		}
		snapshot.PullRequestDetails = append(snapshot.PullRequestDetails, hubserver.PullRequestDetailSource{
			HeadSHA: headSHA, Checks: &checks,
		})
	}
	return nil
}

func replacePullRequestSource(sources *[]hubserver.PullRequestSource, replacement hubserver.PullRequestSource) {
	for index := range *sources {
		if (*sources)[index].Number == replacement.Number {
			(*sources)[index] = replacement
			return
		}
	}
	*sources = append(*sources, replacement)
}

func (r *Reconciler) fetchCheckSummary(ctx context.Context, repositoryPath string, headSHA string) (tracker.CheckSummary, error) {
	checkRuns, err := fetchRESTCheckRuns(ctx, r.client, repositoryPath+"/commits/"+url.PathEscape(headSHA)+"/check-runs?per_page=100")
	if err != nil {
		return tracker.CheckSummary{}, fmt.Errorf("fetch github check runs: %w", err)
	}
	statuses, err := fetchRESTList[restStatus](ctx, r.client, repositoryPath+"/commits/"+url.PathEscape(headSHA)+"/statuses?per_page=100")
	if err != nil {
		return tracker.CheckSummary{}, fmt.Errorf("fetch github commit statuses: %w", err)
	}
	return summarizeChecks(checkRuns, statuses), nil
}

func (r *Reconciler) fetchReviewSummary(ctx context.Context, repositoryPath string, number int) (tracker.ReviewSummary, error) {
	reviews, err := fetchRESTList[restReview](ctx, r.client, repositoryPath+"/pulls/"+strconv.Itoa(number)+"/reviews?per_page=100")
	if err != nil {
		return tracker.ReviewSummary{}, fmt.Errorf("fetch github pull request reviews: %w", err)
	}
	return summarizeReviews(reviews), nil
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
	ID             int64     `json:"id"`
	NodeID         string    `json:"node_id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	HTMLURL        string    `json:"html_url"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	MergeableState string    `json:"mergeable_state"`
	Head           restRef   `json:"head"`
	Base           restRef   `json:"base"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type restRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

func summarizeChecks(checkRuns []restCheckRun, statuses []restStatus) tracker.CheckSummary {
	latestCheckRuns := make(map[string]restCheckRun)
	for index, checkRun := range checkRuns {
		key := strings.ToLower(strings.TrimSpace(checkRun.Name))
		if key == "" {
			key = strconv.Itoa(index)
		}
		current, ok := latestCheckRuns[key]
		if !ok || checkRun.ID > current.ID {
			latestCheckRuns[key] = checkRun
		}
	}
	latestStatuses := make(map[string]restStatus)
	for index, status := range statuses {
		key := strings.ToLower(strings.TrimSpace(status.Context))
		if key == "" {
			key = strconv.Itoa(index)
		}
		if _, ok := latestStatuses[key]; !ok {
			latestStatuses[key] = status
		}
	}
	summary := tracker.CheckSummary{Total: len(latestCheckRuns) + len(latestStatuses)}
	for _, checkRun := range latestCheckRuns {
		status := strings.ToLower(strings.TrimSpace(checkRun.Status))
		conclusion := strings.ToLower(strings.TrimSpace(checkRun.Conclusion))
		if status != "completed" || conclusion == "" {
			summary.Pending++
			continue
		}
		switch conclusion {
		case "success", "neutral", "skipped":
			summary.Passed++
		default:
			summary.Failed++
		}
	}
	for _, status := range latestStatuses {
		switch strings.ToLower(strings.TrimSpace(status.State)) {
		case "success":
			summary.Passed++
		case "failure", "error":
			summary.Failed++
		default:
			summary.Pending++
		}
	}
	if summary.Total == 0 {
		return summary
	}
	if summary.Pending > 0 {
		summary.Status = "pending"
		summary.Conclusion = "pending"
		return summary
	}
	summary.Status = "completed"
	if summary.Failed > 0 {
		summary.Conclusion = "failure"
	} else {
		summary.Conclusion = "success"
	}
	return summary
}

func summarizeReviews(reviews []restReview) tracker.ReviewSummary {
	latestByAuthor := make(map[string]restReview)
	for index, review := range reviews {
		author := strings.ToLower(strings.TrimSpace(review.User.Login))
		if author == "" {
			author = strconv.Itoa(index)
		}
		current, ok := latestByAuthor[author]
		if !ok || reviewAfter(review, current) {
			latestByAuthor[author] = review
		}
	}
	summary := tracker.ReviewSummary{}
	var latest *restReview
	blockingFinding := false
	for _, review := range latestByAuthor {
		state := strings.ToLower(strings.TrimSpace(review.State))
		if state == "dismissed" {
			continue
		}
		switch state {
		case "approved":
			summary.Approvals++
		case "changes_requested":
			summary.ChangesRequested++
		case "commented":
			summary.Comments++
		}
		if reviewseverity.Contains(review.Body, "P1") {
			blockingFinding = true
		}
		if latest == nil || reviewAfter(review, *latest) {
			value := review
			latest = &value
		}
	}
	if blockingFinding {
		summary.State = "p1"
	} else if latest != nil {
		summary.State = strings.ToLower(strings.TrimSpace(latest.State))
	}
	switch {
	case summary.ChangesRequested > 0 || blockingFinding:
		summary.Decision = "changes_requested"
	case summary.Approvals > 0:
		summary.Decision = "approved"
	case summary.Comments > 0:
		summary.Decision = "commented"
	}
	return summary
}

func reviewAfter(left restReview, right restReview) bool {
	if left.SubmittedAt == nil {
		return right.SubmittedAt == nil
	}
	return right.SubmittedAt == nil || left.SubmittedAt.After(*right.SubmittedAt)
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
