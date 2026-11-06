package hubgithub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Importer struct{ client restClient }

func NewImporter(client restClient) *Importer { return &Importer{client: client} }

func (i *Importer) FetchImportPage(ctx context.Context, request hubserver.GitHubImportRequest) (result hubserver.GitHubImportPage, resultErr error) {
	defer func() {
		var status *connectorgithub.StatusError
		if errors.As(resultErr, &status) {
			result.RetryAt = time.Now().Add(max(time.Minute, status.RetryAfter))
			if status.ResetAt.After(result.RetryAt) {
				result.RetryAt = status.ResetAt
			}
		}
	}()
	ctx = scopedRequests(ctx, request.Profile, "import", request.Repository)
	if i == nil || i.client == nil {
		return result, errors.New("github importer is not configured")
	}
	if request.Stage == "scheduled-range" {
		return i.fetchScheduledRange(ctx, request)
	}
	if request.Stage == "batch" {
		return i.fetchBatch(ctx, request)
	}
	if request.Stage == "scheduled" {
		return i.fetchScheduledOccurrence(ctx, request)
	}
	if request.Stage == "edits" {
		return i.fetchEdits(ctx, request)
	}
	parts := strings.Split(request.Repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || request.IssueNumber <= 0 {
		return result, errors.New("github import repository and issue number are required")
	}
	path := repositoryRESTPath(parts[0], parts[1]) + "/issues/" + strconv.Itoa(request.IssueNumber)
	if request.Stage == "issue" {
		var raw json.RawMessage
		if err := i.client.REST(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return result, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return result, err
		}
		if _, present := fields["body"]; !present {
			return result, errors.New("github issue response omits its body; an excerpt cannot complete import")
		}
		var issue reconcileIssue
		if err := json.Unmarshal(raw, &issue); err != nil {
			return result, err
		}
		if issue.PullRequest != nil {
			return result, errors.New("github import target is a pull request")
		}
		source := issue.source()
		if err := validateIssueSource(source); err != nil {
			return result, err
		}
		result.Issue = &source
		result.Gaps = []string{"GitHub exposes only retained, authorized history. Deleted comments, inaccessible private events and redacted edit diffs may be unavailable. Pagination is not an atomic source snapshot; reimport after quiescing source edits."}
		record, err := importRecord(raw, "issue")
		if err != nil {
			return result, err
		}
		result.Records = []hubserver.GitHubImportRecord{record}
		return result, nil
	}
	var kind string
	switch request.Stage {
	case "comments":
		path += "/comments"
		kind = "comment"
	case "timeline":
		path += "/timeline"
		kind = "timeline"
	case "dependencies":
		path += "/dependencies/blocked_by"
		kind = "dependency"
	default:
		return result, errors.New("unknown github import stage")
	}
	endpoint := path
	path += "?per_page=100"
	if request.Cursor != "" {
		cursor, err := url.Parse(request.Cursor)
		if err != nil || cursor.Path != endpoint || cursor.Fragment != "" || cursor.User != nil {
			return result, errors.New("github import cursor changed endpoint")
		}
		path = request.Cursor
	}
	var records []json.RawMessage
	next, err := i.client.RESTPage(ctx, path, &records)
	if err != nil {
		return result, err
	}
	result.NextCursor = next
	for _, raw := range records {
		record, err := importRecord(raw, kind)
		if err != nil {
			return result, err
		}
		result.Records = append(result.Records, record)
		if record.Kind == "comment" && record.Body == "" {
			result.Gaps = append(result.Gaps, "Comment body was unavailable: "+record.Provenance.ExternalID)
		}
	}
	return result, nil
}

func importRecord(raw json.RawMessage, kind string) (hubserver.GitHubImportRecord, error) {
	var source struct {
		ID        json.RawMessage `json:"id"`
		NodeID    string          `json:"node_id"`
		Body      string          `json:"body"`
		Event     string          `json:"event"`
		CreatedAt time.Time       `json:"created_at"`
		UpdatedAt time.Time       `json:"updated_at"`
		User      struct {
			Login  string `json:"login"`
			NodeID string `json:"node_id"`
		} `json:"user"`
		Actor struct {
			Login  string `json:"login"`
			NodeID string `json:"node_id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return hubserver.GitHubImportRecord{}, err
	}
	id := source.NodeID
	if id == "" && len(source.ID) > 0 && string(source.ID) != "null" {
		id = string(source.ID)
	}
	if id == "" {
		id = fmt.Sprintf("content-sha256:%x", sha256.Sum256(raw))
	}
	if kind == "timeline" && source.Event == "commented" {
		kind = "comment"
	}
	author, name := source.User.NodeID, source.User.Login
	if author == "" {
		author = source.User.Login
	}
	if author == "" {
		author, name = source.Actor.NodeID, source.Actor.Login
	}
	if author == "" {
		author = source.Actor.Login
	}
	if author == "" {
		author = "unavailable"
	}
	updated := source.UpdatedAt
	if updated.IsZero() {
		updated = source.CreatedAt
	}
	record := hubserver.GitHubImportRecord{SourceKey: kind + ":" + id, Kind: kind, Data: raw, Provenance: tracker.Provenance{Provider: "github", ExternalID: id, AuthorID: author, AuthorDisplayName: name, CreatedAt: source.CreatedAt, UpdatedAt: updated, ObservedAt: time.Now().UTC()}}
	if kind == "comment" {
		record.Body = source.Body
	}
	if kind == "dependency" {
		record.DependencyID = source.NodeID
	}
	return record, nil
}

func (i *Importer) fetchScheduledOccurrence(ctx context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	if request.Scheduled == nil {
		return hubserver.GitHubImportPage{}, errors.New("scheduled occurrence is required")
	}
	evidence := *request.Scheduled
	parsed := gate.ParseScheduledEvidence(evidence.Stamp())
	if len(parsed) != 1 || evidence.Repository != request.Repository || evidence.JobID == "0" {
		return hubserver.GitHubImportPage{}, errors.New("scheduled occurrence is invalid")
	}
	path := "/repos/" + request.Repository + "/actions/runs/" + evidence.RunID + "/attempts/" + evidence.RunAttempt
	var run struct {
		ID         int64  `json:"id"`
		Attempt    int64  `json:"run_attempt"`
		Path       string `json:"path"`
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		Repository struct {
			Name string `json:"full_name"`
		} `json:"repository"`
		HeadRepository struct {
			Name string `json:"full_name"`
		} `json:"head_repository"`
	}
	if err := i.client.REST(ctx, http.MethodGet, path, nil, &run); err != nil {
		return hubserver.GitHubImportPage{}, err
	}
	if strconv.FormatInt(run.ID, 10) != evidence.RunID || strconv.FormatInt(run.Attempt, 10) != evidence.RunAttempt || !matchesScheduledCIWorkflow(request, run.Path, run.HeadBranch) || run.HeadSHA != evidence.HeadSHA || !strings.EqualFold(run.Repository.Name, request.Repository) || !strings.EqualFold(run.HeadRepository.Name, request.Repository) || (run.Event != "schedule" && run.Event != "push" && run.Event != "workflow_dispatch") {
		return hubserver.GitHubImportPage{}, errors.New("scheduled occurrence does not match the repository CI run attempt")
	}
	var jobs struct {
		Jobs []struct {
			ID          int64     `json:"id"`
			RunID       int64     `json:"run_id"`
			Name        string    `json:"name"`
			HeadSHA     string    `json:"head_sha"`
			Status      string    `json:"status"`
			Conclusion  string    `json:"conclusion"`
			StartedAt   time.Time `json:"started_at"`
			CompletedAt time.Time `json:"completed_at"`
		} `json:"jobs"`
	}
	if err := i.client.REST(ctx, http.MethodGet, path+"/jobs?per_page=100", nil, &jobs); err != nil {
		return hubserver.GitHubImportPage{}, err
	}
	for _, job := range jobs.Jobs {
		if strconv.FormatInt(job.ID, 10) == evidence.JobID && job.RunID == run.ID && job.HeadSHA == evidence.HeadSHA && job.Name == evidence.JobName && job.Status == "completed" && job.Conclusion == evidence.Conclusion && (job.Conclusion == "failure" || job.Conclusion == "timed_out") {
			evidence.Checks = nil
			evidence.StartedAt, evidence.FinishedAt = job.StartedAt, job.CompletedAt
			evidence.JobURL = "https://github.com/" + request.Repository + "/actions/jobs/" + evidence.JobID
			return hubserver.GitHubImportPage{Scheduled: &evidence}, nil
		}
	}
	return hubserver.GitHubImportPage{}, errors.New("scheduled occurrence does not match a failed job in the run attempt")
}

func matchesScheduledCIWorkflow(request hubserver.GitHubImportRequest, workflow, branch string) bool {
	expectedWorkflow := request.ScheduledWorkflow
	if expectedWorkflow == "" {
		expectedWorkflow = ".github/workflows/ci.yml"
	}
	if workflow != expectedWorkflow {
		return false
	}
	if request.ScheduledBranch == "release/v*" {
		matched, err := path.Match(request.ScheduledBranch, branch)
		return err == nil && matched && branch != "release/v"
	}
	if request.ScheduledBranch != "" {
		return branch == request.ScheduledBranch
	}
	return expectedWorkflow == ".github/workflows/ci.yml" && (branch == "develop" || strings.HasPrefix(branch, "release/v"))
}

func (i *Importer) fetchBatch(ctx context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	client, ok := i.client.(graphQLClient)
	if !ok || request.Discovery == nil {
		return hubserver.GitHubImportPage{}, errors.New("github GraphQL batch transport is unavailable")
	}
	page, err := connectorgithub.FetchDiscoveryPage(ctx, client, *request.Discovery)
	if err != nil {
		return hubserver.GitHubImportPage{}, err
	}
	for index := range page.Issues {
		preview := &page.Issues[index]
		if !preview.NeedsDiscussion {
			continue
		}
		preview.Snapshot.Comments = nil
		comments := hubserver.GitHubImportRequest{Profile: request.Profile, Repository: request.Repository, IssueNumber: preview.Number, Stage: "comments"}
		seen := map[string]bool{}
		commentCount := 0
		for pages := 0; ; pages++ {
			if pages == 500 {
				return hubserver.GitHubImportPage{}, errors.New("github discussion exceeds intake page bound")
			}
			result, err := i.FetchImportPage(ctx, comments)
			if err != nil {
				return result, err
			}
			for _, record := range result.Records {
				if record.Kind == "comment" {
					commentCount++
				}
				if record.Kind == "comment" && !strings.Contains(record.Body, "<!-- detent-summary:") {
					preview.Snapshot.Comments = append(preview.Snapshot.Comments, tracker.GitHubIssueComment{Body: record.Body, Provenance: record.Provenance})
				}
			}
			if result.NextCursor == "" {
				break
			}
			if seen[result.NextCursor] {
				return hubserver.GitHubImportPage{}, errors.New("github discussion pagination did not advance")
			}
			seen[result.NextCursor] = true
			comments.Cursor = result.NextCursor
		}
		if commentCount != preview.DiscussionCount {
			return hubserver.GitHubImportPage{}, errors.New("github discussion changed or is incomplete; retry intake")
		}
		preview.NeedsDiscussion = false
	}
	return hubserver.GitHubImportPage{BatchPage: &page}, nil
}

func (i *Importer) fetchScheduledRange(ctx context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	var result struct {
		Status  string `json:"status"`
		Total   int    `json:"total_commits"`
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	endpoint := "/repos/" + request.Repository + "/compare/" + url.PathEscape(request.CompareBase) + "..." + url.PathEscape(request.CompareHead) + "?per_page=100"
	if err := i.client.REST(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		return hubserver.GitHubImportPage{}, err
	}
	if result.Status != "ahead" || result.Total == 0 || result.Total > 100 || result.Total != len(result.Commits) {
		return hubserver.GitHubImportPage{}, nil
	}
	page := hubserver.GitHubImportPage{}
	for _, commit := range result.Commits {
		page.Commits = append(page.Commits, commit.SHA)
	}
	return page, nil
}
