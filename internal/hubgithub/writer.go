package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

const workpadHeading = "## Codex Workpad"

type restClient interface {
	REST(context.Context, string, string, any, any) error
	RESTPage(context.Context, string, any) (string, error)
}

type Writer struct {
	client restClient
}

func NewWriter(client *connectorgithub.Client) *Writer {
	return &Writer{client: client}
}

func (w *Writer) Execute(ctx context.Context, item hubserver.OutboxItem) error {
	if w == nil || w.client == nil {
		return hubserver.Permanent(errors.New("github outbox writer is not configured"))
	}
	var err error
	switch item.Kind {
	case hubserver.MutationWorkflowLabel:
		err = w.updateWorkflowLabel(ctx, item)
	case hubserver.MutationWorkpad:
		err = w.upsertWorkpad(ctx, item)
	case hubserver.MutationMergePullRequest:
		return hubserver.Permanent(errors.New("irreversible github mutation requires fresh verification"))
	default:
		return hubserver.Permanent(fmt.Errorf("unsupported github mutation kind %q", item.Kind))
	}
	return classifyError(err)
}

func (w *Writer) VerifyAndExecute(ctx context.Context, item hubserver.OutboxItem) error {
	if w == nil || w.client == nil {
		return hubserver.Permanent(errors.New("github outbox writer is not configured"))
	}
	if item.Kind != hubserver.MutationMergePullRequest {
		return hubserver.Permanent(fmt.Errorf("github mutation %q is not irreversible", item.Kind))
	}
	return classifyError(w.verifyAndMerge(ctx, item))
}

func (w *Writer) updateWorkflowLabel(ctx context.Context, item hubserver.OutboxItem) error {
	if err := validateIssueTarget(item); err != nil {
		return err
	}
	var desired hubserver.WorkflowLabelDesired
	if err := decodeDesired(item, &desired); err != nil {
		return err
	}
	desired.Label = strings.TrimSpace(desired.Label)
	desired.ManagedPrefix = strings.TrimSpace(desired.ManagedPrefix)
	if desired.Label == "" || desired.ManagedPrefix == "" {
		return hubserver.Permanent(errors.New("workflow label mutation is incomplete"))
	}

	var issue restIssue
	path := issuePath(item)
	if err := w.client.REST(ctx, http.MethodGet, path, nil, &issue); err != nil {
		return fmt.Errorf("refresh github issue labels: %w", err)
	}
	labels := make([]string, 0, len(issue.Labels)+1)
	for _, label := range issue.Labels {
		name := strings.TrimSpace(label.Name)
		if name == "" || strings.HasPrefix(strings.ToLower(name), strings.ToLower(desired.ManagedPrefix)) {
			continue
		}
		labels = append(labels, name)
	}
	labels = append(labels, desired.Label)
	labels = normalizedLabels(labels)
	if err := w.client.REST(ctx, http.MethodPut, path+"/labels", map[string]any{"labels": labels}, nil); err != nil {
		return fmt.Errorf("replace github workflow label: %w", err)
	}
	return nil
}

func (w *Writer) upsertWorkpad(ctx context.Context, item hubserver.OutboxItem) error {
	if err := validateIssueTarget(item); err != nil {
		return err
	}
	var desired hubserver.WorkpadDesired
	if err := decodeDesired(item, &desired); err != nil {
		return err
	}
	body := strings.TrimSpace(desired.Body)
	marker := strings.TrimSpace(desired.Marker)
	if body == "" || marker == "" {
		return hubserver.Permanent(errors.New("workpad mutation is incomplete"))
	}
	if !strings.Contains(body, marker) {
		body += "\n\n" + marker
	}

	commentsPath := issuePath(item) + "/comments?per_page=100"
	comments, err := fetchRESTList[restComment](ctx, w.client, commentsPath)
	if err != nil {
		return fmt.Errorf("list github issue comments for workpad: %w", err)
	}
	commentID := workpadCommentID(comments, marker)
	if commentID == 0 {
		var created restComment
		if err := w.client.REST(ctx, http.MethodPost, issuePath(item)+"/comments", map[string]any{"body": body}, &created); err != nil {
			return fmt.Errorf("create github workpad comment: %w", err)
		}
		if created.ID == 0 {
			return errors.New("create github workpad comment returned no id")
		}
		return nil
	}

	commentPath := repositoryPath(item) + "/issues/comments/" + strconv.FormatInt(commentID, 10)
	if err := w.client.REST(ctx, http.MethodPatch, commentPath, map[string]any{"body": body}, nil); err != nil {
		return fmt.Errorf("update github workpad comment: %w", err)
	}
	return nil
}

func (w *Writer) verifyAndMerge(ctx context.Context, item hubserver.OutboxItem) error {
	if err := validateRepository(item); err != nil {
		return err
	}
	var desired hubserver.MergePullRequestDesired
	if err := decodeDesired(item, &desired); err != nil {
		return err
	}
	if desired.PullRequestNumber <= 0 || strings.TrimSpace(desired.HeadSHA) == "" {
		return hubserver.Permanent(errors.New("merge pull request mutation is incomplete"))
	}

	pullPath := repositoryPath(item) + "/pulls/" + strconv.Itoa(desired.PullRequestNumber)
	var pullRequest restPullRequest
	if err := w.client.REST(ctx, http.MethodGet, pullPath, nil, &pullRequest); err != nil {
		return fmt.Errorf("refresh github pull request before merge: %w", err)
	}
	if strings.TrimSpace(pullRequest.Head.SHA) != strings.TrimSpace(desired.HeadSHA) {
		return hubserver.Permanent(errors.New("fresh github pull request head does not match the requested head"))
	}
	if !strings.EqualFold(strings.TrimSpace(pullRequest.State), "open") {
		if pullRequest.Merged {
			return nil
		}
		return hubserver.Permanent(errors.New("fresh github pull request is not open"))
	}
	if pullRequest.Draft {
		return hubserver.Permanent(errors.New("fresh github pull request is a draft"))
	}
	if pullRequest.Mergeable == nil {
		return errors.New("fresh github pull request mergeability is not yet available")
	}
	if !*pullRequest.Mergeable {
		return hubserver.Permanent(errors.New("fresh github pull request has merge conflicts"))
	}
	if !strings.EqualFold(strings.TrimSpace(pullRequest.MergeableState), "clean") {
		return errors.New("fresh github pull request is not cleanly mergeable")
	}

	reviews, err := fetchRESTList[restReview](ctx, w.client, pullPath+"/reviews?per_page=100")
	if err != nil {
		return fmt.Errorf("refresh github pull request reviews before merge: %w", err)
	}
	if reviewer := changesRequestingReviewer(reviews); reviewer != "" {
		return fmt.Errorf("fresh github review by %s requests changes", reviewer)
	}

	checksPath := repositoryPath(item) + "/commits/" + url.PathEscape(strings.TrimSpace(desired.HeadSHA)) + "/check-runs?per_page=100"
	checks, err := fetchRESTCheckRuns(ctx, w.client, checksPath)
	if err != nil {
		return fmt.Errorf("refresh github check runs before merge: %w", err)
	}
	if check := unreadyCheck(checks); check != "" {
		return fmt.Errorf("fresh github check %s is not successful", check)
	}

	statusesPath := repositoryPath(item) + "/commits/" + url.PathEscape(strings.TrimSpace(desired.HeadSHA)) + "/statuses?per_page=100"
	statuses, err := fetchRESTList[restStatus](ctx, w.client, statusesPath)
	if err != nil {
		return fmt.Errorf("refresh github commit statuses before merge: %w", err)
	}
	if status := unreadyStatus(statuses); status != "" {
		return fmt.Errorf("fresh github status %s is not successful", status)
	}

	method := strings.ToLower(strings.TrimSpace(desired.MergeMethod))
	switch method {
	case "merge", "rebase", "squash":
	default:
		return hubserver.Permanent(errors.New("merge pull request mutation has an invalid merge method"))
	}
	var response restMergeResponse
	if err := w.client.REST(ctx, http.MethodPut, pullPath+"/merge", map[string]string{
		"merge_method": method,
		"sha":          strings.TrimSpace(desired.HeadSHA),
	}, &response); err != nil {
		return fmt.Errorf("merge freshly verified github pull request: %w", err)
	}
	if !response.Merged {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "github did not merge the freshly verified pull request"
		}
		return errors.New(message)
	}
	return nil
}

func decodeDesired(item hubserver.OutboxItem, target any) error {
	if err := json.Unmarshal(item.Desired, target); err != nil {
		return hubserver.Permanent(fmt.Errorf("decode github %s desired state: %w", item.Kind, err))
	}
	return nil
}

func validateRepository(item hubserver.OutboxItem) error {
	if strings.TrimSpace(item.RepositoryOwner) == "" || strings.TrimSpace(item.RepositoryName) == "" {
		return hubserver.Permanent(errors.New("github mutation repository is incomplete"))
	}
	return nil
}

func validateIssueTarget(item hubserver.OutboxItem) error {
	if err := validateRepository(item); err != nil {
		return err
	}
	if item.IssueNumber <= 0 {
		return hubserver.Permanent(errors.New("github mutation issue number must be positive"))
	}
	return nil
}

func repositoryPath(item hubserver.OutboxItem) string {
	return "/repos/" + url.PathEscape(strings.TrimSpace(item.RepositoryOwner)) + "/" + url.PathEscape(strings.TrimSpace(item.RepositoryName))
}

func issuePath(item hubserver.OutboxItem) string {
	return repositoryPath(item) + "/issues/" + strconv.Itoa(item.IssueNumber)
}

func fetchRESTList[T any](ctx context.Context, client restClient, path string) ([]T, error) {
	result := make([]T, 0)
	for path != "" {
		var page []T
		next, err := client.RESTPage(ctx, path, &page)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		path = next
	}
	return result, nil
}

func fetchRESTCheckRuns(ctx context.Context, client restClient, path string) ([]restCheckRun, error) {
	result := make([]restCheckRun, 0)
	for path != "" {
		var page restCheckRuns
		next, err := client.RESTPage(ctx, path, &page)
		if err != nil {
			return nil, err
		}
		result = append(result, page.CheckRuns...)
		path = next
	}
	return result, nil
}

func normalizedLabels(labels []string) []string {
	seen := make(map[string]string, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		seen[strings.ToLower(label)] = label
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, seen[key])
	}
	return result
}

func workpadCommentID(comments []restComment, marker string) int64 {
	for _, comment := range comments {
		if comment.ID > 0 && strings.Contains(comment.Body, marker) {
			return comment.ID
		}
	}
	for _, comment := range comments {
		if comment.ID > 0 && strings.HasPrefix(strings.TrimSpace(comment.Body), workpadHeading) {
			return comment.ID
		}
	}
	return 0
}

func changesRequestingReviewer(reviews []restReview) string {
	latest := make(map[string]string)
	for _, review := range reviews {
		login := strings.ToLower(strings.TrimSpace(review.User.Login))
		if login == "" {
			continue
		}
		state := strings.ToUpper(strings.TrimSpace(review.State))
		if state == "COMMENTED" || state == "PENDING" {
			continue
		}
		latest[login] = state
	}
	requesting := make([]string, 0)
	for login, state := range latest {
		if state == "CHANGES_REQUESTED" {
			requesting = append(requesting, login)
		}
	}
	sort.Strings(requesting)
	if len(requesting) == 0 {
		return ""
	}
	return requesting[0]
}

func unreadyCheck(checks []restCheckRun) string {
	latest := make(map[string]restCheckRun)
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		prior, exists := latest[strings.ToLower(name)]
		if !exists || check.ID > prior.ID {
			latest[strings.ToLower(name)] = check
		}
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		check := latest[key]
		if !strings.EqualFold(strings.TrimSpace(check.Status), "completed") {
			return check.Name
		}
		switch strings.ToLower(strings.TrimSpace(check.Conclusion)) {
		case "success", "neutral", "skipped":
		default:
			return check.Name
		}
	}
	return ""
}

func unreadyStatus(statuses []restStatus) string {
	seen := make(map[string]struct{})
	for _, status := range statuses {
		name := strings.TrimSpace(status.Context)
		key := strings.ToLower(name)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if !strings.EqualFold(strings.TrimSpace(status.State), "success") {
			return name
		}
	}
	return ""
}

func classifyError(err error) error {
	if err == nil || hubserver.IsPermanent(err) {
		return err
	}
	if errors.Is(err, connectorgithub.ErrMissingToken) ||
		errors.Is(err, connectorgithub.ErrAuthenticationFailed) ||
		errors.Is(err, connectorgithub.ErrNotFound) {
		return hubserver.Permanent(err)
	}
	if errors.Is(err, connectorgithub.ErrRateLimited) ||
		errors.Is(err, connectorgithub.ErrRESTBudgetReserved) ||
		errors.Is(err, connectorgithub.ErrTransient) {
		return err
	}
	var statusErr *connectorgithub.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
			http.StatusMethodNotAllowed, http.StatusGone, http.StatusUnprocessableEntity:
			return hubserver.Permanent(err)
		}
	}
	return err
}

type restIssue struct {
	Labels []restLabel `json:"labels"`
}

type restLabel struct {
	Name string `json:"name"`
}

type restComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

type restPullRequest struct {
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type restReview struct {
	State string `json:"state"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

type restCheckRuns struct {
	CheckRuns []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type restStatus struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

type restMergeResponse struct {
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

var _ hubserver.OutboxBackend = (*Writer)(nil)
